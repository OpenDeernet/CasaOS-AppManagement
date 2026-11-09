package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/backupprogress"
)

// The backups that run right now, and how far each has got.
//
// A copy of 25 000 small files to Google Drive took thirteen hours on the owner's box while
// the interface said "within a minute" and "Succeeded": the only progress there was, one
// event per folder, said 0 % the whole time. What runs is kept here, in memory, with the
// engine's own counters; after a restart nothing runs, so nothing is lost by not writing it.

// BackupOrigin is why a backup runs.
type BackupOrigin string

const (
	OriginScheduled BackupOrigin = "scheduled"
	OriginForced    BackupOrigin = "forced"
	OriginOnDemand  BackupOrigin = "on_demand"
)

type originKey struct{}

// WithBackupOrigin says why the backups run under ctx.
func WithBackupOrigin(ctx context.Context, origin BackupOrigin) context.Context {
	return context.WithValue(ctx, originKey{}, origin)
}

// backupOriginOf is why a backup runs; one nobody scheduled was asked for.
func backupOriginOf(ctx context.Context) BackupOrigin {
	if origin, ok := ctx.Value(originKey{}).(BackupOrigin); ok {
		return origin
	}

	return OriginOnDemand
}

var (
	ErrLiveBackupNotFound   = errors.New("no backup with this id is running")
	ErrStoppedFromInterface = errors.New("stopped from the interface")
)

// LiveBackup is one backup that runs. Operation counts from 1; Stats is nil until the engine
// has said something about the operation under way.
type LiveBackup struct {
	ID          string
	App         string
	Destination string
	Stamp       string
	Origin      BackupOrigin
	StartedAt   time.Time
	Operation   int
	Operations  int
	Source      string
	Stats       *backupprogress.Stats
	StatsAt     time.Time
}

type liveEntry struct {
	LiveBackup
	cancel      context.CancelCauseFunc
	publishedAt time.Time
}

var live = struct {
	sync.Mutex
	entries map[string]*liveEntry
	next    int64
}{entries: map[string]*liveEntry{}}

type liveKey struct{}

// progressEvery is how often a running backup says how far it has got on the message bus.
const progressEvery = 5 * time.Second

// publishProgressFraction is a variable so a test can count what is said.
var publishProgressFraction = publishBackupFraction

// beginLive lists one backup as running until end is called, and returns the context its
// copies run under: Stop cancels it, and the engines report their counters to it.
func beginLive(ctx context.Context, app, destination, stamp string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)

	live.Lock()
	live.next++
	entry := &liveEntry{
		LiveBackup: LiveBackup{
			ID: "b" + strconv.FormatInt(live.next, 10), App: app, Destination: destination, Stamp: stamp,
			Origin: backupOriginOf(ctx), StartedAt: backupClock(),
		},
		cancel: cancel,
	}
	live.entries[entry.ID] = entry
	live.Unlock()

	ctx = context.WithValue(ctx, liveKey{}, entry)
	events := ctx
	ctx = backupprogress.With(ctx, func(stats backupprogress.Stats) { liveStats(events, entry, stats) })

	return ctx, func() {
		live.Lock()
		delete(live.entries, entry.ID)
		live.Unlock()
		cancel(nil)
	}
}

// liveOperation says which operation of the plan is under way; its figures start again.
func liveOperation(ctx context.Context, operation, operations int, source string) {
	entry, ok := ctx.Value(liveKey{}).(*liveEntry)
	if !ok {
		return
	}

	live.Lock()
	defer live.Unlock()
	entry.Operation, entry.Operations, entry.Source = operation, operations, source
	entry.Stats, entry.StatsAt = nil, time.Time{}
}

func liveStats(ctx context.Context, entry *liveEntry, stats backupprogress.Stats) {
	now := backupClock()

	live.Lock()
	entry.Stats, entry.StatsAt = &stats, now
	due := entry.publishedAt.IsZero() || now.Sub(entry.publishedAt) >= progressEvery
	var fraction float64
	if due {
		entry.publishedAt = now
		// the operations before this one, this one's share, and the manifest written last
		done := float64(entry.Operation - 1)
		if stats.TotalBytes > 0 {
			done += float64(stats.Bytes) / float64(stats.TotalBytes)
		}
		fraction = done / float64(entry.Operations+1)
	}
	source := entry.Source
	live.Unlock()

	if due {
		publishProgressFraction(ctx, fraction, source)
	}
}

// LiveBackups is every backup that runs, oldest first.
func LiveBackups() []LiveBackup {
	live.Lock()
	out := make([]LiveBackup, 0, len(live.entries))
	for _, entry := range live.entries {
		backup := entry.LiveBackup
		if backup.Stats != nil {
			stats := *backup.Stats
			backup.Stats = &stats
		}
		out = append(out, backup)
	}
	live.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}

		return out[i].ID < out[j].ID
	})

	return out
}

// StopLiveBackup stops one backup. What it already copied stays where it went; the copy's
// engine is stopped by the cancellation, and whatever the backup held still comes back.
func StopLiveBackup(id string) error {
	live.Lock()
	defer live.Unlock()

	entry, ok := live.entries[id]
	if !ok {
		return ErrLiveBackupNotFound
	}
	entry.cancel(ErrStoppedFromInterface)

	return nil
}

// stoppedFromInterface says a run that ended because somebody stopped it ended for that reason,
// not for the cancellation the engine reports.
func stoppedFromInterface(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), ErrStoppedFromInterface) && !errors.Is(err, ErrStoppedFromInterface) {
		return fmt.Errorf("%w: %v", ErrStoppedFromInterface, err)
	}

	return err
}
