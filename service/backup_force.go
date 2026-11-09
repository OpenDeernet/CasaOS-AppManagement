package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"go.uber.org/zap"
)

// Running the standing arrangements now, because somebody asked.
//
// A forced run is the scheduled run of that couple, settings and retention included, and is
// recorded as one. It never spends the schedule's slot: last_run stays where it is, so the
// backup of tonight still runs tonight.

// ForceRequest narrows a force to one app, one destination or one couple. Empty is no filter.
type ForceRequest struct{ App, Destination string }

// ForceRefusal is a couple that did not start, and why, in words the owner can read.
type ForceRefusal struct{ App, Destination, Reason string }

// ForceResult is what started and what did not.
type ForceResult struct {
	Started []BackupSchedule
	Refused []ForceRefusal
}

var (
	ErrNoSuchSchedule = errors.New("no such backup schedule")
	ErrUpgradeOpen    = errors.New("an operating system upgrade is open")
)

// forced is the couples a force has claimed and not finished. Its lock is taken before the
// lock of appOperations, never after.
var forced = struct {
	sync.Mutex
	inFlight map[string]struct{}
}{inFlight: map[string]struct{}{}}

// forcedRuns counts the forced runs still going, so a test can wait for them.
var forcedRuns sync.WaitGroup

func forcedKey(app, destination string) string { return app + "\x00" + destination }

func forcedInFlight(app, destination string) bool {
	forced.Lock()
	defer forced.Unlock()

	_, ok := forced.inFlight[forcedKey(app, destination)]

	return ok
}

// markUnlessForced is the tick's markInProgress, refused for a couple a force runs. Both under
// the lock of forced, so a force that looks at what runs either sees the tick's mark or is seen.
func markUnlessForced(app, destination string) (done func(), ok bool) {
	forced.Lock()
	defer forced.Unlock()

	if _, running := forced.inFlight[forcedKey(app, destination)]; running {
		return nil, false
	}

	return markInProgress(app, "backup"), true
}

// ForceSchedules starts the selected schedules now and returns at once. Each accepted couple is
// claimed before it returns, so a second force of the same couple is refused; the runs go on in
// the background, one after the other as a tick's do, detached from ctx.
func ForceSchedules(ctx context.Context, request ForceRequest, runner backupRunner) (ForceResult, error) {
	result := ForceResult{Started: []BackupSchedule{}, Refused: []ForceRefusal{}}

	schedules, err := BackupSchedules()
	if err != nil {
		return result, err
	}

	filtered := request.App != "" || request.Destination != ""
	var matched []BackupSchedule
	for _, schedule := range schedules {
		if (request.App == "" || schedule.App == request.App) &&
			(request.Destination == "" || schedule.Destination == request.Destination) &&
			(schedule.Enabled || filtered) {
			matched = append(matched, schedule)
		}
	}
	if len(matched) == 0 {
		return result, ErrNoSuchSchedule
	}

	sort.Slice(matched, func(i, j int) bool {
		return forcedKey(matched[i].App, matched[i].Destination) < forcedKey(matched[j].App, matched[j].Destination)
	})

	now := backupClock()
	refuse := func(schedule BackupSchedule, reason string) {
		result.Refused = append(result.Refused, ForceRefusal{App: schedule.App, Destination: schedule.Destination, Reason: reason})
	}

	forced.Lock()
	busy := map[string]string{}
	for _, operation := range RunningOperations() {
		busy[operation.App] = operation.Kind
	}

	var marks []func()
	for _, schedule := range matched {
		key := forcedKey(schedule.App, schedule.Destination)
		_, running := forced.inFlight[key]
		_, unreadable := IsDue(schedule, now)

		switch {
		case !schedule.Enabled:
			refuse(schedule, "this schedule is switched off")
		case unreadable != nil:
			refuse(schedule, "this schedule cannot be read: "+unreadable.Error())
		case running:
			refuse(schedule, "already running")
		case busy[schedule.App] != "":
			refuse(schedule, fmt.Sprintf("another operation is running on this app (%s)", busy[schedule.App]))
		default:
			forced.inFlight[key] = struct{}{}
			marks = append(marks, markInProgress(schedule.App, "backup"))
			result.Started = append(result.Started, schedule)
		}
	}
	forced.Unlock()

	release := func(schedule BackupSchedule, mark func()) {
		mark()
		forced.Lock()
		delete(forced.inFlight, forcedKey(schedule.App, schedule.Destination))
		forced.Unlock()
	}

	if len(result.Started) == 0 {
		return result, nil
	}

	// Listed as running before the flag is looked for, as the tick does: see RunDueBackups.
	if open, _ := osUpgradeOpen(); open {
		for i, schedule := range result.Started {
			release(schedule, marks[i])
			refuse(schedule, ErrUpgradeOpen.Error())
		}
		result.Started = []BackupSchedule{}

		return result, ErrUpgradeOpen
	}

	started := result.Started
	ctx = context.WithoutCancel(ctx)

	forcedRuns.Add(1)
	go func() {
		defer forcedRuns.Done()

		for i, schedule := range started {
			// a chain can last hours, and an upgrade can open between two of its backups
			if open, err := osUpgradeOpen(); open {
				for j := i; j < len(started); j++ {
					release(started[j], marks[j])
				}
				logHeldForUpgrade("forced backups were dropped: an operating system upgrade is open, or whether one is cannot be told",
					zap.Int("dropped", len(started)-i), zap.Error(err))

				return
			}

			runScheduled(ctx, now, schedule, runner)
			release(schedule, marks[i])
		}
	}()

	return result, nil
}
