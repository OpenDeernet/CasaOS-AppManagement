package service

import (
	"context"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// A backup that ran is a backup that is written down, whatever happened to it.
//
// The scheduler recorded its runs and the on-demand route did not, so the
// History tab never showed a backup somebody had asked for with the button --
// only the ones that went off by themselves. The first install that exercised
// the feature end to end waited two minutes for a record that was never going
// to be written. Both paths finish their record here now.

// BackupOnDemand runs one backup somebody asked for, and records it.
func BackupOnDemand(ctx context.Context, app *ComposeApp, docker backupDocker, copier BackupCopier, opts BackupOptions) (BackupManifest, error) {
	record := BackupRunRecord{
		App: app.Name, Destination: opts.Destination, Stamp: opts.Stamp,
		StartedAt: time.Now(), Scheduled: false, ContainersStopped: opts.HoldStill,
	}

	manifest, err := RunBackup(ctx, app, docker, copier, opts)
	finishBackupRecord(&record, manifest, err)

	return manifest, err
}

// finishBackupRecord fills in how a run ended and writes it down.
//
// Recorded before the error is looked at by the caller: a failure that is not
// written down is a failure nobody finds out about until they need the backup.
func finishBackupRecord(record *BackupRunRecord, manifest BackupManifest, err error) {
	finishBackupRecordWith(record, manifest, err, RecordBackupRun)
}

// finishBackupRecordWith is finishBackupRecord writing through write, so a test can
// read what would be written. The record says whether the app was stopped as the
// run did it, not as it was asked: an app that answers DNS is copied running.
func finishBackupRecordWith(record *BackupRunRecord, manifest BackupManifest, err error, write func(BackupRunRecord) error) {
	record.FinishedAt = time.Now()
	if !record.Restore {
		record.ContainersStopped = manifest.ContainersStopped
	}
	record.Copied = len(manifest.Operations)
	record.SkippedCount = len(manifest.Skipped)
	record.Bytes = manifest.Bytes
	if err != nil {
		record.Error = err.Error()
	}

	if logErr := write(*record); logErr != nil {
		logger.Error("a backup ran and could not be written down", zap.Error(logErr), zap.String("app", record.App))
	}
}
