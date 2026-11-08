package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// Backing up the box itself.
//
// Apps can be put back on a machine that has nothing. The machine's own state
// could not: its users, its shares and their accounts, its schedules, the
// destinations it sends backups to. This is that state, treated as one more
// thing a destination can hold, under a name no app is likely to take.

// SystemBackupName is the app name the box's own backups are filed under.
const SystemBackupName = "casaos-system"

// systemPaths is what the box is, apart from its apps. Each is an entry the
// plan treats like a bind mount: a folder or a file on this host, put back at
// the same place. Absent ones are skipped with the reason, not planned.
var systemPaths = []string{
	"/etc/casaos",
	"/etc/samba/smb.casa.conf",
	"/var/lib/casaos/db",
	"/var/lib/casaos/conf",
	"/root/.config/rclone/rclone.conf",
}

// systemUnits are the services that hold the files above open. Stopped for the
// copy in both directions -- sqlite files copied under a running writer are the
// same bad backup a database in a container would be -- and started again
// after. Not app-management, which is doing the copying, nor the gateway or
// the bus, which hold none of them.
var systemUnits = []string{"casaos", "casaos-user-service", "casaos-local-storage"}

// rcloneUnit holds the one file a restore changes under the daemon's feet.
const rcloneUnit = "rclone"

// unitControl starts and stops services; a seam so a test does not touch systemd.
type unitControl interface {
	Stop(unit string) error
	Start(unit string) error
	Restart(unit string) error
}

type systemdControl struct{}

func (systemdControl) Stop(unit string) error  { return exec.Command("systemctl", "stop", unit).Run() }
func (systemdControl) Start(unit string) error { return exec.Command("systemctl", "start", unit).Run() }
func (systemdControl) Restart(unit string) error {
	return exec.Command("systemctl", "restart", unit).Run()
}

// Systemd is the real thing.
func Systemd() unitControl { return systemdControl{} }

// systemBackupPaths is systemPaths and the backup schedules, whose file follows
// the data path, so it is read at the moment of the backup, not fixed.
func systemBackupPaths() []string {
	return append(slices.Clone(systemPaths), BackupSchedulePath)
}

// SystemBackupInventory lists what the box is, as backup entries.
func SystemBackupInventory(exists func(string) bool) []BackupEntry {
	paths := systemBackupPaths()
	entries := make([]BackupEntry, 0, len(paths))
	for _, p := range paths {
		entry := BackupEntry{Kind: BackupKindBind, Path: p, Target: p}
		if !exists(p) {
			entry.Skip = "not present on this box"
		}
		entries = append(entries, entry)
	}

	return entries
}

func pathExists(p string) bool {
	_, err := os.Stat(p)

	return err == nil
}

// PlanSystemBackup is the plan for the box, worked out from what is here now.
func PlanSystemBackup(exists func(string) bool) BackupManifest {
	manifest := PlanBackup(SystemBackupName, SystemBackupInventory(exists), nil)
	manifest.MarkBindFiles(func(p string) bool {
		info, err := os.Stat(p)

		return err == nil && info.Mode().IsRegular()
	})

	return manifest
}

// SystemBackupOptions is one backup of the box.
type SystemBackupOptions struct {
	Destination string
	Stamp       string
	// HoldStill stops the services that hold the files open for the copy.
	HoldStill bool
}

// RunSystemBackup copies the box's own state to a destination.
func RunSystemBackup(ctx context.Context, copier BackupCopier, units unitControl, opts SystemBackupOptions) (BackupManifest, error) {
	if opts.Destination == "" {
		return BackupManifest{}, errors.New("a backup needs a destination")
	}
	if opts.Stamp == "" {
		return BackupManifest{}, errors.New("a backup needs a name to file it under")
	}

	// it holds services, never an app, so Begin does not see it: see markInProgress
	defer markInProgress(SystemBackupName, "backup")()

	ctx = backupEventContext(ctx, &ComposeApp{Name: SystemBackupName}, opts.Destination, opts.Stamp, "backup")
	publishBackupBegin(ctx)

	manifest := PlanSystemBackup(pathExists)
	manifest.ContainersStopped = opts.HoldStill

	err := func() error {
		if opts.HoldStill {
			release, err := holdUnits(units, systemUnits)
			defer release()
			if err != nil {
				return err
			}
		}

		return copyPlan(ctx, &manifest, copier, opts.Destination, RootFor(SystemBackupName, opts.Stamp))
	}()
	if err != nil {
		publishBackupError(ctx, err)
		return manifest, err
	}
	publishBackupEnd(ctx)

	return manifest, nil
}

// holdUnits stops units in order and returns what starts them again in reverse,
// every one attempted, the way holdApp does for containers.
func holdUnits(units unitControl, names []string) (release func(), err error) {
	stopped := []string{}
	release = func() {
		for i := len(stopped) - 1; i >= 0; i-- {
			if err := units.Start(stopped[i]); err != nil {
				logger.Error("a service did not start again after the copy", zap.Error(err), zap.String("unit", stopped[i]))
			}
		}
	}

	for _, name := range names {
		if err := units.Stop(name); err != nil {
			return release, fmt.Errorf("could not stop %s, so the copy did not run: %w", name, err)
		}
		stopped = append(stopped, name)
	}

	return release, nil
}

// SystemBackupOnDemand runs one backup of the box and records it.
func SystemBackupOnDemand(ctx context.Context, copier BackupCopier, units unitControl, opts SystemBackupOptions) (BackupManifest, error) {
	record := BackupRunRecord{
		App: SystemBackupName, Destination: opts.Destination, Stamp: opts.Stamp,
		StartedAt: time.Now(), Scheduled: false, ContainersStopped: opts.HoldStill,
	}

	manifest, err := RunSystemBackup(ctx, copier, units, opts)
	finishBackupRecord(&record, manifest, err)

	return manifest, err
}

// SystemRestoreOptions is one restore of the box.
type SystemRestoreOptions struct {
	Destination string
	Stamp       string
}

// RestoreSystemBackup puts the box's own state back the way the backup had it.
//
// Where things go is this box's list, never the manifest's paths, as with an
// app. The services holding the files are stopped for the copy and started
// again whatever happens; rclone is restarted last, after everything has been
// copied through it, if its configuration was among what came back.
func RestoreSystemBackup(ctx context.Context, restorer BackupRestorer, units unitControl, opts SystemRestoreOptions) (RestoreReport, error) {
	report := RestoreReport{Restored: []BackupOperation{}, Missing: []BackupOperation{}, Skipped: []BackupEntry{}}
	if opts.Destination == "" {
		return report, ErrRestoreNeedsDestination
	}
	if opts.Stamp == "" {
		return report, ErrRestoreNeedsStamp
	}

	defer markInProgress(SystemBackupName, "restore")()

	root := RootFor(SystemBackupName, opts.Stamp)
	manifest, err := fetchManifest(ctx, restorer, opts.Destination, root)
	if err != nil {
		return report, err
	}
	if manifest.App != SystemBackupName {
		return report, fmt.Errorf("the backup at %s is of `%s`, not of this box", root, manifest.App)
	}
	report.Skipped = append(report.Skipped, manifest.Skipped...)

	ctx = backupEventContext(ctx, &ComposeApp{Name: SystemBackupName}, opts.Destination, opts.Stamp, "restore")
	publishBackupBegin(ctx)

	// Matched by the path each part belongs to, never by its number in the
	// backup. The numbers are given out in order over the parts that were
	// PRESENT when the backup was taken; a box without a Samba config had its
	// database folder as binds/2, and a restore that paired binds/2 with this
	// box's second path put that database folder where the Samba config goes
	// and the next folder's contents over the database. The path is the
	// identity; the number is where the copy happened to land.
	byTarget := map[string]BackupOperation{}
	for _, operation := range manifest.Operations {
		byTarget[operation.Target] = operation
	}

	// The plan is built from the paths, not from what exists: a folder this box
	// does not have yet is exactly what a restore to a fresh box puts there.
	local := PlanBackup(SystemBackupName, SystemBackupInventory(func(string) bool { return true }), nil)

	rcloneTouched := false
	err = func() error {
		release, err := holdUnits(units, systemUnits)
		defer release()
		if err != nil {
			return err
		}

		for i, operation := range local.Operations {
			held, ok := byTarget[operation.Target]
			if !ok {
				continue
			}
			delete(byTarget, operation.Target)
			publishBackupProgress(ctx, i, len(local.Operations), held.Destination)

			// where the copy landed, and what it was: the backup's knowledge, since
			// the path may not exist here yet
			remote := path.Join(root, held.Destination)
			operation.Directory = held.Directory
			if operation.Directory {
				err = restorer.RestoreDirectory(ctx, opts.Destination, remote, operation.Source)
			} else {
				err = restorer.RestoreFile(ctx, opts.Destination, remote, operation.Source)
			}
			if err != nil {
				return fmt.Errorf("restoring %s: %w", operation.Source, err)
			}
			if operation.Source == "/root/.config/rclone/rclone.conf" {
				rcloneTouched = true
			}

			report.Restored = append(report.Restored, operation)
		}

		return nil
	}()
	if err != nil {
		publishBackupError(ctx, err)
		return report, err
	}

	for _, operation := range manifest.Operations {
		if _, left := byTarget[operation.Target]; left {
			report.Missing = append(report.Missing, operation)
		}
	}

	// Last, once nothing more goes through the daemon: it reads its configuration
	// at start, and the file under it has just been replaced.
	if rcloneTouched {
		if err := units.Restart(rcloneUnit); err != nil {
			logger.Error("rclone did not restart after its configuration was put back", zap.Error(err))
		}
	}

	publishBackupEnd(ctx)

	return report, nil
}

// SystemRestoreOnDemand runs one restore of the box and records it.
func SystemRestoreOnDemand(ctx context.Context, restorer BackupRestorer, units unitControl, opts SystemRestoreOptions) (RestoreReport, error) {
	record := BackupRunRecord{
		App: SystemBackupName, Destination: opts.Destination, Stamp: opts.Stamp,
		StartedAt: time.Now(), Scheduled: false, ContainersStopped: true, Restore: true,
	}

	report, err := RestoreSystemBackup(ctx, restorer, units, opts)

	record.FinishedAt = time.Now()
	record.Copied = len(report.Restored)
	record.SkippedCount = len(report.Missing)
	if err != nil {
		record.Error = err.Error()
	}
	if logErr := RecordBackupRun(record); logErr != nil {
		logger.Error("a restore ran and could not be written down", zap.Error(logErr), zap.String("app", SystemBackupName))
	}

	return report, err
}
