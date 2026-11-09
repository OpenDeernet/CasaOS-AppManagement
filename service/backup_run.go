package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	stdjson "encoding/json"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
)

// Running a backup: the one place the pieces meet.
//
// Everything it needs is decided before anything is copied -- the inventory, the
// plan, where Docker keeps each volume -- so the part that touches the destination
// has no decisions left to make, and the part that makes decisions can be read
// without a network.

// BackupCopier is the destination, as this needs it. *rclone.Client is one.
type BackupCopier interface {
	CopyDirectory(ctx context.Context, source, destinationName, destinationPath string) error
	CopyFile(ctx context.Context, source, destinationName, destinationPath string) error
}

// BackupOptions is one run.
type BackupOptions struct {
	// Destination is the name of an rclone remote this project manages
	Destination string
	// Stamp names this run inside the destination. Passed in, not read from a
	// clock, so a caller can name a run and find it again.
	Stamp string
	// HoldStill stops the app's containers for the length of the copy. Off means a
	// copy taken from a running app, which for anything holding a database may not
	// restore -- the manifest records which was done.
	HoldStill bool
	// Containers answers which containers the app has, for HoldStill. Nil means ask
	// the app, which is what production does; a test supplies its own so that
	// exercising the hold does not require a daemon -- and does not leave the
	// daemon's client goroutines behind for the next test to trip over.
	Containers func(context.Context) (map[string][]codegen.ContainerSummary, error)
}

// backupDocker is the half of DockerService a run needs.
type backupDocker interface {
	containerStopStarter
	VolumeMountpoints(ctx context.Context, names []string) map[string]string
}

// backupPatience is how long a backup held still waits for whatever else holds the
// app. Long enough for an install or an update to finish pulling its images, short
// enough that a hold nobody lets go of still ends as a refusal somebody can read.
const backupPatience = 10 * time.Minute

// RunBackup copies an app to a destination and returns the manifest it wrote.
//
// The manifest goes last, after everything it describes. A run interrupted
// half-way leaves files and no manifest, which reads as what it is -- an
// incomplete backup -- rather than as a complete one missing its contents.
func RunBackup(ctx context.Context, app *ComposeApp, docker backupDocker, copier BackupCopier, opts BackupOptions) (BackupManifest, error) {
	if opts.Destination == "" {
		return BackupManifest{}, errors.New("a backup needs a destination")
	}
	if opts.Stamp == "" {
		return BackupManifest{}, errors.New("a backup needs a name to file it under")
	}

	// listed from the start, held still or not: see markInProgress
	defer markInProgress(app.Name, "backup")()

	ctx = backupEventContext(ctx, app, opts.Destination, opts.Stamp, "backup")
	// listed before anything is held still, so a backup waiting for its app can be stopped too
	ctx, endLive := beginLive(ctx, app.Name, opts.Destination, opts.Stamp)
	defer endLive()
	publishBackupBegin(ctx)
	manifest, err := runBackup(ctx, app, docker, copier, opts)
	err = stoppedFromInterface(ctx, err)
	if err != nil {
		publishBackupError(ctx, err)
		return manifest, err
	}
	publishBackupEnd(ctx)

	return manifest, nil
}

// runBackup is the copy itself; RunBackup wraps it in what it says about it.
func runBackup(ctx context.Context, app *ComposeApp, docker backupDocker, copier BackupCopier, opts BackupOptions) (BackupManifest, error) {

	inventory := app.BackupInventory()

	// Which volumes, then where they are. Asked for all of them at once so one
	// unreachable daemon is one failure rather than one per volume.
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

	manifest := PlanBackup(app.Name, inventory, mountpoints)
	manifest.MarkBindFiles(isRegularFile)
	manifest.Git = gitBackupOrigin(app.Name)

	hold := opts.HoldStill
	if hold && servesDNS(app.Services) {
		hold = false
		logger.Info("backup not held still: the app answers DNS, and stopped it would take the box's lookups down with it",
			zap.String("app", app.Name))
	}
	manifest.ContainersStopped = hold

	if hold {
		end, err := beginWaiting(ctx, app.Name, "backup", backupPatience)
		if err != nil {
			return manifest, err
		}
		defer end()

		listContainers := opts.Containers
		if listContainers == nil {
			listContainers = func(ctx context.Context) (map[string][]codegen.ContainerSummary, error) {
				return app.Containers(ctx)
			}
		}

		containerLists, err := listContainers(ctx)
		if err != nil {
			return manifest, fmt.Errorf("could not read the app's containers, so it was not held still: %w", err)
		}

		release, err := holdApp(ctx, docker, app.Name, containerLists)
		// Deferred before the error is examined: a stop that failed part-way has
		// already put containers down, and they come back either way.
		defer func() {
			if releaseErr := release(); releaseErr != nil {
				logger.Error("app did not come back after its backup", zap.Error(releaseErr), zap.String("app", app.Name))
			}
		}()

		if err != nil {
			return manifest, err
		}
	}

	err := copyPlan(ctx, &manifest, copier, opts.Destination, RootFor(app.Name, opts.Stamp))

	return manifest, err
}

// copyPlan carries out a manifest's operations, in order, adds up what they
// read, then writes the manifest at the root. Shared by an app's backup and the
// box's own.
func copyPlan(ctx context.Context, manifest *BackupManifest, copier BackupCopier, destinationName, root string) error {
	for i, operation := range manifest.Operations {
		liveOperation(ctx, i+1, len(manifest.Operations), operation.Source)
		publishBackupProgress(ctx, i, len(manifest.Operations)+1, operation.Destination)
		destination := path.Join(root, operation.Destination)

		var err error
		if operation.Directory {
			err = copier.CopyDirectory(ctx, operation.Source, destinationName, destination)
		} else {
			err = copier.CopyFile(ctx, operation.Source, destinationName, destination)
		}

		if err != nil {
			return fmt.Errorf("copying %s: %w", operation.Source, err)
		}
		manifest.Bytes += sourceBytes(operation)
	}

	publishBackupProgress(ctx, len(manifest.Operations), len(manifest.Operations)+1, ManifestFileName)

	return writeManifest(ctx, *manifest, copier, destinationName, root)
}

// sourceBytes is the size of what an operation read, measured on this host: a
// file's size, or the regular files under a folder added up. Measured here
// rather than asked of the engine, so rclone and restic answer alike; a file
// that cannot be read counts for nothing, the copy itself has said what failed.
func sourceBytes(operation BackupOperation) int64 {
	if !operation.Directory {
		info, err := os.Stat(operation.Source)
		if err != nil || !info.Mode().IsRegular() {
			return 0
		}

		return info.Size()
	}

	var total int64
	_ = filepath.WalkDir(operation.Source, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}

		return nil
	})

	return total
}

// writeManifest puts the manifest at the root of the backup.
//
// Through a local temp file, because rclone copies things that exist rather than
// accepting content: there is no rc call that writes bytes to a remote.
func writeManifest(ctx context.Context, manifest BackupManifest, copier BackupCopier, destination, root string) error {
	encoded, err := stdjson.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}

	temp, err := os.MkdirTemp("", "casaos-backup-manifest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)

	local := filepath.Join(temp, ManifestFileName)
	if err := os.WriteFile(local, encoded, 0o600); err != nil {
		return err
	}

	return copier.CopyFile(ctx, filepath.ToSlash(local), destination, path.Join(root, ManifestFileName))
}

// isRegularFile answers the question the compose file cannot: whether a bind
// source is one file or a folder.
func isRegularFile(p string) bool {
	info, err := os.Stat(p)

	return err == nil && info.Mode().IsRegular()
}

// compile-time check that the real docker service can run a backup
var _ backupDocker = (DockerService)(nil)
