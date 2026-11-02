package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
)

// Looking inside a snapshot, and taking some of it back.

// lsNode is one line of `restic ls --json`: the snapshot first, then its nodes.
type lsNode struct {
	StructType string    `json:"struct_type"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	Mtime      time.Time `json:"mtime"`
}

// ls reads what restic lists at dir of a snapshot. Only what sits right under dir
// is kept, and dir itself when it is a file: whether restic lists a folder's
// descendants or only its children has changed between versions, and neither is
// what a person browsing one level wants.
func (e *Engine) ls(ctx context.Context, destinationName, snapshotID, dir string) ([]lsNode, error) {
	out, err := e.call(ctx, destinationName, "ls", "--json", snapshotID, dir)
	if err != nil {
		return nil, err
	}

	nodes := []lsNode{}
	decoder := json.NewDecoder(bytes.NewReader(out))
	for decoder.More() {
		var node lsNode
		if err := decoder.Decode(&node); err != nil {
			return nil, fmt.Errorf("restic listed a snapshot in a form this box does not read: %w", err)
		}
		if node.StructType == "snapshot" || node.Name == "" || node.Path == "" {
			continue
		}

		if path.Dir(node.Path) == dir || (node.Path == dir && node.Type != "dir") {
			nodes = append(nodes, node)
		}
	}

	return nodes, nil
}

// List lists one level of `sub` inside the snapshot taken for remotePath.
func (e *Engine) List(ctx context.Context, destinationName, remotePath, sub string) ([]rclone.File, error) {
	snap, err := e.find(ctx, destinationName, remotePath)
	if err != nil {
		return nil, err
	}

	nodes, err := e.ls(ctx, destinationName, snap.ID, path.Join(snap.Paths[0], sub))
	if err != nil {
		return nil, err
	}

	files := make([]rclone.File, 0, len(nodes))
	for _, node := range nodes {
		files = append(files, rclone.File{Name: node.Name, Directory: node.Type == "dir", Size: node.Size, Modified: node.Mtime})
	}

	return files, nil
}

// CopyBack puts `sub` of the snapshot taken for remotePath on hostPath, with its
// owners and modes, over what is there and deleting nothing.
func (e *Engine) CopyBack(ctx context.Context, destinationName, remotePath, sub, hostPath string) error {
	snap, err := e.find(ctx, destinationName, remotePath)
	if err != nil {
		return err
	}

	abs := path.Join(snap.Paths[0], sub)
	if sub != "" {
		// it sits in a folder of the snapshot: the folder is the root to restore
		// from, and its name the one thing to take. The host path ends in the same
		// name, which the caller built from sub.
		_, err = e.call(ctx, destinationName, "restore", snap.ID+":"+path.Dir(abs),
			"--target", filepath.ToSlash(filepath.Dir(hostPath)), "--include", "/"+globEscape(path.Base(abs)))

		return err
	}

	// the whole copy: a folder's contents into the folder, or the one file
	nodes, err := e.ls(ctx, destinationName, snap.ID, abs)
	if err != nil {
		return err
	}
	if len(nodes) == 1 && nodes[0].Path == abs {
		return e.RestoreFile(ctx, destinationName, remotePath, hostPath)
	}

	_, err = e.call(ctx, destinationName, "restore", snap.ID+":"+abs, "--target", hostPath)

	return err
}

// globEscape keeps a name from being read as a pattern by --include.
func globEscape(name string) string {
	return strings.NewReplacer(`\`, `\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(name)
}
