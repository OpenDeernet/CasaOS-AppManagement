package rclone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"time"
)

// Copying, which the daemon does as a job rather than inside the call.
//
// A transfer takes as long as it takes -- minutes for a photo library, longer over
// a slow link -- so nothing here holds an HTTP request open waiting for one.
// rclone is asked to start a job, answers with its number, and is polled.

// pollInterval is how often a running job is asked how it is getting on. Often
// enough that cancelling feels immediate, rarely enough that a long copy is not
// mostly this.
var pollInterval = 2 * time.Second

// JobStatus is what the daemon says about a job.
type JobStatus struct {
	ID       int64   `json:"id"`
	Finished bool    `json:"finished"`
	Success  bool    `json:"success"`
	Error    string  `json:"error"`
	Duration float64 `json:"duration"`
	// Output is the endpoint's own answer, which can say failure while the job succeeds.
	Output json.RawMessage `json:"output"`
}

// startJob asks for an async call and returns the job number.
func (c *Client) startJob(endpoint string, params map[string]string) (int64, error) {
	params["_async"] = "true"

	var started struct {
		JobID int64 `json:"jobid"`
	}
	if err := c.call(endpoint, params, &started); err != nil {
		return 0, err
	}

	if started.JobID == 0 {
		return 0, fmt.Errorf("rclone accepted %s without starting a job", endpoint)
	}

	return started.JobID, nil
}

// JobStatus asks after one job.
func (c *Client) JobStatus(id int64) (JobStatus, error) {
	var status JobStatus
	err := c.call("/job/status", map[string]string{"jobid": strconv.FormatInt(id, 10)}, &status)

	return status, err
}

// StopJob asks the daemon to abandon a job.
func (c *Client) StopJob(id int64) error {
	return c.call("/job/stop", map[string]string{"jobid": strconv.FormatInt(id, 10)}, nil)
}

// waitForJob polls until the job ends, the context is cancelled, or the daemon
// stops answering.
//
// A cancelled context stops the job rather than merely walking away from it:
// abandoning a running transfer leaves it writing to the destination long after
// whoever asked for it has gone, and the next run then races it.
func (c *Client) waitForJob(ctx context.Context, id int64) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		status, err := c.JobStatus(id)
		if err != nil {
			return err
		}

		if status.Finished {
			if status.Success {
				return nil
			}
			if status.Error != "" {
				return fmt.Errorf("rclone: %s", status.Error)
			}

			return fmt.Errorf("rclone job %d failed without saying why", id)
		}

		select {
		case <-ctx.Done():
			if stopErr := c.StopJob(id); stopErr != nil {
				return errors.Join(ctx.Err(), fmt.Errorf("and job %d could not be stopped: %w", id, stopErr))
			}

			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Destination turns a destination name and a path inside it into the `remote:path`
// rclone expects.
func Destination(name, remotePath string) string {
	return DestinationPrefix + name + ":" + remotePath
}

// CopyDirectory copies a folder and everything under it, and blocks until it is
// done.
//
// This is rclone's `copy`, not `sync`: it adds and updates, and never deletes
// anything at the destination. A backup that deletes is a backup that can destroy
// the previous one when the source is wrong -- an app whose folder was unmounted
// at the moment of the run looks exactly like an app whose files were all deleted.
func (c *Client) CopyDirectory(ctx context.Context, source, destinationName, destinationPath string) error {
	id, err := c.startJob("/sync/copy", map[string]string{
		"srcFs": source,
		"dstFs": Destination(destinationName, destinationPath),
	})
	if err != nil {
		return err
	}

	return c.waitForJob(ctx, id)
}

// CopyFile copies a single file.
//
// rclone addresses a file as a folder plus a name, on both sides, which is why
// this takes the whole path and splits it: handing `/etc/app/php.ini` to the
// directory call above would copy `/etc/app` entire.
func (c *Client) CopyFile(ctx context.Context, source, destinationName, destinationPath string) error {
	id, err := c.startJob("/operations/copyfile", map[string]string{
		"srcFs":     path.Dir(source),
		"srcRemote": path.Base(source),
		"dstFs":     Destination(destinationName, path.Dir(destinationPath)),
		"dstRemote": path.Base(destinationPath),
	})
	if err != nil {
		return err
	}

	return c.waitForJob(ctx, id)
}

// Runs lists the runs an app has at a destination, by their stamp.
//
// Directories only, one level down: the layout puts every run of an app in its
// own folder under the app's, and nothing else lives there.
func (c *Client) Runs(destinationName, appPath string) ([]string, error) {
	var listing struct {
		List []struct {
			Name  string `json:"Name"`
			IsDir bool   `json:"IsDir"`
		} `json:"list"`
	}

	err := c.call("/operations/list", map[string]string{
		"fs":     DestinationPrefix + destinationName + ":",
		"remote": appPath,
		"opt":    `{"dirsOnly": true}`,
	}, &listing)
	if err != nil {
		return nil, err
	}

	stamps := []string{}
	for _, entry := range listing.List {
		if entry.IsDir && entry.Name != "" {
			stamps = append(stamps, entry.Name)
		}
	}

	return stamps, nil
}

// Purge deletes one run and everything under it.
//
// The only call in this package that removes anything, kept separate from the
// copies for that reason: nothing that copies can reach it by accident.
func (c *Client) Purge(ctx context.Context, destinationName, remotePath string) error {
	id, err := c.startJob("/operations/purge", map[string]string{
		"fs":     DestinationPrefix + destinationName + ":",
		"remote": remotePath,
	})
	if err != nil {
		return err
	}

	return c.waitForJob(ctx, id)
}
