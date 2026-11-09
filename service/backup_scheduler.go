package service

import (
	"context"
	stdjson "encoding/json"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// The part that runs backups nobody asked for, on time, and says so afterwards.

// BackupSchedulePath is where the standing arrangements live.
var BackupSchedulePath = "/var/lib/casaos/backups/schedules.json"

var backupScheduleLock sync.Mutex

// BackupSchedules reads the standing arrangements.
//
// A file that will not parse is an error, not an empty list: starting over
// silently would switch off every backup on the box and leave nothing saying so.
func BackupSchedules() ([]BackupSchedule, error) {
	backupScheduleLock.Lock()
	defer backupScheduleLock.Unlock()

	return readSchedules()
}

func readSchedules() ([]BackupSchedule, error) {
	content, err := os.ReadFile(BackupSchedulePath)
	if os.IsNotExist(err) || (err == nil && len(content) == 0) {
		return []BackupSchedule{}, nil
	}
	if err != nil {
		return nil, err
	}

	var schedules []BackupSchedule
	if err := stdjson.Unmarshal(content, &schedules); err != nil {
		return nil, err
	}

	return schedules, nil
}

// SaveBackupSchedules replaces the lot.
func SaveBackupSchedules(schedules []BackupSchedule) error {
	backupScheduleLock.Lock()
	defer backupScheduleLock.Unlock()

	return writeSchedules(schedules)
}

func writeSchedules(schedules []BackupSchedule) error {
	encoded, err := stdjson.MarshalIndent(schedules, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(BackupSchedulePath), 0o700); err != nil {
		return err
	}

	temp := BackupSchedulePath + ".tmp"
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}

	return os.Rename(temp, BackupSchedulePath)
}

// forgetBackupSchedules removes every schedule of app, whatever its destination: what an
// uninstall does, so the app does not fail at every slot after it is gone.
func forgetBackupSchedules(app string) error {
	backupScheduleLock.Lock()
	defer backupScheduleLock.Unlock()

	schedules, err := readSchedules()
	if err != nil {
		return err
	}

	kept := schedules[:0:0]
	for _, schedule := range schedules {
		if schedule.App != app {
			kept = append(kept, schedule)
		}
	}
	if len(kept) == len(schedules) {
		return nil
	}

	return writeSchedules(kept)
}

// backupRunner is what a scheduled run needs, as an interface so the tick can be
// tested without a daemon, a network or a clock.
type backupRunner interface {
	Run(ctx context.Context, schedule BackupSchedule, stamp string) (BackupManifest, error)
	Runs(destination, app string) ([]string, error)
	Purge(ctx context.Context, destination, remotePath string) error
}

// RunDueBackups runs everything that has an unrun slot, records each one, and
// applies retention.
//
// It returns rather than loops, and takes `now`, so the whole of the decision can
// be exercised in a test. main drives it from the crontab that is already there,
//
// which is the only part that reads a clock.
//
// A schedule's LastRun is written whether the run SUCCEEDED or not. Retrying a
// failing backup every minute until the disk fills is worse than waiting for
// tomorrow's slot, and the failure is in the log where somebody can see it. It is written
// when that backup ends, in the file as it is then, not for all of them when the tick does:
// a tick can last hours, the schedules can be saved meanwhile, and a process that stops
// half way owes only what it did not do.
//
// One tick at a time. Cron starts one every minute whether the last is over or not, and a
// tick that finds another running leaves its backups to it: what falls due meanwhile is
// found by the tick after that one ends. Otherwise the backups owed after a hold, which can
// take longer than a minute, would be started again by every tick until they were recorded.
//
// While an operating system upgrade is open nothing runs and nothing is recorded: the
// slot stays unrun, so what was skipped runs once when the upgrade is closed. The tick
// says so in one line, at most once an hour.
func RunDueBackups(ctx context.Context, now time.Time, runner backupRunner) {
	// ponytail: one tick for the whole box, so a long backup holds up the slots that fall due
	// behind it; a tick per destination if that is ever felt.
	if !backupTick.TryLock() {
		return
	}
	defer backupTick.Unlock()

	schedules, err := BackupSchedules()
	if err != nil {
		logger.Error("could not read the backup schedules, so none ran", zap.Error(err))
		return
	}

	due, broken := DueSchedules(schedules, now)
	for _, complaint := range broken {
		logger.Error("a backup schedule cannot be understood and will never run", zap.Error(complaint))
	}

	if len(due) == 0 {
		return
	}

	pass := startPass(OriginScheduled, due, backupClock())
	defer func() { pass.end(backupClock()) }()

	for done, schedule := range due {
		// Listed as running BEFORE the flag is looked for. The core writes the flag and then
		// asks what runs, so with this order one of the two always sees the other; a backup
		// that looked first and listed itself after could be seen by neither, and run through
		// the whole upgrade.
		listed, ok := markUnlessForced(schedule.App, schedule.Destination)
		if !ok {
			// a force runs it now; its slot stays unrun for the next tick, and nothing is recorded
			pass.drop(done)
			continue
		}

		// Asked before each backup and not once for the tick: a tick can hold hours of
		// backups, and the upgrade can open between two of them.
		if open, err := osUpgradeOpen(); open {
			listed()
			if holdNoticeDue() {
				logHeldForUpgrade("backups are waiting: an operating system upgrade is open, or whether one is cannot be told",
					zap.Int("waiting", len(due)-done), zap.Error(err))
			}

			pass.hold(done)

			break
		}

		pass.running(done, backupClock())
		pass.finished(done, runScheduled(ctx, now, schedule, runner))
		listed()

		if err := recordBackupRun(schedule, now); err != nil {
			// Worth shouting about: an unrecorded run repeats at the next tick.
			logger.Error("could not record when a backup ran, so it may run again",
				zap.Error(err), zap.String("app", schedule.App), zap.String("destination", schedule.Destination))
		}
	}
}

// backupTick is held by the tick that is running: see RunDueBackups.
var backupTick sync.Mutex

// backupClock is the one clock a run's own times and the tick's hold notice are read from, a
// variable so a test can move it while a fake backup "runs". The tick's `now` is not it: that is
// when the tick began, which for the second backup of a tick can be hours before it did.
var backupClock = time.Now

// lastUpgradeHoldLog is when the tick last said backups were waiting for an upgrade.
var (
	lastUpgradeHoldLog   time.Time
	lastUpgradeHoldLogMu sync.Mutex
)

// holdNoticeDue says whether the tick should say it is holding backups back, and counts it as
// said. Once an hour: a hold can last days, and a line a minute buries the reason in the journal.
func holdNoticeDue() bool {
	lastUpgradeHoldLogMu.Lock()
	defer lastUpgradeHoldLogMu.Unlock()

	now := backupClock()
	if !lastUpgradeHoldLog.IsZero() && now.Sub(lastUpgradeHoldLog) < time.Hour {
		return false
	}

	lastUpgradeHoldLog = now

	return true
}

// recordBackupRun writes down that a schedule's slot is spent, in the file as it is now. The
// owner may have saved the schedules while the backup ran, and the list the tick read when it
// began would undo what they saved.
func recordBackupRun(schedule BackupSchedule, now time.Time) error {
	backupScheduleLock.Lock()
	defer backupScheduleLock.Unlock()

	schedules, err := readSchedules()
	if err != nil {
		return err
	}

	for i := range schedules {
		if schedules[i].App == schedule.App && schedules[i].Destination == schedule.Destination {
			schedules[i].LastRun = now
		}
	}

	return writeSchedules(schedules)
}

// runScheduled takes the tick's `now` for the backup's stamp (the slot it answers) and the clock
// for when it began: a tick chains backups, and the second begins hours after the tick did.
func runScheduled(ctx context.Context, now time.Time, schedule BackupSchedule, runner backupRunner) BackupRunRecord {
	// a force runs its schedules through here too, and has said so already
	if _, said := ctx.Value(originKey{}).(BackupOrigin); !said {
		ctx = WithBackupOrigin(ctx, OriginScheduled)
	}
	stamp := now.UTC().Format("2006-01-02T15-04-05Z")
	record := BackupRunRecord{
		App: schedule.App, Destination: schedule.Destination, Stamp: stamp,
		StartedAt: backupClock(), Scheduled: true, ContainersStopped: schedule.HoldStill,
	}

	manifest, err := runner.Run(ctx, schedule, stamp)
	finishBackupRecord(&record, manifest, err)

	if err != nil {
		logger.Error("scheduled backup failed",
			zap.Error(err), zap.String("app", schedule.App), zap.String("destination", schedule.Destination))

		// Nothing is deleted after a failure. Applying retention now could drop a
		// good backup from last week in favour of a broken one from tonight.
		return record
	}

	applyRetention(ctx, schedule, runner)

	return record
}

func applyRetention(ctx context.Context, schedule BackupSchedule, runner backupRunner) {
	ApplyRetention(ctx, schedule.Destination, schedule.App, schedule.Keep, runner)
}

// retentionRunner is the part of a runner retention needs: what is at the
// destination, and deleting some of it.
type retentionRunner interface {
	Runs(destination, app string) ([]string, error)
	Purge(ctx context.Context, destination, remotePath string) error
}

// ApplyRetention deletes the runs of `app` at `destination` beyond the last
// `keep`. Zero or less keeps everything. Scheduled runs and runs somebody asked
// for share the pool: a retention counts backups, not reasons.
func ApplyRetention(ctx context.Context, destination, app string, keep int, runner retentionRunner) {
	if keep <= 0 {
		return
	}

	// the tail of a backup, run once the backup has let go of the app: see markInProgress
	defer markInProgress(app, "backup")()

	stamps, err := runner.Runs(destination, app)
	if err != nil {
		logger.Error("could not list what is already at the destination, so nothing was deleted",
			zap.Error(err), zap.String("app", app))

		return
	}

	for _, stamp := range RunsToPurge(stamps, keep) {
		if err := runner.Purge(ctx, destination, path.Join(app, stamp)); err != nil {
			logger.Error("could not delete an old backup",
				zap.Error(err), zap.String("app", app), zap.String("run", stamp))
		}
	}
}
