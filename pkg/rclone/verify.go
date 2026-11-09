package rclone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrVerificationJobUncertain means a daemon job may still access its staging
// directory. The caller must retain that directory instead of removing it.
var ErrVerificationJobUncertain = errors.New("the verification job could not be confirmed stopped")

var verificationStopTimeout = 30 * time.Second

type VerificationSize struct {
	Bytes int64
	Files int64
}

// Verification deliberately resets inherited filters: a daemon-wide exclude or
// size filter must not turn a partial read into a successful full check.
func verificationFilter(filesFromRaw string) string {
	filter := map[string]any{
		"DeleteExcluded": false, "FilterRule": []string{}, "FilterFrom": []string{},
		"ExcludeRule": []string{}, "ExcludeFrom": []string{}, "IncludeRule": []string{}, "IncludeFrom": []string{},
		"ExcludeFile": []string{}, "FilesFrom": []string{}, "FilesFromRaw": []string{}, "FilesFrom0": []string{},
		"MinAge": "off", "MaxAge": "off", "MinSize": -1, "MaxSize": -1, "IgnoreCase": false, "HashFilter": "",
		"MetaRules": map[string]any{"FilterRule": []string{}, "FilterFrom": []string{}, "ExcludeRule": []string{}, "ExcludeFrom": []string{}, "IncludeRule": []string{}, "IncludeFrom": []string{}},
	}
	if filesFromRaw != "" {
		filter["FilesFromRaw"] = []string{filesFromRaw}
	}
	encoded, _ := json.Marshal(filter)
	return string(encoded)
}

// _config is per-job in rclone v1.75.1 (fs/rc/context.go, jobs/job.go).
// The JSON field names are ConfigInfo names, not command-line flag names.
func verificationConfig(ctx context.Context, maxBytes int64) string {
	duration := time.Hour
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < duration {
		duration = time.Until(deadline)
	}
	if duration <= 0 {
		duration = time.Nanosecond
	}
	// rclone refuses a copy once bytes so far >= MaxTransfer, which at exactly
	// maxBytes refuses every empty file; the caller compares sizes afterwards.
	encoded, _ := json.Marshal(map[string]any{
		"MaxTransfer": maxBytes + 1, "MaxDuration": duration.String(), "CutoffMode": "HARD",
		"Transfers": 1, "Checkers": 1, "LowLevelRetries": 1, "Links": false,
		"SizeOnly": false, "IgnoreSize": false, "IgnoreChecksum": false,
		"IgnoreExisting": false, "IgnoreTimes": true, "UpdateOlder": false, "MaxDepth": -1,
	})
	return string(encoded)
}

func verificationLocal(local string) string {
	// CopyLinks is a local-backend option, not a ConfigInfo field. Explicit
	// connection options also override any daemon-wide local backend defaults.
	return ":local,copy_links=false,links=false,no_clone=true:" + filepath.ToSlash(local)
}

// verificationJob checks both the job envelope and the endpoint's output.
// operations/check reports differences in output.success while the job itself
// succeeds, so callers must decode its output rather than trust job.success.
func (c *Client) verificationJob(ctx context.Context, endpoint string, params map[string]string, maxBytes int64) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes < 0 {
		return nil, errors.New("verification requires a non-negative byte limit")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	params["_async"] = "true"
	params["_config"] = verificationConfig(ctx, maxBytes)
	if _, ok := params["_filter"]; !ok {
		params["_filter"] = verificationFilter("")
	}
	var started struct {
		JobID int64 `json:"jobid"`
	}
	if err := c.callContext(ctx, endpoint, params, &started); err != nil {
		if notStarted(err) {
			return nil, errors.Join(errors.New("could not start the verification job"), err)
		}
		// A lost start response can hide an already running job's ID.
		return nil, errors.Join(errors.New("could not start the verification job"), ctx.Err(), ErrVerificationJobUncertain)
	}
	if started.JobID <= 0 {
		return nil, errors.Join(errors.New("rclone did not return a verification job ID"), ErrVerificationJobUncertain)
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		var status JobStatus
		err := c.callContext(ctx, "/job/status", map[string]string{"jobid": strconv.FormatInt(started.JobID, 10)}, &status)
		if err != nil {
			return nil, errors.Join(errors.New("could not read the verification job status"), ctx.Err(), c.stopVerificationJob(started.JobID))
		}
		if status.Finished {
			if !status.Success {
				return nil, errors.New("rclone verification job failed")
			}
			return status.Output, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), c.stopVerificationJob(started.JobID))
		case <-ticker.C:
		}
	}
}

func (c *Client) stopVerificationJob(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), verificationStopTimeout)
	defer cancel()
	params := map[string]string{"jobid": strconv.FormatInt(id, 10)}
	if err := c.callContext(ctx, "/job/stop", params, nil); err != nil {
		return errors.Join(ErrVerificationJobUncertain, ctx.Err())
	}
	// job/stop acknowledges cancellation, not completion. Wait for finished
	// before permitting the staging directory to be removed.
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		var status JobStatus
		if err := c.callContext(ctx, "/job/status", params, &status); err != nil {
			return errors.Join(ErrVerificationJobUncertain, ctx.Err())
		}
		if status.Finished {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrVerificationJobUncertain, ctx.Err())
		case <-ticker.C:
		}
	}
}

// InspectBackupPath refuses missing objects, type mismatches and unknown sizes.
// Empty directories must actually exist; an absent prefix is not an empty backup.
func (c *Client) InspectBackupPath(ctx context.Context, destination, remotePath string, directory bool) (VerificationSize, error) {
	var stat struct {
		Item *struct {
			Size  *int64
			IsDir *bool
		} `json:"item"`
	}
	out, err := c.verificationJob(ctx, "/operations/stat", map[string]string{
		"fs": Destination(destination, ""), "remote": remotePath,
	}, 0)
	if err != nil {
		return VerificationSize{}, err
	}
	if err := json.Unmarshal(out, &stat); err != nil {
		return VerificationSize{}, errors.New("rclone returned an unreadable object stat")
	}
	if stat.Item == nil {
		return VerificationSize{}, errors.New("the backup object is missing")
	}
	if stat.Item.IsDir == nil || *stat.Item.IsDir != directory {
		return VerificationSize{}, errors.New("the backup object has a different type from its manifest")
	}
	if !directory {
		if stat.Item.Size == nil || *stat.Item.Size < 0 {
			return VerificationSize{}, errors.New("the backup file has an unknown size")
		}
		return VerificationSize{Bytes: *stat.Item.Size, Files: 1}, nil
	}
	out, err = c.verificationJob(ctx, "/operations/size", map[string]string{"fs": Destination(destination, remotePath)}, 0)
	if err != nil {
		return VerificationSize{}, err
	}
	var size struct {
		Bytes    *int64 `json:"bytes"`
		Count    *int64 `json:"count"`
		Sizeless *int64 `json:"sizeless"`
	}
	if err := json.Unmarshal(out, &size); err != nil || size.Bytes == nil || size.Count == nil || size.Sizeless == nil || *size.Bytes < 0 || *size.Count < 0 || *size.Sizeless != 0 {
		return VerificationSize{}, errors.New("the backup directory size could not be bounded")
	}
	return VerificationSize{Bytes: *size.Bytes, Files: *size.Count}, nil
}

func (c *Client) StagingFreeSpace(ctx context.Context, stagingDir string) (int64, error) {
	out, err := c.verificationJob(ctx, "/operations/about", map[string]string{"fs": verificationLocal(stagingDir)}, 0)
	if err != nil {
		return 0, err
	}
	var usage struct {
		Free *int64 `json:"free"`
	}
	if err := json.Unmarshal(out, &usage); err != nil || usage.Free == nil || *usage.Free < 0 {
		return 0, errors.New("the staging filesystem did not report its free space")
	}
	return *usage.Free, nil
}

func (c *Client) copyVerificationPath(ctx context.Context, destination, remotePath, stagingPath string, directory bool, maxBytes int64) error {
	endpoint := "/operations/copyfile"
	params := map[string]string{
		"srcFs": Destination(destination, path.Dir(remotePath)), "srcRemote": path.Base(remotePath),
		"dstFs": verificationLocal(filepath.Dir(stagingPath)), "dstRemote": filepath.Base(stagingPath),
	}
	if directory {
		endpoint = "/sync/copy"
		params = map[string]string{"srcFs": Destination(destination, remotePath), "dstFs": verificationLocal(stagingPath), "createEmptySrcDirs": "true"}
	}
	_, err := c.verificationJob(ctx, endpoint, params, maxBytes)
	return err
}

// FetchLimited bounds both transfer and the final local read. It never follows
// a symlink and retains its private directory if a job might still write there.
func (c *Client) FetchLimited(ctx context.Context, destination, remotePath string, maxBytes int64) (data []byte, fetchErr error) {
	if maxBytes < 0 || maxBytes == math.MaxInt64 {
		return nil, errors.New("invalid fetch byte limit")
	}
	size, err := c.InspectBackupPath(ctx, destination, remotePath, false)
	if err != nil {
		return nil, err
	}
	if size.Bytes > maxBytes {
		return nil, errors.New("the backup metadata exceeds its byte limit")
	}
	dir, err := os.MkdirTemp("", "casaos-verify-metadata-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if !errors.Is(fetchErr, ErrVerificationJobUncertain) {
			_ = os.RemoveAll(dir)
		}
	}()
	local := filepath.Join(dir, "metadata.json")
	if err := c.copyVerificationPath(ctx, destination, remotePath, local, false, maxBytes); err != nil {
		return nil, err
	}
	info, err := os.Lstat(local)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("the downloaded metadata is not a regular file")
	}
	file, err := os.Open(local)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes || int64(len(data)) != size.Bytes {
		return nil, errors.New("the downloaded metadata size changed or exceeded its limit")
	}
	return data, nil
}

// VerifyBackupPath copies into a caller-owned private staging path and compares
// all bytes with a second remote read. This proves current readability, not an
// application's ability to start or equality with unrecorded historical hashes.
func (c *Client) VerifyBackupPath(ctx context.Context, destination, remotePath, stagingPath string, directory bool, maxBytes int64) (verifyErr error) {
	if maxBytes < 0 || maxBytes > math.MaxInt64/2 {
		return errors.New("invalid verification byte limit")
	}
	if !directory && (filepath.Base(stagingPath) != path.Base(remotePath) || strings.ContainsAny(path.Base(remotePath), "\r\n")) {
		return errors.New("the verification file must keep its remote basename without line breaks")
	}
	if err := c.copyVerificationPath(ctx, destination, remotePath, stagingPath, directory, maxBytes); err != nil {
		return err
	}
	params := map[string]string{
		"srcFs": Destination(destination, remotePath), "dstFs": verificationLocal(stagingPath),
		"download": "true", "oneWay": "false", "combined": "false", "match": "false",
		"missingOnSrc": "false", "missingOnDst": "false", "differ": "false", "error": "false",
	}
	if !directory {
		// FilesFromRaw is an exact name list, not a glob. The same basename on
		// each side allows checking one file without reading its siblings.
		filter, err := os.CreateTemp(filepath.Dir(stagingPath), ".verify-files-")
		if err != nil {
			return err
		}
		defer func() {
			if !errors.Is(verifyErr, ErrVerificationJobUncertain) {
				_ = os.Remove(filter.Name())
			}
		}()
		_, writeErr := io.WriteString(filter, path.Base(remotePath)+"\n")
		closeErr := filter.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
		params["srcFs"] = Destination(destination, path.Dir(remotePath))
		params["dstFs"] = verificationLocal(filepath.Dir(stagingPath))
		params["_filter"] = verificationFilter(filepath.ToSlash(filter.Name()))
	}
	out, err := c.verificationJob(ctx, "/operations/check", params, maxBytes*2)
	if err != nil {
		return err
	}
	var check struct {
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal(out, &check); err != nil || check.Success == nil || !*check.Success {
		return errors.New("the downloaded backup did not pass its content comparison")
	}
	return nil
}
