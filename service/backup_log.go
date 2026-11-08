package service

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// What happened, every time a backup ran.
//
// A scheduled backup nobody watches is only worth having if it can be asked
// afterwards how it went. This is that record -- including, and especially, the
// runs that failed: a feature that logs only its successes is how a box goes six
// months backing nothing up while its screen says everything is fine.

// BackupRunRecord is one run, finished or failed.
type BackupRunRecord struct {
	App         string    `json:"app"`
	Destination string    `json:"destination"`
	Stamp       string    `json:"stamp"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	// Scheduled tells a run that went off by itself from one somebody asked for.
	Scheduled bool `json:"scheduled"`
	// ContainersStopped says whether the app was held still, which decides whether
	// anything holding a database can be trusted to restore.
	ContainersStopped bool `json:"containers_stopped"`
	Copied            int  `json:"copied"`
	// Bytes is the size of what was read for the copy, from the manifest; 0 for a
	// restore, and for a run too old to have measured it.
	Bytes        int64 `json:"bytes"`
	SkippedCount int   `json:"skipped_count"`
	// Error is empty when the run succeeded.
	Error string `json:"error,omitempty"`
	// Restore marks a run that put the app back from a backup rather than one
	// that took a backup. Same log, because a person looking for what happened
	// to an app wants both in one place, newest first.
	Restore bool `json:"restore,omitempty"`
}

// Succeeded reports whether this run finished.
func (r BackupRunRecord) Succeeded() bool { return r.Error == "" }

// backupLogKept is how many runs are remembered. Enough to see a pattern -- a
// destination that has been failing every night for a fortnight -- without the
// file growing without end.
const backupLogKept = 200

// BackupLogPath is where the record lives. A variable so a test can put it
// somewhere of its own.
var BackupLogPath = "/var/lib/casaos/backups/runs.json"

var backupLogLock sync.Mutex

// RecordBackupRun appends one run and keeps the file to its last few hundred.
//
// A record that cannot be written is an error the caller hears about rather than
// a silence: losing the run is bad, and not knowing the log is broken is worse.
func RecordBackupRun(record BackupRunRecord) error {
	backupLogLock.Lock()
	defer backupLogLock.Unlock()

	records, err := readBackupLog()
	if err != nil {
		return err
	}

	records = append(records, record)
	if len(records) > backupLogKept {
		records = records[len(records)-backupLogKept:]
	}

	encoded, err := stdjson.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(BackupLogPath), 0o700); err != nil {
		return err
	}

	// Written beside and renamed: a log truncated by a power cut half-way through
	// a write is a log nobody can read at all, which loses every run rather than
	// the one being added.
	temp := BackupLogPath + ".tmp"
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}

	return os.Rename(temp, BackupLogPath)
}

// BackupRuns is what happened, newest first.
func BackupRuns() ([]BackupRunRecord, error) {
	backupLogLock.Lock()
	defer backupLogLock.Unlock()

	records, err := readBackupLog()
	if err != nil {
		return nil, err
	}

	sort.SliceStable(records, func(i, j int) bool {
		return records[i].StartedAt.After(records[j].StartedAt)
	})

	return records, nil
}

// readBackupLog reads the file, treating "not there yet" as "nothing has run".
//
// A file that will not parse is NOT treated that way. Quietly starting over would
// throw away every past run at the first bad byte, and the person who most needs
// this record is the one whose box has been misbehaving.
func readBackupLog() ([]BackupRunRecord, error) {
	content, err := os.ReadFile(BackupLogPath)
	if os.IsNotExist(err) {
		return []BackupRunRecord{}, nil
	}
	if err != nil {
		return nil, err
	}

	if len(content) == 0 {
		return []BackupRunRecord{}, nil
	}

	var records []BackupRunRecord
	if err := stdjson.Unmarshal(content, &records); err != nil {
		return nil, err
	}

	return records, nil
}
