package restic

import (
	"context"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
)

// Router answers a backup's seven calls with restic where the destination keeps
// its backups with restic, and with rclone everywhere else.
//
// It carries the rclone client along, so the calls that are not about the bytes of
// a backup -- listing destinations, checking one, creating one -- reach rclone
// unchanged.
type Router struct {
	*rclone.Client
	Restic *Engine
}

// NewRouter is the engine every backup call goes through.
func NewRouter() *Router {
	return &Router{Client: rclone.NewClient(), Restic: New()}
}

func (r *Router) CopyDirectory(ctx context.Context, source, destinationName, destinationPath string) error {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.CopyDirectory(ctx, source, destinationName, destinationPath)
	}

	return r.Client.CopyDirectory(ctx, source, destinationName, destinationPath)
}

func (r *Router) CopyFile(ctx context.Context, source, destinationName, destinationPath string) error {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.CopyFile(ctx, source, destinationName, destinationPath)
	}

	return r.Client.CopyFile(ctx, source, destinationName, destinationPath)
}

func (r *Router) RestoreDirectory(ctx context.Context, destinationName, remotePath, hostPath string) error {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.RestoreDirectory(ctx, destinationName, remotePath, hostPath)
	}

	return r.Client.RestoreDirectory(ctx, destinationName, remotePath, hostPath)
}

func (r *Router) RestoreFile(ctx context.Context, destinationName, remotePath, hostPath string) error {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.RestoreFile(ctx, destinationName, remotePath, hostPath)
	}

	return r.Client.RestoreFile(ctx, destinationName, remotePath, hostPath)
}

func (r *Router) Fetch(ctx context.Context, destinationName, remotePath string) ([]byte, error) {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.Fetch(ctx, destinationName, remotePath)
	}

	return r.Client.Fetch(ctx, destinationName, remotePath)
}

func (r *Router) Runs(destinationName, appPath string) ([]string, error) {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.Runs(destinationName, appPath)
	}

	return r.Client.Runs(destinationName, appPath)
}

func (r *Router) Purge(ctx context.Context, destinationName, remotePath string) error {
	if r.Restic.Enabled(destinationName) {
		return r.Restic.Purge(ctx, destinationName, remotePath)
	}

	return r.Client.Purge(ctx, destinationName, remotePath)
}
