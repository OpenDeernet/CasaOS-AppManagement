package service

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
)

type fakeVerificationClient struct {
	manifest   []byte
	size       rclone.VerificationSize
	free       int64
	verify     func(context.Context, string, bool) error
	inspectErr error
	fetches    int
	change     bool
	paths      []string
	limits     []int64
}

func (f *fakeVerificationClient) FetchLimited(_ context.Context, _, _ string, limit int64) ([]byte, error) {
	f.fetches++
	if int64(len(f.manifest)) > limit {
		return nil, errors.New("too large")
	}
	if f.change && f.fetches > 1 {
		return append(append([]byte{}, f.manifest...), ' '), nil
	}
	return f.manifest, nil
}

func (f *fakeVerificationClient) InspectBackupPath(context.Context, string, string, bool) (rclone.VerificationSize, error) {
	return f.size, f.inspectErr
}

func (f *fakeVerificationClient) StagingFreeSpace(context.Context, string) (int64, error) {
	return f.free, nil
}

func (f *fakeVerificationClient) VerifyBackupPath(ctx context.Context, _, _, local string, directory bool, limit int64) error {
	f.paths = append(f.paths, local)
	f.limits = append(f.limits, limit)
	if f.verify != nil {
		return f.verify(ctx, local, directory)
	}
	if limit < 4 {
		return errors.New("limit")
	}
	if directory {
		local = filepath.Join(local, "data")
	}
	return os.WriteFile(local, []byte("data"), 0o600)
}

func verificationFixture(t *testing.T) (*backupVerificationManager, string, BackupVerificationOptions, *fakeVerificationClient) {
	t.Helper()
	raw, err := stdjson.Marshal(BackupManifest{
		FormatVersion: BackupFormatVersion, App: "demo",
		Operations: []BackupOperation{{Kind: BackupKindBind, Source: "/production/never-write-here", Destination: "binds/1", Directory: true}},
		Skipped:    []BackupEntry{{Skip: "kernel interface"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &backupVerificationManager{}, filepath.Join(t.TempDir(), "verifications.json"),
		BackupVerificationOptions{Destination: "nas", App: "demo", Stamp: "2026-10-04"},
		&fakeVerificationClient{manifest: raw, size: rclone.VerificationSize{Bytes: 4, Files: 1}, free: 2 * verificationMaxBytes}
}

func awaitVerification(t *testing.T, m *backupVerificationManager, logPath string) BackupVerificationRecord {
	t.Helper()
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("verification did not finish")
	}
	records, err := m.list(logPath)
	if err != nil || len(records) == 0 {
		t.Fatalf("missing verification history: %+v %v", records, err)
	}
	return records[0]
}

func TestBackupVerificationChecksDataPrivatelyAndCleansUp(t *testing.T) {
	m, logPath, opts, client := verificationFixture(t)
	if _, err := m.start(logPath, opts, client); err != nil {
		t.Fatal(err)
	}
	record := awaitVerification(t, m, logPath)
	if record.Status != "passed" || record.CheckedOperations != 1 || record.SkippedCount != 1 || record.Bytes != 4 || record.Files != 1 || record.FinishedAt == nil {
		t.Fatalf("unexpected verification: %+v", record)
	}
	if len(client.paths) != 1 || !strings.HasPrefix(client.paths[0], filepath.Dir(logPath)+string(os.PathSeparator)) {
		t.Fatalf("copy escaped the private workspace: %v", client.paths)
	}
	if len(client.limits) != 1 || client.limits[0] != client.size.Bytes {
		t.Fatalf("copy must be capped at its measured size, not the remaining overall allowance: %v", client.limits)
	}
	if _, err := os.Stat(client.paths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary data was not cleaned: %v", err)
	}
	var reloaded backupVerificationManager
	records, err := reloaded.list(logPath)
	if err != nil || len(records) != 1 || records[0].Status != "passed" {
		t.Fatalf("completed result was not persisted: %+v %v", records, err)
	}
}

func TestBackupVerificationKeepsTheRemoteBasenameForSingleFiles(t *testing.T) {
	m, logPath, opts, client := verificationFixture(t)
	raw, err := stdjson.Marshal(BackupManifest{FormatVersion: BackupFormatVersion, App: opts.App,
		Operations: []BackupOperation{{Kind: BackupKindBind, Source: "/etc/unrelated", Destination: "binds/1/app.conf"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.manifest = raw
	if _, err := m.start(logPath, opts, client); err != nil {
		t.Fatal(err)
	}
	record := awaitVerification(t, m, logPath)
	if record.Status != "passed" || len(client.paths) != 1 || filepath.Base(client.paths[0]) != "app.conf" {
		t.Fatalf("file should be compared under its original basename, privately: %+v %v", record, client.paths)
	}
}

func TestBackupVerificationRejectsInvalidIdentifiersBeforeStarting(t *testing.T) {
	for _, value := range []string{"", ".", "..", "../nas", "nas/path", "nas\\path", "nas:other", " nas", "nas\x00", "nas,remote=etc", "nas,password=x"} {
		t.Run(value, func(t *testing.T) {
			m, logPath, opts, client := verificationFixture(t)
			opts.Destination = value
			if _, err := m.start(logPath, opts, client); !errors.Is(err, ErrBackupVerificationInvalid) {
				t.Fatalf("invalid destination accepted: %v", err)
			}
			if client.fetches != 0 {
				t.Fatal("invalid request touched the remote")
			}
		})
	}
}

func TestBackupVerificationCancellationWaitsForTheTransferToStop(t *testing.T) {
	m, logPath, opts, client := verificationFixture(t)
	entered, cancelled, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	client.verify = func(ctx context.Context, _ string, _ bool) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-stopped
		return ctx.Err()
	}
	record, err := m.start(logPath, opts, client)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := m.cancelRun(logPath, record.ID); err != nil {
		t.Fatal(err)
	}
	<-cancelled
	records, err := m.list(logPath)
	if err != nil || records[0].Status != "running" {
		t.Fatalf("cancellation must not claim the transfer already stopped: %+v %v", records, err)
	}
	if _, err := m.start(logPath, opts, client); !errors.Is(err, ErrBackupVerificationBusy) {
		t.Fatalf("second job should be refused until the first stops: %v", err)
	}
	close(stopped)
	if result := awaitVerification(t, m, logPath); result.Status != "cancelled" {
		t.Fatalf("unexpected cancellation result: %+v", result)
	}
	if _, err := os.Stat(client.paths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a cancelled verification must remove its workspace: %v", err)
	}
}

func TestBackupVerificationRetainsWorkspaceWhenStopIsUncertain(t *testing.T) {
	m, logPath, opts, client := verificationFixture(t)
	client.verify = func(context.Context, string, bool) error { return rclone.ErrVerificationJobUncertain }
	if _, err := m.start(logPath, opts, client); err != nil {
		t.Fatal(err)
	}
	record := awaitVerification(t, m, logPath)
	if record.Status != "failed" || !strings.Contains(record.Error, "retained") {
		t.Fatalf("uncertain transfer should be visible: %+v", record)
	}
	if _, err := os.Stat(client.paths[0]); err != nil {
		t.Fatalf("workspace removed while the daemon may still write: %v", err)
	}
	if _, err := m.start(logPath, opts, client); !errors.Is(err, ErrBackupVerificationNeedsRecovery) {
		t.Fatalf("an unconfirmed old transfer must block a new job: %v", err)
	}
	var restarted backupVerificationManager
	if _, err := restarted.start(logPath, opts, client); !errors.Is(err, ErrBackupVerificationNeedsRecovery) {
		t.Fatalf("restarting the service must not erase the uncertainty: %v", err)
	}
}

func TestBackupVerificationDoesNotReportSuccessWhenHistoryCannotBeWritten(t *testing.T) {
	m, logPath, opts, client := verificationFixture(t)
	client.verify = func(_ context.Context, local string, _ bool) error {
		if err := os.WriteFile(filepath.Join(local, "data"), []byte("data"), 0o600); err != nil {
			return err
		}
		if err := os.Remove(logPath); err != nil {
			return err
		}
		return os.Mkdir(logPath, 0o700) // make the atomic replacement fail
	}
	if _, err := m.start(logPath, opts, client); err != nil {
		t.Fatal(err)
	}
	record := awaitVerification(t, m, logPath)
	if record.Status != "failed" || !strings.Contains(record.Error, "could not be saved") {
		t.Fatalf("a missing durable result must not be presented as success: %+v", record)
	}
}

func TestBackupVerificationHistoryIsBounded(t *testing.T) {
	m, logPath, _, _ := verificationFixture(t)
	records := make([]BackupVerificationRecord, backupLogKept+1)
	for i := range records {
		records[i].Status = "passed"
	}
	records[0].ID = "discarded"
	if err := writeVerificationLog(logPath, records); err != nil {
		t.Fatal(err)
	}
	loaded, err := m.list(logPath)
	if err != nil || len(loaded) != backupLogKept {
		t.Fatalf("history was not bounded: records=%d err=%v", len(loaded), err)
	}
	for _, record := range loaded {
		if record.ID == "discarded" {
			t.Fatal("oldest result was retained beyond the history limit")
		}
	}
}

func TestBackupVerificationRejectsFailuresWithoutExposingBackendSecrets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeVerificationClient)
	}{
		{"no free space", func(f *fakeVerificationClient) { f.free = 0 }},
		{"unknown size", func(f *fakeVerificationClient) { f.size.Bytes = -1 }},
		{"changed manifest", func(f *fakeVerificationClient) { f.change = true }},
		{"changed contents", func(f *fakeVerificationClient) { f.size.Bytes = 9 }},
		{"backend secret", func(f *fakeVerificationClient) { f.inspectErr = errors.New("https://cloud/?token=never-return-this") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, logPath, opts, client := verificationFixture(t)
			tc.change(client)
			if _, err := m.start(logPath, opts, client); err != nil {
				t.Fatal(err)
			}
			record := awaitVerification(t, m, logPath)
			if record.Status != "failed" || record.Error == "" || strings.Contains(record.Error, "never-return-this") {
				t.Fatalf("unsafe failure result: %+v", record)
			}
		})
	}
}

func TestBackupVerificationMarksInterruptedHistoryAndPreservesCorruption(t *testing.T) {
	m, logPath, _, _ := verificationFixture(t)
	if err := writeVerificationLog(logPath, []BackupVerificationRecord{{ID: "old", Status: "running"}}); err != nil {
		t.Fatal(err)
	}
	records, err := m.list(logPath)
	if err != nil || len(records) != 1 || records[0].Status != "interrupted" {
		t.Fatalf("old running job must not become a success: %+v %v", records, err)
	}
	if err := os.WriteFile(logPath, []byte("broken history"), 0o600); err != nil {
		t.Fatal(err)
	}
	var restarted backupVerificationManager
	if _, err := restarted.list(logPath); err == nil {
		t.Fatal("corrupt history must not be silently replaced")
	}
	raw, err := os.ReadFile(logPath)
	if err != nil || string(raw) != "broken history" {
		t.Fatalf("corrupt file was altered: %q %v", raw, err)
	}
}

func TestBackupVerificationRejectsUnsafeManifestPaths(t *testing.T) {
	for _, destination := range []string{"../outside", "/binds/1", "binds/../outside", "binds/1\\outside", "binds/0", "binds/1\x00", "other/1"} {
		raw, err := stdjson.Marshal(BackupManifest{FormatVersion: BackupFormatVersion, App: "demo", Operations: []BackupOperation{{Kind: BackupKindBind, Destination: destination, Directory: true}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := verificationPlan(raw, "demo"); err == nil {
			t.Fatalf("unsafe manifest accepted: %q", destination)
		}
	}
}

func TestBackupVerificationRefusesLocalSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := verificationLocalSize(dir, verificationMaxBytes); err == nil {
		t.Fatal("a retrieved symlink must not be followed")
	}
}

// A crash leaves its workspace behind, and the transfer it was running may still
// be writing, so a recent one blocks. Every run ends within the hour, though, so
// an old one is reclaimed rather than blocking verification for good.
func TestBackupVerificationReclaimsAWorkspaceLeftLongAgo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		started time.Duration
		blocked bool
	}{{"recent", -10 * time.Minute, true}, {"long ago", -3 * time.Hour, false}} {
		t.Run(tc.name, func(t *testing.T) {
			m, logPath, opts, client := verificationFixture(t)
			if err := writeVerificationLog(logPath, []BackupVerificationRecord{{ID: "OLD", Status: "running", StartedAt: time.Now().Add(tc.started)}}); err != nil {
				t.Fatal(err)
			}
			left := filepath.Join(filepath.Dir(logPath), "verification-work", "verify-OLD-123")
			if err := os.MkdirAll(filepath.Join(left, "0"), 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := m.start(logPath, opts, client)
			if tc.blocked {
				if !errors.Is(err, ErrBackupVerificationNeedsRecovery) {
					t.Fatalf("a recent workspace must block: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("an old workspace must not block for good: %v", err)
			}
			if record := awaitVerification(t, m, logPath); record.Status != "passed" {
				t.Fatalf("unexpected verification: %+v", record)
			}
			if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old workspace was not reclaimed: %v", err)
			}
		})
	}
}

// A cancellation that arrives once every object is read and compared changes
// nothing: the result is a pass, not a cancellation that hides one.
func TestBackupVerificationThatFinishedIsNotRecordedAsCancelled(t *testing.T) {
	m, logPath, opts, client := verificationFixture(t)
	client.verify = func(_ context.Context, local string, _ bool) error {
		m.mu.Lock()
		id := m.activeID
		m.mu.Unlock()
		if err := m.cancelRun(logPath, id); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(local, "data"), []byte("data"), 0o600)
	}
	if _, err := m.start(logPath, opts, client); err != nil {
		t.Fatal(err)
	}
	if record := awaitVerification(t, m, logPath); record.Status != "passed" {
		t.Fatalf("a finished check was hidden: %+v", record)
	}
}

// A daemon that is down fails the run, and that is all: nothing is kept and the
// next verification may start at once.
func TestBackupVerificationWithTheDaemonDownBlocksNothing(t *testing.T) {
	m, logPath, opts, client := verificationFixture(t)
	client.inspectErr = errors.New("the rclone daemon is not answering")
	if _, err := m.start(logPath, opts, client); err != nil {
		t.Fatal(err)
	}
	if record := awaitVerification(t, m, logPath); record.Status != "failed" || strings.Contains(record.Error, "retained") {
		t.Fatalf("unexpected verification: %+v", record)
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(logPath), "verification-work"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace kept: %v %v", entries, err)
	}
	client.inspectErr = nil
	if _, err := m.start(logPath, opts, client); err != nil {
		t.Fatalf("the next verification was blocked: %v", err)
	}
	awaitVerification(t, m, logPath)
}

func TestVerifyRefusesADestinationKeptWithRestic(t *testing.T) {
	m, logPath, opts, client := verificationFixture(t)
	defer func(old func(string) bool) { resticHeld = old }(resticHeld)
	var asked string
	resticHeld = func(name string) bool { asked = name; return true }
	if _, err := m.start(logPath, opts, client); !errors.Is(err, ErrBackupVerificationRestic) {
		t.Fatalf("a restic destination must be refused, got %v", err)
	}
	if asked != opts.Destination || client.fetches != 0 {
		t.Fatalf("wrong destination asked or backup touched: %q %d", asked, client.fetches)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nothing may be written for a refused verification: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(logPath), "verification")); err == nil {
		t.Fatal("no workspace may be created")
	}
	// the identifier check comes first
	opts.App = "../demo"
	if _, err := m.start(logPath, opts, client); !errors.Is(err, ErrBackupVerificationInvalid) {
		t.Fatalf("invalid identifiers come first, got %v", err)
	}
}

func TestBackupBiggerThanTheLimitIsNotVerifiable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeVerificationClient)
		limit  string
	}{
		{"bytes", func(f *fakeVerificationClient) { f.size.Bytes = verificationMaxBytes + 1 }, strconv.FormatInt(verificationMaxBytes, 10)},
		{"files", func(f *fakeVerificationClient) { f.size.Files = verificationMaxFiles + 1 }, strconv.FormatInt(verificationMaxFiles, 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, logPath, opts, client := verificationFixture(t)
			tc.change(client)
			if _, err := m.start(logPath, opts, client); err != nil {
				t.Fatal(err)
			}
			record := awaitVerification(t, m, logPath)
			if record.Status != "not_verifiable" || record.FinishedAt == nil || !strings.Contains(record.Error, tc.limit) {
				t.Fatalf("a too big backup is not verifiable, and says which limit: %+v", record)
			}
			if len(client.paths) != 0 {
				t.Fatal("nothing may be downloaded for a backup that is not verifiable")
			}
		})
	}
}

func TestAFailedReadStaysFailed(t *testing.T) {
	for name, change := range map[string]func(*fakeVerificationClient){
		"inspect":       func(f *fakeVerificationClient) { f.inspectErr = errors.New("remote is down") },
		"unknown size":  func(f *fakeVerificationClient) { f.size.Bytes = -1 },
		"unknown files": func(f *fakeVerificationClient) { f.size.Files = -1 },
		"retrieve": func(f *fakeVerificationClient) {
			f.verify = func(context.Context, string, bool) error { return errors.New("read failed") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, logPath, opts, client := verificationFixture(t)
			change(client)
			if _, err := m.start(logPath, opts, client); err != nil {
				t.Fatal(err)
			}
			if record := awaitVerification(t, m, logPath); record.Status != "failed" || record.Error == "" {
				t.Fatalf("a failed read is a failure, not a size verdict: %+v", record)
			}
		})
	}
}
