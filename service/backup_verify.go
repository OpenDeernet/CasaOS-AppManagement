package service

import (
	"bytes"
	"context"
	"crypto/rand"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/restic"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

const (
	verificationMaxBytes      = int64(1 << 30)
	verificationManifestBytes = int64(1 << 20)
	verificationMaxOperations = 1024
	verificationMaxFiles      = int64(100000)
	verificationReserveBytes  = int64(64 << 20)
)

var (
	ErrBackupVerificationInvalid    = errors.New("invalid backup verification request")
	ErrBackupVerificationBusy       = errors.New("a backup verification is already running")
	ErrBackupVerificationNotFound   = errors.New("backup verification not found")
	ErrBackupVerificationNotRunning = errors.New("backup verification is not running")
	ErrBackupVerificationRestic     = errors.New("this destination keeps its backups with restic: use its repository check")
	// not busy: nothing runs, and it lasts until the workspace is reclaimed (verificationStaleAfter)
	ErrBackupVerificationNeedsRecovery = errors.New("a previous verification transfer could not be confirmed stopped; its temporary data needs recovery")
	BackupVerificationLogPath          = "/var/lib/casaos/backups/verifications.json"
	backupVerificationTasks            backupVerificationManager
	// replaced by tests; a destination kept with restic has its own repository check
	resticHeld = func(name string) bool { return restic.New().Held(name) }
)

// verificationTooBig is a verdict, not a failure: the backup is bigger than what a
// verification may read back, so nothing was retrieved.
type verificationTooBig struct{ message string }

func (e verificationTooBig) Error() string { return e.message }

type BackupVerificationOptions struct {
	Destination string
	App         string
	Stamp       string
}

// Passed means these objects were retrieved and compared with the remote during
// this run. Historical manifests contain no hashes of the original app data.
type BackupVerificationRecord struct {
	ID                string     `json:"id"`
	Destination       string     `json:"destination"`
	App               string     `json:"app"`
	Stamp             string     `json:"stamp"`
	Status            string     `json:"status"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	CheckedOperations int        `json:"checked_operations"`
	SkippedCount      int        `json:"skipped_count"`
	Files             int64      `json:"files"`
	Bytes             int64      `json:"bytes"`
	Error             string     `json:"error,omitempty"`
}

type BackupVerificationClient interface {
	FetchLimited(context.Context, string, string, int64) ([]byte, error)
	InspectBackupPath(context.Context, string, string, bool) (rclone.VerificationSize, error)
	StagingFreeSpace(context.Context, string) (int64, error)
	VerifyBackupPath(context.Context, string, string, string, bool, int64) error
}

type backupVerificationManager struct {
	mu       sync.Mutex
	logPath  string
	loaded   bool
	records  []BackupVerificationRecord
	activeID string
	cancel   context.CancelFunc
	done     chan struct{}
}

func StartBackupVerification(opts BackupVerificationOptions, client BackupVerificationClient) (BackupVerificationRecord, error) {
	return backupVerificationTasks.start(BackupVerificationLogPath, opts, client)
}

func BackupVerifications() ([]BackupVerificationRecord, error) {
	return backupVerificationTasks.list(BackupVerificationLogPath)
}

func CancelBackupVerification(id string) error {
	return backupVerificationTasks.cancelRun(BackupVerificationLogPath, id)
}

func verificationIdentifier(value string) bool {
	return value != "" && len(value) <= 128 && value != "." && value != ".." && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "/\\:") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func (m *backupVerificationManager) start(logPath string, opts BackupVerificationOptions, client BackupVerificationClient) (BackupVerificationRecord, error) {
	// "name,option=value:" overrides a remote's config in rclone (connection strings),
	// e.g. repointing an alias at local files, so a destination name has no comma.
	if !verificationIdentifier(opts.Destination) || strings.ContainsAny(opts.Destination, ",=") || strings.HasSuffix(opts.Destination, rclone.RawSuffix) || !verificationIdentifier(opts.App) || !verificationIdentifier(opts.Stamp) || client == nil {
		return BackupVerificationRecord{}, ErrBackupVerificationInvalid
	}
	if resticHeld(opts.Destination) {
		return BackupVerificationRecord{}, ErrBackupVerificationRestic
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(logPath); err != nil {
		return BackupVerificationRecord{}, err
	}
	if m.activeID != "" {
		return BackupVerificationRecord{}, ErrBackupVerificationBusy
	}
	if err := verificationWorkspaceAvailable(logPath, m.records); err != nil {
		return BackupVerificationRecord{}, err
	}
	record := BackupVerificationRecord{
		ID: rand.Text(), Destination: opts.Destination, App: opts.App, Stamp: opts.Stamp,
		Status: "running", StartedAt: time.Now().UTC(),
	}
	records := append(append([]BackupVerificationRecord{}, m.records...), record)
	if len(records) > backupLogKept {
		records = records[len(records)-backupLogKept:]
	}
	if err := writeVerificationLog(logPath, records); err != nil {
		return BackupVerificationRecord{}, err // never start an unrecorded job
	}
	m.records = records
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	m.activeID, m.cancel, m.done = record.ID, cancel, make(chan struct{})
	// listed before start returns, so whoever asks what runs on the app next sees it
	unmark := markInProgress(record.App, "verification")
	go m.run(ctx, client, record, m.done, unmark)
	return record, nil
}

func (m *backupVerificationManager) list(logPath string) ([]BackupVerificationRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(logPath); err != nil {
		return nil, err
	}
	out := make([]BackupVerificationRecord, len(m.records))
	for i, record := range m.records {
		out[len(out)-1-i] = record
	}
	return out, nil
}

func (m *backupVerificationManager) cancelRun(logPath, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(logPath); err != nil {
		return err
	}
	for _, record := range m.records {
		if record.ID != id {
			continue
		}
		if record.Status != "running" || m.activeID != id || m.cancel == nil {
			return ErrBackupVerificationNotRunning
		}
		m.cancel() // status remains running until the transfer has really stopped
		return nil
	}
	return ErrBackupVerificationNotFound
}

func (m *backupVerificationManager) loadLocked(logPath string) error {
	if m.loaded && m.logPath == logPath {
		return nil
	}
	if m.activeID != "" {
		return ErrBackupVerificationBusy
	}
	raw, err := os.ReadFile(logPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("backup verification history could not be read")
	}
	records := []BackupVerificationRecord{}
	if len(raw) > 0 {
		if err := stdjson.Unmarshal(raw, &records); err != nil {
			return errors.New("backup verification history is invalid; it was left untouched")
		}
	}
	changed := false
	if len(records) > backupLogKept {
		records = records[len(records)-backupLogKept:]
		changed = true
	}
	for i := range records {
		if records[i].Status == "running" {
			now := time.Now().UTC()
			records[i].Status, records[i].FinishedAt = "interrupted", &now
			records[i].Error = "verification was interrupted; any private temporary data was retained"
			changed = true
		}
	}
	if changed {
		if err := writeVerificationLog(logPath, records); err != nil {
			return err
		}
	}
	m.records, m.logPath, m.loaded = records, logPath, true
	return nil
}

func writeVerificationLog(filename string, records []BackupVerificationRecord) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("backup verification history could not be saved")
	}
	file, err := os.CreateTemp(dir, ".verification-log-")
	if err != nil {
		return errors.New("backup verification history could not be saved")
	}
	defer os.Remove(file.Name())
	err = stdjson.NewEncoder(file).Encode(records)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err == nil {
		err = os.Rename(file.Name(), filename)
	}
	if err != nil {
		return errors.New("backup verification history could not be saved")
	}
	return nil
}

func (m *backupVerificationManager) saveRecord(record BackupVerificationRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.records {
		if m.records[i].ID == record.ID {
			m.records[i] = record
			break
		}
	}
	return writeVerificationLog(m.logPath, m.records)
}

// verificationFailure keeps the cause for cancellation/cleanup decisions, but
// its text never exposes backend credentials, object contents or raw RC errors.
type verificationFailure struct {
	stage string
	cause error
}

func (e verificationFailure) Error() string { return e.stage }
func (e verificationFailure) Unwrap() error { return e.cause }

func (m *backupVerificationManager) run(ctx context.Context, client BackupVerificationClient, record BackupVerificationRecord, done chan struct{}, unmark func()) {
	defer close(done)
	defer unmark()
	err := m.verify(ctx, client, &record)
	if failure := (verificationFailure{}); errors.As(err, &failure) && failure.cause != nil {
		// the record keeps only the stage; the cause is for whoever reads the logs
		logger.Error("backup verification failed", zap.String("id", record.ID), zap.String("stage", failure.stage), zap.Error(failure.cause))
	}
	now := time.Now().UTC()
	record.FinishedAt = &now
	tooBig := verificationTooBig{}
	switch {
	case err == nil: // everything was read and compared, whatever arrived since
		record.Status = "passed"
	case errors.As(err, &tooBig):
		record.Status, record.Error = "not_verifiable", tooBig.message
	case errors.Is(err, rclone.ErrVerificationJobUncertain):
		record.Status, record.Error = "failed", "transfer stop could not be confirmed; private verification data was retained"
	case errors.Is(ctx.Err(), context.Canceled):
		record.Status, record.Error = "cancelled", "verification was cancelled; "+err.Error()
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		record.Status, record.Error = "failed", "verification exceeded the one-hour limit; "+err.Error()
	default:
		record.Status, record.Error = "failed", err.Error()
	}
	if saveErr := m.saveRecord(record); saveErr != nil {
		record.Status = "failed"
		if record.Error != "" {
			record.Error += "; "
		}
		record.Error += "verification result could not be saved"
		// Keep both failures visible even when persistence is unavailable.
		m.mu.Lock()
		for i := range m.records {
			if m.records[i].ID == record.ID {
				m.records[i] = record
			}
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.cancel()
	m.activeID, m.cancel = "", nil
	m.mu.Unlock()
}

func (m *backupVerificationManager) verify(ctx context.Context, client BackupVerificationClient, record *BackupVerificationRecord) (runErr error) {
	staging, err := createVerificationWorkspace(m.logPath, record.ID)
	if err != nil {
		return err
	}
	defer func() {
		if !errors.Is(runErr, rclone.ErrVerificationJobUncertain) {
			if err := os.RemoveAll(staging); err != nil {
				runErr = errors.Join(runErr, errors.New("private verification workspace could not be cleaned up"))
			}
		}
	}()
	root := path.Join(record.App, record.Stamp) // identifiers validated without normalization
	raw, err := client.FetchLimited(ctx, record.Destination, path.Join(root, ManifestFileName), verificationManifestBytes)
	if err != nil {
		return verificationFailure{"backup manifest could not be retrieved", err}
	}
	operations, skipped, err := verificationPlan(raw, record.App)
	if err != nil {
		return err
	}
	record.SkippedCount = skipped
	if err := m.saveRecord(*record); err != nil {
		return err
	}
	sizes := make([]rclone.VerificationSize, len(operations))
	var total, files int64
	for i, op := range operations {
		if err := ctx.Err(); err != nil {
			return verificationFailure{"verification was interrupted", err}
		}
		size, err := client.InspectBackupPath(ctx, record.Destination, path.Join(root, op.Destination), op.Directory)
		if err != nil {
			return verificationFailure{"a backup object could not be inspected", err}
		}
		if size.Bytes < 0 || size.Files < 0 { // unmeasurable is a failure, not "too big"
			return errors.New("backup size is unknown")
		}
		if size.Bytes > verificationMaxBytes-total {
			return verificationTooBig{fmt.Sprintf("backup is bigger than the %d-byte verification limit", verificationMaxBytes)}
		}
		if size.Files > verificationMaxFiles-files {
			return verificationTooBig{fmt.Sprintf("backup has more files than the %d-file verification limit", verificationMaxFiles)}
		}
		total += size.Bytes
		files += size.Files
		sizes[i] = size
	}
	free, err := client.StagingFreeSpace(ctx, staging)
	if err != nil {
		return verificationFailure{"free space for verification could not be checked", err}
	}
	if free < total+verificationReserveBytes {
		return errors.New("insufficient free space for verification and its safety reserve")
	}
	for i, op := range operations {
		if err := ctx.Err(); err != nil {
			return verificationFailure{"verification was interrupted", err}
		}
		free, err := client.StagingFreeSpace(ctx, staging)
		if err != nil {
			return verificationFailure{"free space for verification could not be checked", err}
		}
		if free < sizes[i].Bytes+verificationReserveBytes {
			return errors.New("insufficient free space for verification and its safety reserve")
		}
		ordinal := filepath.Join(staging, strconv.Itoa(i))
		if err := os.Mkdir(ordinal, 0o700); err != nil {
			return errors.New("private verification workspace could not be created")
		}
		local := ordinal
		if !op.Directory {
			local = filepath.Join(ordinal, path.Base(op.Destination))
		}
		if err := client.VerifyBackupPath(ctx, record.Destination, path.Join(root, op.Destination), local, op.Directory, sizes[i].Bytes); err != nil {
			return verificationFailure{"a backup object could not be retrieved and compared", err}
		}
		actual, err := verificationLocalSize(ordinal, verificationMaxBytes-record.Bytes)
		if err != nil {
			return err
		}
		if actual != sizes[i] {
			return errors.New("backup object size or file count changed during verification")
		}
		record.CheckedOperations++
		record.Bytes += actual.Bytes
		record.Files += actual.Files
		if err := m.saveRecord(*record); err != nil {
			return err
		}
	}
	current, err := client.FetchLimited(ctx, record.Destination, path.Join(root, ManifestFileName), verificationManifestBytes)
	if err != nil {
		return verificationFailure{"backup manifest could not be checked again", err}
	}
	if !bytes.Equal(raw, current) {
		return errors.New("backup manifest changed during verification")
	}
	return nil
}

func createVerificationWorkspace(logPath, id string) (string, error) {
	root := filepath.Join(filepath.Dir(logPath), "verification-work")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", errors.New("private verification workspace could not be created")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("verification workspace is not a private directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", errors.New("verification workspace permissions could not be secured")
	}
	dir, err := os.MkdirTemp(root, "verify-"+id+"-")
	if err != nil {
		return "", errors.New("private verification workspace could not be created")
	}
	return dir, nil
}

func verificationPlan(raw []byte, app string) ([]BackupOperation, int, error) {
	invalid := errors.New("backup manifest is invalid or has unsafe object paths")
	if len(raw) > int(verificationManifestBytes) {
		return nil, 0, invalid
	}
	var manifest BackupManifest
	if stdjson.Unmarshal(raw, &manifest) != nil || manifest.FormatVersion != BackupFormatVersion || manifest.App != app || len(manifest.Operations) == 0 || len(manifest.Operations) > verificationMaxOperations {
		return nil, 0, invalid
	}
	operations := []BackupOperation{}
	seen := map[string]BackupOperation{}
	for _, op := range manifest.Operations {
		if !fs.ValidPath(op.Destination) || len(op.Destination) > 4096 || strings.ContainsAny(op.Destination, "\\") || strings.IndexFunc(op.Destination, unicode.IsControl) >= 0 {
			return nil, 0, invalid
		}
		parts := strings.Split(op.Destination, "/")
		valid := false
		switch op.Kind {
		case BackupKindCompose, BackupKindEnv:
			valid = len(parts) == 2 && parts[0] == "compose" && !op.Directory
		case BackupKindVolume:
			valid = len(parts) == 2 && parts[0] == "volumes" && op.Directory
		case BackupKindBind:
			want := 2
			if !op.Directory {
				want = 3
			}
			if len(parts) == want && parts[0] == "binds" {
				n, err := strconv.Atoi(parts[1])
				valid = err == nil && n > 0
			}
		}
		if !valid {
			return nil, 0, invalid
		}
		if previous, duplicate := seen[op.Destination]; duplicate {
			if previous.Kind != op.Kind || previous.Directory != op.Directory {
				return nil, 0, invalid
			}
			continue // two services can share one named volume
		}
		seen[op.Destination] = op
		operations = append(operations, op)
	}
	return operations, len(manifest.Skipped), nil
}

// verificationStaleAfter is when a left workspace is reclaimed: a run's own
// deadline and each job's MaxDuration are one hour, its stop wait 30 seconds.
const verificationStaleAfter = 2 * time.Hour

// An old transfer may outlive this process. Without its confirmed terminal job
// state, neither deleting its workspace nor starting another bounded job is safe
// -- until the run that made it is long past every limit, or it would block for good.
func verificationWorkspaceAvailable(logPath string, records []BackupVerificationRecord) error {
	root := filepath.Join(filepath.Dir(logPath), "verification-work")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private verification workspace could not be inspected")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return errors.New("private verification workspace could not be inspected")
	}
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(entry.Name(), "verify-")
		if !ok {
			continue
		}
		started := time.Time{}
		if info, err := entry.Info(); err == nil {
			started = info.ModTime() // the history may no longer hold its run
		}
		id, _, _ := strings.Cut(rest, "-")
		for _, record := range records {
			if record.ID == id {
				started = record.StartedAt
			}
		}
		if !started.IsZero() && time.Since(started) > verificationStaleAfter && os.RemoveAll(filepath.Join(root, entry.Name())) == nil {
			continue
		}
		return ErrBackupVerificationNeedsRecovery
	}
	return nil
}

func verificationLocalSize(root string, limit int64) (rclone.VerificationSize, error) {
	var size rclone.VerificationSize
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return errors.New("retrieved verification data could not be inspected")
		}
		if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return errors.New("retrieved verification data contains a link or special file")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() < 0 || info.Size() > limit-size.Bytes {
			return errors.New("retrieved verification data exceeds its size limit or is unreadable")
		}
		size.Bytes += info.Size()
		size.Files++
		if size.Files > verificationMaxFiles {
			return errors.New("retrieved verification data exceeds the file-count limit")
		}
		return nil
	})
	return size, err
}
