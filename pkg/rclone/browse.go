package rclone

import (
	"context"
	"path"
	"time"
)

// Looking inside a backup, and taking some of it back.

// File is one file or folder held in a backup.
type File struct {
	Name      string    `json:"name"`
	Directory bool      `json:"directory"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
}

// List lists one level of `sub` inside the copy at remotePath: its files and its
// folders. An empty sub is the copy itself.
func (c *Client) List(_ context.Context, destinationName, remotePath, sub string) ([]File, error) {
	var listing struct {
		List []struct {
			Name    string    `json:"Name"`
			Size    int64     `json:"Size"`
			ModTime time.Time `json:"ModTime"`
			IsDir   bool      `json:"IsDir"`
		} `json:"list"`
	}

	err := c.call("/operations/list", map[string]string{
		"fs":     DestinationPrefix + destinationName + ":",
		"remote": path.Join(remotePath, sub),
	}, &listing)
	if err != nil {
		return nil, err
	}

	files := []File{}
	for _, entry := range listing.List {
		if entry.Name == "" {
			continue
		}

		size := entry.Size
		if entry.IsDir || size < 0 {
			size = 0
		}
		files = append(files, File{Name: entry.Name, Directory: entry.IsDir, Size: size, Modified: entry.ModTime})
	}

	return files, nil
}

// CopyBack puts `sub` of the copy at remotePath on hostPath: a file where the file
// goes, a folder's contents into the folder.
//
// sync/copy rather than sync/sync, unlike RestoreDirectory: this is a few things
// taken back out of a backup, and what else is in the folder now is not part of
// the request. Nothing is deleted here.
func (c *Client) CopyBack(ctx context.Context, destinationName, remotePath, sub, hostPath string) error {
	remote := path.Join(remotePath, sub)

	var stat struct {
		Item *struct {
			IsDir bool `json:"IsDir"`
		} `json:"item"`
	}
	err := c.call("/operations/stat", map[string]string{
		"fs":     DestinationPrefix + destinationName + ":",
		"remote": remote,
	}, &stat)
	if err != nil {
		return err
	}
	if stat.Item == nil {
		return &NotFoundError{Remote: remote}
	}

	if !stat.Item.IsDir {
		return c.RestoreFile(ctx, destinationName, remote, hostPath)
	}

	id, err := c.startJob("/sync/copy", map[string]string{
		"srcFs": Destination(destinationName, remote),
		"dstFs": hostPath,
	})
	if err != nil {
		return err
	}

	return c.waitForJob(ctx, id)
}

// NotFoundError is a path the destination does not hold.
type NotFoundError struct{ Remote string }

func (e *NotFoundError) Error() string { return "the destination holds nothing at " + e.Remote }
