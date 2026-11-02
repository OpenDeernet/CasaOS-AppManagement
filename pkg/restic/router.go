package restic

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
)

// legacy is the rclone side of the nine calls: the stamped copies.
type legacy interface {
	CopyDirectory(ctx context.Context, source, destinationName, destinationPath string) error
	CopyFile(ctx context.Context, source, destinationName, destinationPath string) error
	RestoreDirectory(ctx context.Context, destinationName, remotePath, hostPath string) error
	RestoreFile(ctx context.Context, destinationName, remotePath, hostPath string) error
	Fetch(ctx context.Context, destinationName, remotePath string) ([]byte, error)
	Runs(destinationName, appPath string) ([]string, error)
	Purge(ctx context.Context, destinationName, remotePath string) error
	List(ctx context.Context, destinationName, remotePath, sub string) ([]rclone.File, error)
	CopyBack(ctx context.Context, destinationName, remotePath, sub, hostPath string) error
}

// Router answers a backup's nine calls. The copies go where the destination keeps
// its backups now -- restic where it is switched to it, rclone everywhere else.
// What is read goes to both while the box holds the key of a repository there: the
// stamped copies a destination held before it was switched, and the repository it
// keeps after it was switched back, stay listed, restorable and subject to the
// schedule's retention, which counts them as the one set of runs they are.
//
// It carries the rclone client along, so the calls that are not about the bytes of
// a backup -- listing destinations, checking one, creating one -- reach rclone
// unchanged.
type Router struct {
	*rclone.Client
	Restic *Engine

	legacy legacy
}

// NewRouter is the engine every backup call goes through.
func NewRouter() *Router {
	client := rclone.NewClient()

	return &Router{Client: client, Restic: New(), legacy: client}
}

// isNotFound is rclone saying a folder is not there: for an app that only has runs
// in the repository, the stamped side has no folder of its name.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "directory not found")
}

func (r *Router) CopyDirectory(ctx context.Context, source, destinationName, destinationPath string) error {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.CopyDirectory(ctx, source, destinationName, destinationPath)
	}

	return r.legacy.CopyDirectory(ctx, source, destinationName, destinationPath)
}

func (r *Router) CopyFile(ctx context.Context, source, destinationName, destinationPath string) error {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.CopyFile(ctx, source, destinationName, destinationPath)
	}

	return r.legacy.CopyFile(ctx, source, destinationName, destinationPath)
}

// The repository is asked first, and the stamped copies only when it holds nothing
// at that path: a run lives in one of the two, its stamp is a time. Any other answer
// of the repository is its own and is not covered up by a second try.

func (r *Router) RestoreDirectory(ctx context.Context, destinationName, remotePath, hostPath string) error {
	if r.Restic.Held(destinationName) {
		err := r.Restic.RestoreDirectory(ctx, destinationName, remotePath, hostPath)
		if !errors.Is(err, ErrNothingHeld) {
			return err
		}
	}

	return r.legacy.RestoreDirectory(ctx, destinationName, remotePath, hostPath)
}

func (r *Router) RestoreFile(ctx context.Context, destinationName, remotePath, hostPath string) error {
	if r.Restic.Held(destinationName) {
		err := r.Restic.RestoreFile(ctx, destinationName, remotePath, hostPath)
		if !errors.Is(err, ErrNothingHeld) {
			return err
		}
	}

	return r.legacy.RestoreFile(ctx, destinationName, remotePath, hostPath)
}

// List and CopyBack follow the same rule as the restores: the repository first, the
// stamped copies when it holds nothing at that path.

func (r *Router) List(ctx context.Context, destinationName, remotePath, sub string) ([]rclone.File, error) {
	if r.Restic.Held(destinationName) {
		files, err := r.Restic.List(ctx, destinationName, remotePath, sub)
		if !errors.Is(err, ErrNothingHeld) {
			return files, err
		}
	}

	return r.legacy.List(ctx, destinationName, remotePath, sub)
}

func (r *Router) CopyBack(ctx context.Context, destinationName, remotePath, sub, hostPath string) error {
	if r.Restic.Held(destinationName) {
		err := r.Restic.CopyBack(ctx, destinationName, remotePath, sub, hostPath)
		if !errors.Is(err, ErrNothingHeld) {
			return err
		}
	}

	return r.legacy.CopyBack(ctx, destinationName, remotePath, sub, hostPath)
}

func (r *Router) Fetch(ctx context.Context, destinationName, remotePath string) ([]byte, error) {
	if r.Restic.Held(destinationName) {
		content, err := r.Restic.Fetch(ctx, destinationName, remotePath)
		if !errors.Is(err, ErrNothingHeld) {
			return content, err
		}
	}

	return r.legacy.Fetch(ctx, destinationName, remotePath)
}

// Runs lists the apps a destination holds backups of, or an app's runs by stamp:
// those of the repository and the stamped copies together.
func (r *Router) Runs(destinationName, appPath string) ([]string, error) {
	if !r.Restic.Held(destinationName) {
		return r.legacy.Runs(destinationName, appPath)
	}

	inRepository, err := r.Restic.Runs(destinationName, appPath)
	if err != nil {
		return nil, err
	}

	stamped, err := r.legacy.Runs(destinationName, appPath)
	if err != nil && !isNotFound(err) {
		return nil, err
	}

	seen := map[string]bool{}
	names := []string{}
	for _, name := range append(inRepository, stamped...) {
		// the repository's own folder sits among the apps and is not one
		if seen[name] || (appPath == "" && name == RepositoryFolder) {
			continue
		}

		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)

	return names, nil
}

// Purge deletes one run, from the repository if it is there and otherwise as the
// stamped copy it is.
func (r *Router) Purge(ctx context.Context, destinationName, remotePath string) error {
	if r.Restic.Held(destinationName) {
		forgotten, err := r.Restic.forget(ctx, destinationName, remotePath)
		if err != nil {
			return err
		}
		if forgotten {
			return nil
		}

		// a run neither side holds was deleted already: that is not an error here
		if err := r.legacy.Purge(ctx, destinationName, remotePath); err != nil && !isNotFound(err) {
			return err
		}

		return nil
	}

	return r.legacy.Purge(ctx, destinationName, remotePath)
}
