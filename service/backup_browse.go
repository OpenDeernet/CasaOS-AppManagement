package service

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
)

// Looking inside a backup, and taking only some of it back.
//
// What a backup holds is a set of copies -- a volume, a bind mount, a file -- filed
// under the destination each operation of its manifest names. A person browsing it
// sees one tree: the folders that lead to those names, and below them what each copy
// holds. A path in that tree is relative to the backup and always uses forward
// slashes.

// BackupBrowser is the half of the engine a look inside a backup needs.
type BackupBrowser interface {
	BackupRestorer
	List(ctx context.Context, destinationName, remotePath, sub string) ([]rclone.File, error)
}

// BackupPartialRestorer is what taking some paths of a backup back needs.
type BackupPartialRestorer interface {
	BackupRestorer
	CopyBack(ctx context.Context, destinationName, remotePath, sub, hostPath string) error
}

var (
	ErrBackupPathOutside = errors.New("a path in a backup stays inside it")
	ErrBackupPathNotHeld = errors.New("the backup holds nothing at that path")
	ErrRestoreBoxInParts = errors.New("the backup of the box is looked into and restored whole, not in parts")
)

// CheckRestorePaths refuses, before anything is started, a path that leaves the backup.
func CheckRestorePaths(paths []string) error {
	for _, wanted := range paths {
		if _, err := cleanBackupPath(wanted); err != nil {
			return err
		}
	}

	return nil
}

// cleanBackupPath is a path inside a backup, cleaned, or an error when it leaves it.
// "" and "/" are the top of the backup.
func cleanBackupPath(rel string) (string, error) {
	rel = strings.ReplaceAll(rel, "\\", "/")

	// path.Clean("/"+rel) cannot climb above "/", so "../x" would be quietly turned
	// into "x": a path that asks to climb is refused instead
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q", ErrBackupPathOutside, rel)
		}
	}

	return strings.TrimPrefix(path.Clean("/"+rel), "/"), nil
}

// dataOperations are the copies that are data: the compose file and the .env are the
// definition of the app, which a restore of an installed app never touches.
func dataOperations(manifest BackupManifest) []BackupOperation {
	operations := make([]BackupOperation, 0, len(manifest.Operations))
	for _, operation := range manifest.Operations {
		if operation.Kind != BackupKindCompose && operation.Kind != BackupKindEnv {
			operations = append(operations, operation)
		}
	}

	return operations
}

// operationFor is the copy that holds rel, with the rest of the path inside it. The
// longest name wins, so a copy filed inside another's folder is not taken for part
// of it.
func operationFor(operations []BackupOperation, rel string) (BackupOperation, string, bool) {
	var found BackupOperation
	var rest string
	held := false

	for _, operation := range operations {
		if rel != operation.Destination && !strings.HasPrefix(rel, operation.Destination+"/") {
			continue
		}
		if held && len(operation.Destination) <= len(found.Destination) {
			continue
		}

		found, held = operation, true
		rest = strings.TrimPrefix(strings.TrimPrefix(rel, operation.Destination), "/")
	}

	return found, rest, held
}

// BrowseBackup lists one level of a backup: the folders that lead to its copies at
// the top, and what a copy holds below that. Folders first, then by name.
func BrowseBackup(ctx context.Context, browser BackupBrowser, destination, app, stamp, rel string) ([]rclone.File, error) {
	switch {
	case destination == "":
		return nil, ErrRestoreNeedsDestination
	case app == "":
		return nil, ErrRestoreNeedsApp
	case stamp == "":
		return nil, ErrRestoreNeedsStamp
	}

	rel, err := cleanBackupPath(rel)
	if err != nil {
		return nil, err
	}

	root := RootFor(app, stamp)
	manifest, err := fetchManifest(ctx, browser, destination, root)
	if err != nil {
		return nil, err
	}
	operations := dataOperations(manifest)

	var files []rclone.File
	if operation, rest, held := operationFor(operations, rel); held {
		files, err = browser.List(ctx, destination, path.Join(root, operation.Destination), rest)
		if err != nil {
			return nil, err
		}
	} else {
		files = foldersTo(operations, rel)
		if len(files) == 0 {
			return nil, fmt.Errorf("%w: %q", ErrBackupPathNotHeld, rel)
		}
	}

	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Directory != files[j].Directory {
			return files[i].Directory
		}

		return files[i].Name < files[j].Name
	})

	return files, nil
}

// foldersTo is the next step towards each copy filed below rel: a folder for each
// name on the way, and the copy itself where the way ends.
func foldersTo(operations []BackupOperation, rel string) []rclone.File {
	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}

	seen := map[string]bool{}
	files := []rclone.File{}
	for _, operation := range operations {
		rest, below := strings.CutPrefix(operation.Destination, prefix)
		if !below || rest == "" {
			continue
		}

		name, more, _ := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}

		seen[name] = true
		files = append(files, rclone.File{Name: name, Directory: more != "" || operation.Directory})
	}

	return files
}

// restorePaths takes the paths asked for back from the backup, onto the places the
// app has now, and deletes nothing there. A path the app no longer has a place for
// is reported and does not stop the others.
func restorePaths(ctx context.Context, restorer BackupPartialRestorer, opts RestoreOptions, manifest BackupManifest, root string, local []BackupOperation, report *RestoreReport) error {
	held := dataOperations(manifest)
	here := map[string]BackupOperation{}
	for _, operation := range local {
		here[operation.Destination] = operation
	}

	for i, wanted := range opts.Paths {
		rel, err := cleanBackupPath(wanted)
		if err != nil {
			return err
		}

		operation, rest, found := operationFor(held, rel)
		if !found {
			return fmt.Errorf("%w: %q", ErrBackupPathNotHeld, wanted)
		}

		target, ok := here[operation.Destination]
		if !ok {
			// in the backup and nowhere to go on this app: said, not guessed
			report.Missing = append(report.Missing, operation)
			continue
		}

		hostPath := target.Source
		if rest != "" {
			hostPath = filepath.Join(target.Source, filepath.FromSlash(rest))
		}

		publishBackupProgress(ctx, i, len(opts.Paths), rel)
		if err := restorer.CopyBack(ctx, opts.Destination, path.Join(root, operation.Destination), rest, hostPath); err != nil {
			return fmt.Errorf("restoring %s: %w", rel, err)
		}

		report.Restored = append(report.Restored, BackupOperation{
			Kind: operation.Kind, Source: hostPath, Destination: rel, Directory: operation.Directory && rest == "",
			Volume: operation.Volume, Service: operation.Service, Target: operation.Target,
		})
	}

	return nil
}
