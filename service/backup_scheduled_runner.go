package service

import (
	"context"
	"fmt"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/restic"
)

// The bridge between a schedule and the pieces that do the work.
//
// Small on purpose: the scheduler above takes an interface so its decisions can
// be tested without a daemon, and this is the only place those decisions meet a
// real one.

type scheduledBackupRunner struct{}

// NewScheduledBackupRunner is what the scheduler drives in production.
func NewScheduledBackupRunner() *scheduledBackupRunner { //nolint:revive // deliberately unexported type behind a constructor
	return &scheduledBackupRunner{}
}

func (r *scheduledBackupRunner) Run(ctx context.Context, schedule BackupSchedule, stamp string) (BackupManifest, error) {
	// Looked up now rather than remembered in the schedule: an app can be
	// uninstalled between the moment a schedule is saved and tonight, and a
	// schedule pointing at nothing should say so rather than fail obscurely.
	composeApps, err := MyService.Compose().List(ctx)
	if err != nil {
		return BackupManifest{}, err
	}

	composeApp, ok := composeApps[schedule.App]
	if !ok {
		return BackupManifest{}, fmt.Errorf("`%s` is scheduled for backup but is not installed any more", schedule.App)
	}

	return RunBackup(ctx, composeApp, MyService.Docker(), restic.NewRouter(), BackupOptions{
		Destination: schedule.Destination,
		Stamp:       stamp,
		HoldStill:   schedule.HoldStill,
	})
}

func (r *scheduledBackupRunner) Runs(destination, app string) ([]string, error) {
	return restic.NewRouter().Runs(destination, app)
}

func (r *scheduledBackupRunner) Purge(ctx context.Context, destination, remotePath string) error {
	return restic.NewRouter().Purge(ctx, destination, remotePath)
}
