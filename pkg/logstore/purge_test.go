package logstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

func d(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

var now = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)

func TestPlanPurgeRetentionByDay(t *testing.T) {
	files := []FileInfo{
		{Path: "a/s/old", Day: d("2026-11-02"), Size: 1},
		{Path: "a/s/edge", Day: d("2026-11-03"), Size: 1}, // today-7d: removed
		{Path: "a/s/kept", Day: d("2026-11-04"), Size: 1}, // 7 days kept: Nov 4..10
		{Path: "a/s/new", Day: d("2026-11-10"), Size: 1},
		{Path: "a/s/unknown", Size: 1},
	}
	assert.DeepEqual(t, PlanPurge(files, now, 7, 0), []string{"a/s/old", "a/s/edge"})
	assert.Equal(t, len(PlanPurge(files, now, 0, 0)), 0)
}

func TestPlanPurgeCapRemovesOldestDayAcrossApps(t *testing.T) {
	files := []FileInfo{
		{Path: "chatty/s/1", Day: d("2026-11-08"), Size: 60},
		{Path: "quiet/s/1", Day: d("2026-11-08"), Size: 1},
		{Path: "_events/1", Day: d("2026-11-08"), Size: 1},
		{Path: "chatty/s/2", Day: d("2026-11-09"), Size: 30},
		{Path: "quiet/s/2", Day: d("2026-11-09"), Size: 30},
		{Path: "x/unknown", Size: 500},
	}
	got := PlanPurge(files[:5], now, 0, 70)
	assert.DeepEqual(t, got, []string{"chatty/s/1", "quiet/s/1", "_events/1"})
	// a day only partly needed is still removed whole
	got = PlanPurge(files[:5], now, 0, 100)
	assert.DeepEqual(t, got, []string{"chatty/s/1", "quiet/s/1", "_events/1"})
	assert.Equal(t, len(PlanPurge(files[:5], now, 0, 0)), 0)
	assert.Equal(t, len(PlanPurge(files[:5], now, 0, 1000)), 0)
}

func TestPurgeAndUsage(t *testing.T) {
	root := t.TempDir()
	put := func(rel string, n int) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		assert.NilError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		assert.NilError(t, os.WriteFile(p, make([]byte, n), 0o644))
	}
	put("app1/web/2026-11-01.aaaaaaaaaaaa.log", 10)
	put("app2/db/2026-11-09.bbbbbbbbbbbb.log", 5)
	put("_events/2026-11-01.jsonl", 3)
	put("app2/db/notes.txt", 2)

	n, oldest, err := Usage(root)
	assert.NilError(t, err)
	assert.Equal(t, n, int64(20))
	assert.Equal(t, oldest, d("2026-11-01"))

	removed, err := Purge(root, now, 7, 0)
	assert.NilError(t, err)
	assert.Equal(t, removed, int64(13))
	_, err = os.Stat(filepath.Join(root, "app1"))
	assert.Assert(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "_events"))
	assert.NilError(t, err)
	_, err = os.Stat(filepath.Join(root, "app2", "db", "notes.txt"))
	assert.NilError(t, err)

	n, _, err = Usage(filepath.Join(root, "missing"))
	assert.NilError(t, err)
	assert.Equal(t, n, int64(0))
}

func TestPlanPurgeUnknownCountsButIsNeverRemoved(t *testing.T) {
	files := []FileInfo{
		{Path: "a/s/1", Day: d("2026-11-08"), Size: 10},
		{Path: "a/s/2", Day: d("2026-11-09"), Size: 10},
		{Path: "x/unknown", Size: 500},
	}
	assert.DeepEqual(t, PlanPurge(files, now, 0, 100), []string{"a/s/1", "a/s/2"})
}

func TestDayOfRejectsEventsAsApp(t *testing.T) {
	assert.Assert(t, dayOf("r", filepath.Join("r", "_events", "x", "2026-11-01.c.log")).IsZero())
}

func TestRemovePlannedContinuesAndCountsOnlyRemoved(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		assert.NilError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		assert.NilError(t, os.WriteFile(p, []byte("x"), 0o644))
		return p
	}
	stuck := filepath.Join(root, "a", "s", "2026-11-01.c.log") // a non-empty directory: os.Remove fails
	assert.NilError(t, os.MkdirAll(stuck, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(stuck, "f"), nil, 0o644))
	ok := mk("b/s/2026-11-02.c.log")
	gone := filepath.Join(root, "c", "s", "2026-11-03.c.log")
	size := map[string]int64{stuck: 7, ok: 3, gone: 5}

	removed, err := removePlanned(root, []string{stuck, gone, ok}, size)
	assert.Assert(t, err != nil)
	assert.Equal(t, removed, int64(3))
	_, e := os.Stat(ok)
	assert.Assert(t, os.IsNotExist(e))
}
