package backup

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path"

	"github.com/AlexAkulov/clickhouse-backup/config"
	"github.com/AlexAkulov/clickhouse-backup/pkg/clickhouse"
	"github.com/AlexAkulov/clickhouse-backup/pkg/metadata"
	"github.com/AlexAkulov/clickhouse-backup/pkg/new_storage"
	legacyStorage "github.com/AlexAkulov/clickhouse-backup/pkg/storage"

	apexLog "github.com/apex/log"
)

func legacyDownload(cfg *config.Config, defaultDataPath, backupName string) error {
	log := apexLog.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "download",
	})
	bd, err := legacyStorage.NewBackupDestination(cfg)
	if err != nil {
		return err
	}
	if err := bd.Connect(); err != nil {
		return err
	}
	if err := bd.CompressedStreamDownload(backupName,
		path.Join(defaultDataPath, "backup", backupName)); err != nil {
		return err
	}
	log.Info("done")
	return nil
}

func Download(cfg *config.Config, backupName string, tablePattern string, schemaOnly bool) error {
	log := apexLog.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "download",
	})
	if cfg.General.RemoteStorage == "none" {
		return fmt.Errorf("Remote storage is 'none'")
	}
	if backupName == "" {
		PrintRemoteBackups(cfg, "all")
		return fmt.Errorf("select backup for download")
	}
	localBackups, err := GetLocalBackups(cfg)
	if err != nil {
		return err
	}
	for i := range localBackups {
		if backupName == localBackups[i].BackupName {
			return fmt.Errorf("'%s' already exists", backupName)
		}
	}
	ch := &clickhouse.ClickHouse{
		Config: &cfg.ClickHouse,
	}
	if err := ch.Connect(); err != nil {
		return fmt.Errorf("can't connect to clickhouse: %v", err)
	}
	defer ch.Close()
	bd, err := new_storage.NewBackupDestination(cfg)
	if err != nil {
		return err
	}
	if err := bd.Connect(); err != nil {
		return err
	}
	remoteBackups, err := bd.BackupList()
	if err != nil {
		return err
	}
	found := false
	var remoteBackup new_storage.Backup
	for _, b := range remoteBackups {
		if backupName == b.BackupName {
			remoteBackup = b
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("'%s' is not found on remote storage", backupName)
	}
	defaultDataPath, err := ch.GetDefaultPath()
	if err != nil {
		return err
	}
	if remoteBackup.Legacy {
		if tablePattern != "" {
			return fmt.Errorf("'%s' is old format backup and doesn't supports download of specific tables", backupName)
		}
		if schemaOnly {
			return fmt.Errorf("'%s' is old format backup and doesn't supports download of schema only", backupName)
		}
		log.Debugf("'%s' is old-format backup", backupName)
		return legacyDownload(cfg, defaultDataPath, backupName)
	}
	disks, err := ch.GetDisks()
	if err != nil {
		return err
	}
	diskMap := map[string]string{}
	for _, disk := range disks {
		diskMap[disk.Name] = disk.Path
	}
	tableMetadataForDownload := []metadata.TableMetadata{}
	tablesForDownload := parseTablePatternForDownload(remoteBackup.Tables, tablePattern)
	dataSize := int64(0)
	metadataSize := int64(0)

	for _, t := range tablesForDownload {
		log := log.WithField("table", fmt.Sprintf("%s.%s", t.Database, t.Table))
		remoteTableMetadata := path.Join(backupName, "metadata", clickhouse.TablePathEncode(t.Database), fmt.Sprintf("%s.json", clickhouse.TablePathEncode(t.Table)))
		log.Debug(remoteTableMetadata)
		tmReader, err := bd.GetFileReader(remoteTableMetadata)
		if err != nil {
			return err
		}
		tmBody, err := ioutil.ReadAll(tmReader)
		if err != nil {
			return err
		}
		tmReader.Close()
		var tableMetadata metadata.TableMetadata
		if err := json.Unmarshal(tmBody, &tableMetadata); err != nil {
			return err
		}
		tableMetadataForDownload = append(tableMetadataForDownload, tableMetadata)

		// save metadata
		metadataLocalFile := path.Join(defaultDataPath, "backup", backupName, "metadata", clickhouse.TablePathEncode(t.Database), fmt.Sprintf("%s.json", clickhouse.TablePathEncode(t.Table)))
		size, err := tableMetadata.Save(metadataLocalFile, schemaOnly)
		if err != nil {
			return err
		}
		metadataSize += int64(size)
		log.Info("done")
	}

	if !schemaOnly {
		for _, t := range tableMetadataForDownload {
			for disk := range t.Parts {
				if _, ok := diskMap[disk]; !ok {
					return fmt.Errorf("table '%s.%s' require disk '%s' that not found in clickhouse, you can add nonexistent disks to disk_mapping config", t.Database, t.Table, disk)
				}
			}
		}
		for _, tableMetadata := range tableMetadataForDownload {
			if tableMetadata.MetadataOnly {
				continue
			}
			dataSize += tableMetadata.TotalBytes
			// download data
			uuid := path.Join(clickhouse.TablePathEncode(tableMetadata.Database), clickhouse.TablePathEncode(tableMetadata.Table))
			log := log.WithField("table", fmt.Sprintf("%s.%s", tableMetadata.Database, tableMetadata.Table))
			if remoteBackup.DataFormat != "directory" {
				for disk := range tableMetadata.Files {
					diskPath := diskMap[disk]
					tableLocalDir := path.Join(diskPath, "backup", backupName, "shadow", uuid, disk)
					for _, archiveFile := range tableMetadata.Files[disk] {
						tableRemoteFile := path.Join(backupName, "shadow", clickhouse.TablePathEncode(tableMetadata.Database), clickhouse.TablePathEncode(tableMetadata.Table), archiveFile)
						if err := bd.CompressedStreamDownload(tableRemoteFile, tableLocalDir); err != nil {
							return err
						}
					}
				}
				continue
			}
			for disk := range tableMetadata.Parts {
				tableRemotePath := path.Join(backupName, "shadow", uuid, disk)
				diskPath := diskMap[disk]
				tableLocalDir := path.Join(diskPath, "backup", backupName, "shadow", uuid, disk)
				if err := bd.DownloadPath(0, tableRemotePath, tableLocalDir); err != nil {
					return err
				}
			}
			log.Info("done")
		}
	}
	backupMetadata := remoteBackup.BackupMetadata
	backupMetadata.Tables = tablesForDownload
	backupMetadata.DataSize = dataSize
	backupMetadata.MetadataSize = metadataSize
	backupMetadata.DataFormat = ""
	tbBody, err := json.MarshalIndent(&backupMetadata, "", "\t")
	if err != nil {
		return err
	}
	backupMetafileLocalPath := path.Join(defaultDataPath, "backup", backupName, "metadata.json")
	if err := ioutil.WriteFile(backupMetafileLocalPath, tbBody, 0640); err != nil {
		return err
	}

	log.Info("done")
	return nil
}
