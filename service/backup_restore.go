package service

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
)

// Putting an app back from a backup.
//
// The backup's manifest says what is there. It does not say where any of it goes:
// the host paths it carries are where things were on the box that took it, which
// may be this box a year ago or another box entirely, and a manifest is a file
// on a remote that anyone with the credentials can edit. So the places to put
// things back are worked out here, from the app as it is installed now, exactly
// the way a backup works them out -- and the manifest's paths are never used as
// a destination for anything. What the two plans have in common is restored;
// what the backup holds and the app no longer declares is reported, not written
// somewhere it was not asked for.

// BackupRestorer is the half of the rclone client a restore needs.
type BackupRestorer interface {
	RestoreDirectory(ctx context.Context, destinationName, remotePath, hostPath string) error
	RestoreFile(ctx context.Context, destinationName, remotePath, hostPath string) error
	Fetch(ctx context.Context, destinationName, remotePath string) ([]byte, error)
}

// RestoreInstaller puts an app that is not installed back from its compose file
// and, when the backup holds one, its .env, and returns it once its containers
// exist -- so that its named volumes exist too and have somewhere to be restored.
type RestoreInstaller func(ctx context.Context, name string, compose, env []byte) (*ComposeApp, error)

// RestoreOptions is one restore.
type RestoreOptions struct {
	Destination string
	App         string
	Stamp       string
	// Paths, when there are any, restore only those paths of the backup -- as browsing
	// it names them -- onto the app as it is, over what is there and deleting nothing.
	// Empty restores the whole backup, as it was.
	Paths []string
	// Containers lists the app's containers; nil means ask Docker. A seam for the
	// tests, as on BackupOptions.
	Containers func(ctx context.Context) (map[string][]codegen.ContainerSummary, error)
}

// RestoreReport is what a restore did, and what it could not.
type RestoreReport struct {
	// Restored, in the order it happened, with this host's paths
	Restored []BackupOperation
	// Missing is in the backup and has nowhere to go on this app: a volume it no
	// longer declares, a bind mount it no longer has. Reported, never guessed.
	Missing []BackupOperation
	// Skipped is what the backup itself left out, copied from its manifest so the
	// person restoring finds out here rather than by noticing something absent.
	Skipped []BackupEntry
	// Installed is true when the app was not installed and has been, from the
	// backup's own compose file.
	Installed bool
}

var (
	ErrRestoreNeedsDestination = errors.New("a restore needs a destination")
	ErrRestoreNeedsApp         = errors.New("a restore needs an app")
	ErrRestoreNeedsStamp       = errors.New("a restore needs the backup to restore from")
	ErrRestorePathsNeedApp     = errors.New("a restore of some files needs the app installed: install it, or restore the whole backup")
)

// RestoreBackup puts opts.App back the way the backup at opts.Stamp had it.
//
// installed is the app as it is now, or nil when it is not installed at all --
// in which case it is installed first, from the compose file the backup holds.
// The app is held still for the length of the copy either way, and started
// again afterwards, whatever happened in between.
func RestoreBackup(ctx context.Context, installed *ComposeApp, docker backupDocker, restorer BackupRestorer, install RestoreInstaller, opts RestoreOptions) (RestoreReport, error) {
	report := RestoreReport{Restored: []BackupOperation{}, Missing: []BackupOperation{}, Skipped: []BackupEntry{}}

	switch {
	case opts.Destination == "":
		return report, ErrRestoreNeedsDestination
	case opts.App == "":
		return report, ErrRestoreNeedsApp
	case opts.Stamp == "":
		return report, ErrRestoreNeedsStamp
	}

	// listed from the start, before the hold it takes once the manifest is read
	defer markInProgress(opts.App, "restore")()

	root := RootFor(opts.App, opts.Stamp)

	manifest, err := fetchManifest(ctx, restorer, opts.Destination, root)
	if err != nil {
		return report, err
	}
	// Said from here rather than from the top: an app that is not installed has
	// no title to say until its manifest has been read.
	ctx = backupEventContext(ctx, &ComposeApp{Name: opts.App}, opts.Destination, opts.Stamp, "restore")
	publishBackupBegin(ctx)
	report, err = restoreBackup(ctx, installed, docker, restorer, install, opts, manifest, root)
	if err != nil {
		publishBackupError(ctx, err)
		return report, err
	}
	publishBackupEnd(ctx)

	return report, nil
}

// restoreBackup is the copy itself; RestoreBackup wraps it in what it says.
func restoreBackup(ctx context.Context, installed *ComposeApp, docker backupDocker, restorer BackupRestorer, install RestoreInstaller, opts RestoreOptions, manifest BackupManifest, root string) (RestoreReport, error) {
	report := RestoreReport{Restored: []BackupOperation{}, Missing: []BackupOperation{}, Skipped: []BackupEntry{}}
	if manifest.App != opts.App {
		return report, fmt.Errorf("the backup at %s is of `%s`, not `%s`", root, manifest.App, opts.App)
	}

	end, err := Begin(opts.App, "restore")
	if err != nil {
		return report, err
	}
	defer end()

	report.Skipped = append(report.Skipped, manifest.Skipped...)

	byDestination := map[string]BackupOperation{}
	for _, operation := range manifest.Operations {
		byDestination[operation.Destination] = operation
	}

	app := installed
	if app == nil && len(opts.Paths) > 0 {
		return report, ErrRestorePathsNeedApp
	}
	if app == nil {
		compose, env, err := fetchDefinition(ctx, restorer, opts.Destination, root, byDestination)
		if err != nil {
			return report, err
		}

		// an app deployed from git comes back from its repository, not from a copy of its
		// compose file
		if manifest.Git != nil {
			app, err = restoreGitApp(ctx, opts.App, *manifest.Git, env)
		} else {
			app, err = install(ctx, opts.App, compose, env)
		}
		if err != nil {
			return report, fmt.Errorf("could not install `%s` from its backup: %w", opts.App, err)
		}
		report.Installed = true
	}

	// Where things go: worked out from the app as it is here and now, the same way
	// a backup works it out, and never read from the manifest.
	inventory := app.BackupInventory()
	wanted := []string{}
	byDockerName := map[string]string{}
	for _, entry := range inventory {
		if entry.Kind == BackupKindVolume && entry.Volume != "" && entry.Skip == "" {
			dockerName := app.DockerVolumeName(entry.Volume)
			wanted = append(wanted, dockerName)
			byDockerName[dockerName] = entry.Volume
		}
	}
	mountpoints := map[string]string{}
	for dockerName, mountpoint := range docker.VolumeMountpoints(ctx, wanted) {
		mountpoints[byDockerName[dockerName]] = mountpoint
	}
	local := PlanBackup(app.Name, inventory, mountpoints)
	local.MarkBindFiles(isRegularFile)

	// Held still for the copy, and started again whatever happens: restoring a
	// database under a running server is the mirror image of backing one up.
	listContainers := opts.Containers
	if listContainers == nil {
		listContainers = func(ctx context.Context) (map[string][]codegen.ContainerSummary, error) {
			return app.Containers(ctx)
		}
	}
	containerLists, err := listContainers(ctx)
	if err != nil {
		return report, fmt.Errorf("could not read the app's containers, so nothing was restored: %w", err)
	}
	release, err := holdApp(ctx, docker, app.Name, containerLists)
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			logger.Error("app did not come back after its restore", zap.Error(releaseErr), zap.String("app", app.Name))
		}
	}()
	if err != nil {
		return report, err
	}

	if len(opts.Paths) > 0 {
		partial, ok := restorer.(BackupPartialRestorer)
		if !ok {
			return report, errors.New("this box's backup engine cannot restore some paths of a backup")
		}

		err = restorePaths(ctx, partial, opts, manifest, root, local.Operations, &report)

		return report, err
	}

	for i, operation := range local.Operations {
		// The definition is not data. For an installed app the compose file it
		// runs now stays; the backup's copy was only ever for an app that had to
		// be installed first, and that has happened above by then.
		if operation.Kind == BackupKindCompose || operation.Kind == BackupKindEnv {
			delete(byDestination, operation.Destination)
			continue
		}

		if _, held := byDestination[operation.Destination]; !held {
			continue
		}
		delete(byDestination, operation.Destination)
		publishBackupProgress(ctx, i, len(local.Operations), operation.Destination)

		remote := path.Join(root, operation.Destination)
		if operation.Directory {
			err = restorer.RestoreDirectory(ctx, opts.Destination, remote, operation.Source)
		} else {
			err = restorer.RestoreFile(ctx, opts.Destination, remote, operation.Source)
		}
		if err != nil {
			return report, fmt.Errorf("restoring %s: %w", operation.Destination, err)
		}

		report.Restored = append(report.Restored, operation)
	}

	for _, operation := range manifest.Operations {
		if _, left := byDestination[operation.Destination]; left && operation.Kind != BackupKindCompose && operation.Kind != BackupKindEnv {
			report.Missing = append(report.Missing, operation)
		}
	}

	return report, nil
}

func fetchManifest(ctx context.Context, restorer BackupRestorer, destination, root string) (BackupManifest, error) {
	raw, err := restorer.Fetch(ctx, destination, path.Join(root, ManifestFileName))
	if err != nil {
		return BackupManifest{}, fmt.Errorf("reading the backup's manifest at %s: %w", root, err)
	}

	var manifest BackupManifest
	if err := stdjson.Unmarshal(raw, &manifest); err != nil {
		return BackupManifest{}, fmt.Errorf("the manifest at %s is not one this version can read: %w", root, err)
	}
	if manifest.FormatVersion != BackupFormatVersion {
		return BackupManifest{}, fmt.Errorf("the backup at %s is format %d and this version reads format %d", root, manifest.FormatVersion, BackupFormatVersion)
	}

	return manifest, nil
}

// fetchDefinition reads the compose file and, when the backup holds one, the
// .env, for an app that has to be installed before anything can be put back.
func fetchDefinition(ctx context.Context, restorer BackupRestorer, destination, root string, byDestination map[string]BackupOperation) (compose, env []byte, err error) {
	for _, operation := range byDestination {
		switch operation.Kind {
		case BackupKindCompose:
			compose, err = restorer.Fetch(ctx, destination, path.Join(root, operation.Destination))
		case BackupKindEnv:
			env, err = restorer.Fetch(ctx, destination, path.Join(root, operation.Destination))
		default:
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s from the backup: %w", operation.Destination, err)
		}
	}

	if compose == nil {
		return nil, nil, errors.New("the backup holds no compose file, so the app cannot be installed from it")
	}

	return compose, env, nil
}

// RestoreOnDemand runs one restore somebody asked for, and records it in the
// same log as the backups, marked as a restore.
func RestoreOnDemand(ctx context.Context, installed *ComposeApp, docker backupDocker, restorer BackupRestorer, install RestoreInstaller, opts RestoreOptions) (RestoreReport, error) {
	record := BackupRunRecord{
		App: opts.App, Destination: opts.Destination, Stamp: opts.Stamp,
		StartedAt: time.Now(), Scheduled: false, ContainersStopped: true, Restore: true,
	}

	report, err := RestoreBackup(ctx, installed, docker, restorer, install, opts)

	record.FinishedAt = time.Now()
	record.Copied = len(report.Restored)
	record.SkippedCount = len(report.Missing)
	if err != nil {
		record.Error = err.Error()
	}
	if logErr := RecordBackupRun(record); logErr != nil {
		logger.Error("a restore ran and could not be written down", zap.Error(logErr), zap.String("app", opts.App))
	}

	return report, err
}
