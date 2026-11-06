package logstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

func TestWritesLandInTheDayFileAndRollOverAtMidnight(t *testing.T) {
	root := t.TempDir()
	w := NewWriter(root, "jellyfin", "jellyfin", "0123456789abcdef", nil)
	assert.NilError(t, w.Write(time.Date(2026, 11, 5, 23, 59, 59, 0, time.UTC), "o", "a"))
	assert.NilError(t, w.Write(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC), "o", "b"))
	assert.NilError(t, w.Close())
	d5, _ := os.ReadFile(filepath.Join(root, "jellyfin/jellyfin/2026-11-05.0123456789ab.log"))
	d6, _ := os.ReadFile(filepath.Join(root, "jellyfin/jellyfin/2026-11-06.0123456789ab.log"))
	assert.Equal(t, string(d5), "2026-11-05T23:59:59Z o a\n")
	assert.Equal(t, string(d6), "2026-11-06T00:00:00Z o b\n")
}

func TestNothingIsOnDiskBeforeAFlush(t *testing.T) {
	root := t.TempDir()
	w := NewWriter(root, "a", "s", "0123456789ab", nil)
	defer w.Close()
	at := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	file := DayFile(root, "a", "s", "0123456789ab", at)
	assert.NilError(t, w.Write(at, "o", "hello"))
	b, _ := os.ReadFile(file)
	assert.Equal(t, len(b), 0)
	assert.NilError(t, w.Flush())
	b, _ = os.ReadFile(file)
	assert.Equal(t, string(b), "2026-11-05T01:02:03Z o hello\n")
}

func TestSixtyFourKiBFlushesByItself(t *testing.T) {
	root := t.TempDir()
	w := NewWriter(root, "a", "s", "0123456789ab", nil)
	defer w.Close()
	at := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	for i := 0; i < 65; i++ {
		assert.NilError(t, w.Write(at, "o", strings.Repeat("x", 1024)))
	}
	b, _ := os.ReadFile(DayFile(root, "a", "s", "0123456789ab", at))
	assert.Assert(t, len(b) > 0)
}

func TestLastTimestampIsTheNewestLineOfTheNewestDay(t *testing.T) {
	root := t.TempDir()
	_, ok := LastTimestamp(root, "a", "s", "0123456789ab")
	assert.Assert(t, !ok)

	dir := filepath.Join(root, "a", "s")
	assert.NilError(t, os.MkdirAll(dir, 0o755))
	// the older file has a later-looking text; only the day in the file name decides which is newest
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "2026-11-05.0123456789ab.log"),
		[]byte("2026-11-05T10:00:00Z o 2026-11-09T00:00:00Z o later\n"), 0o644))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "2026-11-06.0123456789ab.log"),
		[]byte("2026-11-06T01:00:00Z o x\n2026-11-06T02:00:00.5Z e y\ngarbage\n"), 0o644))
	got, ok := LastTimestamp(root, "a", "s", "0123456789abcdef")
	assert.Assert(t, ok)
	assert.Equal(t, got, time.Date(2026, 11, 6, 2, 0, 0, 500000000, time.UTC))
}

func TestOnLineSeesEveryWrite(t *testing.T) {
	var got []Line
	w := NewWriter(t.TempDir(), "a", "s", "0123456789abcdef", func(l Line) { got = append(got, l) })
	defer w.Close()
	at := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	assert.NilError(t, w.Write(at, "e", "one"))
	assert.NilError(t, w.Write(at, "o", "two"))
	assert.DeepEqual(t, got, []Line{
		{At: at, App: "a", Service: "s", Container: "0123456789ab", Stream: "e", Text: "one"},
		{At: at, App: "a", Service: "s", Container: "0123456789ab", Stream: "o", Text: "two"},
	})
}

func TestWriteAfterCloseFails(t *testing.T) {
	w := NewWriter(t.TempDir(), "a", "s", "0123456789ab", nil)
	assert.NilError(t, w.Close())
	assert.Assert(t, w.Write(time.Now(), "o", "x") != nil)
}

func TestAFailingDiskIsReportedAndWritesKeepWorking(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file")
	assert.NilError(t, os.WriteFile(root, nil, 0o644)) // a regular file: no directory can be created under it
	var errs []error
	var lines []Line
	w := NewWriter(root, "a", "s", "0123456789ab", func(l Line) { lines = append(lines, l) })
	w.OnError = func(err error) { errs = append(errs, err) }
	at := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	assert.NilError(t, w.Write(at, "o", "one"))
	assert.Assert(t, w.Flush() != nil)
	assert.Equal(t, len(errs), 1)
	assert.Assert(t, strings.Contains(errs[0].Error(), "2026-11-05.0123456789ab.log"), errs[0])

	// day change: the earlier line's flush fails, the new line is still buffered and seen by onLine
	assert.NilError(t, w.Write(at, "o", "two"))
	next := at.Add(24 * time.Hour)
	assert.Assert(t, w.Write(next, "o", "three\nfour") != nil)
	assert.Equal(t, len(errs), 2)
	assert.Equal(t, len(lines), 3)
	assert.Equal(t, lines[2].Text, "three four") // same newline replacement as on disk
}

func TestOnLineMayCallBackIntoTheWriter(t *testing.T) {
	var w *Writer
	w = NewWriter(t.TempDir(), "a", "s", "0123456789ab", func(Line) { _ = w.Flush() })
	assert.NilError(t, w.Write(time.Now(), "o", "x"))
	assert.NilError(t, w.Close())
}
