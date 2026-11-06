package logstore

import (
	"os"
	"runtime"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

func seedEvents(t *testing.T) (string, []Event) {
	t.Helper()
	root := t.TempDir()
	evs := []Event{
		{At: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC), App: "plex", Service: "plex", Container: "aaaaaaaaaaaa", Kind: "started", By: "docker"},
		{At: time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC), App: "nextcloud", Service: "app", Container: "bbbbbbbbbbbb", Kind: "died", ExitCode: "137", By: "docker"},
		{At: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC), App: "plex", Service: "plex", Container: "aaaaaaaaaaaa", Kind: "died", ExitCode: "1", By: "docker"},
	}
	for _, e := range evs {
		assert.NilError(t, AppendEvent(root, e))
	}
	return root, evs
}

func TestReadEventsNewestFirstAndFilters(t *testing.T) {
	root, evs := seedEvents(t)

	got, err := ReadEvents(root, EventQuery{})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 3)
	assert.Assert(t, got[0].At.Equal(evs[2].At) && got[1].At.Equal(evs[1].At) && got[2].At.Equal(evs[0].At))

	got, err = ReadEvents(root, EventQuery{Apps: []string{"plex"}})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 2)
	for _, e := range got {
		assert.Equal(t, e.App, "plex")
	}

	got, err = ReadEvents(root, EventQuery{Kinds: []string{"died"}})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 2)
	for _, e := range got {
		assert.Equal(t, e.Kind, "died")
	}

	got, err = ReadEvents(root, EventQuery{Limit: 1})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 1)
	assert.Assert(t, got[0].At.Equal(evs[2].At))
}

func TestReadEventsBoundsAreExact(t *testing.T) {
	root, evs := seedEvents(t)
	got, err := ReadEvents(root, EventQuery{Since: evs[1].At, Until: evs[1].At})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].App, "nextcloud")
}

func TestReadEventsUntilSkipsLaterDayFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Chmod(0) does not make a file unreadable on Windows")
	}
	root, evs := seedEvents(t)
	later := EventsFile(root, evs[2].At)
	assert.NilError(t, os.Chmod(later, 0))
	t.Cleanup(func() { os.Chmod(later, 0o644) })

	got, err := ReadEvents(root, EventQuery{Until: evs[0].At})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].Kind, "started")
}

func TestReadEventsSkipsCorruptLine(t *testing.T) {
	root, evs := seedEvents(t)
	f, err := os.OpenFile(EventsFile(root, evs[2].At), os.O_APPEND|os.O_WRONLY, 0o644)
	assert.NilError(t, err)
	_, err = f.WriteString("{not json\n")
	assert.NilError(t, err)
	assert.NilError(t, f.Close())

	got, err := ReadEvents(root, EventQuery{})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 3)
}

func TestReadEventsNoDirectory(t *testing.T) {
	got, err := ReadEvents(t.TempDir(), EventQuery{})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 0)
}

func TestReadEventsSkipsADayFileGoneSinceTheListing(t *testing.T) {
	root, _ := seedEvents(t)
	// a dangling link matches the Glob and reads as not found, as a file purged in between does
	if err := os.Symlink("gone.jsonl", EventsFile(root, d("2026-11-05"))); err != nil {
		t.Skip("no symlinks here:", err)
	}
	got, err := ReadEvents(root, EventQuery{})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 3)
}
