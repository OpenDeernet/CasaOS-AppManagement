package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// upgradeFlag stands in for the busy file of an operating system upgrade, and for the
// log line that says backups are waiting, so a test can choose what stat answers and
// count what was said about it.
type upgradeFlag struct {
	present bool  // the file is there
	err     error // stat fails with this instead, whatever present says
	info    os.FileInfo
	// onAsk, when set, is what happens at the moment the file is looked for
	onAsk func()

	asked []string
	said  [][]zap.Field
}

func (u *upgradeFlag) stat(name string) (os.FileInfo, error) {
	u.asked = append(u.asked, name)
	if u.onAsk != nil {
		u.onAsk()
	}

	switch {
	case u.err != nil:
		return nil, &fs.PathError{Op: "stat", Path: name, Err: u.err}
	case u.present:
		return u.info, nil
	}

	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (u *upgradeFlag) log(_ string, fields ...zap.Field) {
	u.said = append(u.said, fields)
}

func (u *upgradeFlag) field(line int, key string) (zap.Field, bool) {
	for _, field := range u.said[line] {
		if field.Key == key {
			return field, true
		}
	}

	return zap.Field{}, false
}

// upgradeFlagIn puts the fake where the scheduler looks, with no upgrade open. The real
// busy file is never read by a test: a box that is being upgraded must not change what
// the suite says.
func upgradeFlagIn(t *testing.T) *upgradeFlag {
	t.Helper()

	// something for stat to answer "it is there" with
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	flag := &upgradeFlag{info: info}
	wasStat, wasLog := osUpgradeBusyStat, logHeldForUpgrade
	osUpgradeBusyStat, logHeldForUpgrade = flag.stat, flag.log
	forgetBackupHoldNotice() // a test starts as a fresh process would: nothing said yet
	t.Cleanup(func() { osUpgradeBusyStat, logHeldForUpgrade = wasStat, wasLog; forgetBackupHoldNotice() })

	return flag
}

// The core is the only one that writes the file, and only its absence says there is no
// upgrade. Anything else the filesystem can answer is an upgrade nobody can rule out.
func TestOnlyAMissingBusyFileMeansNoUpgrade(t *testing.T) {
	cases := []struct {
		name    string
		present bool
		err     error
		open    bool
	}{
		{name: "missing", open: false},
		{name: "there", present: true, open: true},
		{name: "not allowed", err: syscall.EACCES, open: true},
		{name: "not allowed, in the library's words", err: fs.ErrPermission, open: true},
		{name: "disk error", err: syscall.EIO, open: true},
		{name: "the directory is a file", err: syscall.ENOTDIR, open: true},
		{name: "anything else", err: errors.New("stale handle"), open: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			flag := upgradeFlagIn(t)
			flag.present, flag.err = c.present, c.err

			open, err := osUpgradeOpen()
			if open != c.open {
				t.Fatalf("open = %v, want %v", open, c.open)
			}
			if c.err != nil && !errors.Is(err, c.err) {
				t.Fatalf("the reason is handed back: %v", err)
			}
			if c.err == nil && err != nil {
				t.Fatalf("no failure to report: %v", err)
			}

			// the file the core keeps: this is a promise between two services
			if len(flag.asked) != 1 || flag.asked[0] != "/var/lib/casaos/os-upgrade/busy" {
				t.Fatalf("asked about %v", flag.asked)
			}
		})
	}
}

// What the filesystem says about the file, on this system's own terms, with the flag read
// as production wires it: the test's own is the path, nothing else.
func TestTheRealFilesystemIsReadTheSameWay(t *testing.T) {
	// the file the core keeps: this is a promise between two services
	if osUpgradeBusyPath != "/var/lib/casaos/os-upgrade/busy" {
		t.Fatalf("the core keeps the flag elsewhere: %s", osUpgradeBusyPath)
	}

	where := filepath.Join(t.TempDir(), "busy")
	wasPath := osUpgradeBusyPath
	osUpgradeBusyPath = where
	t.Cleanup(func() { osUpgradeBusyPath = wasPath })

	if open, err := osUpgradeOpen(); open || err != nil {
		t.Fatalf("nothing there: %v %v", open, err)
	}

	if err := writeRaw(where, "{}"); err != nil {
		t.Fatal(err)
	}
	if open, err := osUpgradeOpen(); !open || err != nil {
		t.Fatalf("the file is there: %v %v", open, err)
	}

	// Windows words these differently, and it is not where this runs
	if runtime.GOOS == "windows" {
		return
	}

	// A path through a file is not "nothing there".
	osUpgradeBusyPath = filepath.Join(where, "busy")
	if open, err := osUpgradeOpen(); !open || err == nil {
		t.Fatalf("a path through a file is not 'nothing there': %v %v", open, err)
	}

	// A link whose target is gone is something somebody put there, as the core reads it.
	osUpgradeBusyPath = filepath.Join(t.TempDir(), "busy")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), osUpgradeBusyPath); err != nil {
		t.Fatal(err)
	}
	if open, err := osUpgradeOpen(); !open || err != nil {
		t.Fatalf("a dangling link is a flag: %v %v", open, err)
	}
}

// A tick is held by a flag read from the disk, and says so in the log of the service: both as
// production wires them. A fail-open default for either passes every test that fakes the seam.
func TestAHeldTickRunsWithTheSeamsAsTheServiceWiresThem(t *testing.T) {
	schedulesInTempDir(t)

	// the line goes to the log of the service, and to no other
	if reflect.ValueOf(logHeldForUpgrade).Pointer() != reflect.ValueOf(logger.Warn).Pointer() {
		t.Fatal("what waits is not told in the log of the service")
	}

	wasPath := osUpgradeBusyPath
	osUpgradeBusyPath = filepath.Join(t.TempDir(), "busy")
	t.Cleanup(func() { osUpgradeBusyPath = wasPath })
	if err := writeRaw(osUpgradeBusyPath, "{}"); err != nil {
		t.Fatal(err)
	}

	_ = SaveBackupSchedules(threeDue())
	runner := &fakeRunner{}
	RunDueBackups(context.Background(), at(10, 7, 0), runner)

	if len(runner.ran) != 0 {
		t.Fatalf("nothing runs through an upgrade: %v", runner.ran)
	}
}

func TestBackupsWaitWhileAnUpgradeIsOpen(t *testing.T) {
	schedulerInTempDir(t)
	flag := upgradeFlagIn(t)
	flag.present = true

	if err := SaveBackupSchedules([]BackupSchedule{daily(time.Time{})}); err != nil {
		t.Fatal(err)
	}

	// an old file, so that a rewrite would show
	longAgo := time.Now().Add(-time.Hour)
	if err := os.Chtimes(BackupSchedulePath, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	RunDueBackups(context.Background(), at(10, 7, 0), runner)

	if len(runner.ran) != 0 {
		t.Fatalf("nothing runs through an upgrade: %v", runner.ran)
	}
	if info, err := os.Stat(BackupSchedulePath); err != nil || info.ModTime().After(longAgo.Add(time.Minute)) {
		t.Fatalf("a tick that ran nothing leaves the schedules as they were: %v %v", info, err)
	}
	if records, _ := BackupRuns(); len(records) != 0 {
		t.Fatalf("and no run is recorded: %+v", records)
	}
	if len(flag.said) != 1 {
		t.Fatalf("the tick says it held them back, once: %d", len(flag.said))
	}

	// The slot is not spent: a backup skipped for an upgrade is a backup owed, run once
	// when the upgrade is closed.
	schedules, err := BackupSchedules()
	if err != nil || len(schedules) != 1 || !schedules[0].LastRun.IsZero() {
		t.Fatalf("the slot is still unrun: %+v %v", schedules, err)
	}

	flag.present = false
	RunDueBackups(context.Background(), at(10, 7, 1), runner)
	RunDueBackups(context.Background(), at(10, 7, 2), runner)

	if len(runner.ran) != 1 {
		t.Fatalf("once, after the upgrade is closed: %v", runner.ran)
	}
	if len(flag.said) != 1 {
		t.Fatalf("a free box says nothing: %d", len(flag.said))
	}
}

// A flag that cannot be read might be an upgrade, and backups are taken tomorrow if not
// today. The log says which it was.
func TestBackupsWaitWhenTheBusyFileCannotBeRead(t *testing.T) {
	schedulerInTempDir(t)
	flag := upgradeFlagIn(t)
	flag.err = syscall.EACCES

	_ = SaveBackupSchedules([]BackupSchedule{daily(time.Time{})})

	runner := &fakeRunner{}
	RunDueBackups(context.Background(), at(10, 7, 0), runner)

	if len(runner.ran) != 0 {
		t.Fatalf("an unreadable flag is not a free box: %v", runner.ran)
	}
	if len(flag.said) != 1 {
		t.Fatalf("one line: %d", len(flag.said))
	}
	if reason, ok := flag.field(0, "error"); !ok || !errors.Is(reason.Interface.(error), syscall.EACCES) {
		t.Fatalf("the line carries the reason: %+v", reason)
	}

	// nothing is spent either
	schedules, _ := BackupSchedules()
	if len(schedules) != 1 || !schedules[0].LastRun.IsZero() {
		t.Fatalf("%+v", schedules)
	}
}

// A tick is one cycle. A minute of waiting is one line however many backups wait, and the
// line says how many.
func TestAHeldTickSaysOneThingNoMatterHowManyWait(t *testing.T) {
	schedulerInTempDir(t)
	flag := upgradeFlagIn(t)
	flag.present = true

	_ = SaveBackupSchedules(threeDue())

	runner := &fakeRunner{}
	RunDueBackups(context.Background(), at(10, 7, 0), runner)

	if len(flag.said) != 1 {
		t.Fatalf("one line for the cycle, not one per backup: %d", len(flag.said))
	}
	if waiting, ok := flag.field(0, "waiting"); !ok || waiting.Integer != 3 {
		t.Fatalf("it says how many wait: %+v", waiting)
	}
	if len(runner.ran) != 0 {
		t.Fatalf("%v", runner.ran)
	}

	// and the next minute is another cycle, which does not say it again: see the test below
	RunDueBackups(context.Background(), at(10, 7, 1), runner)
	if len(flag.said) != 1 {
		t.Fatalf("said once for the hour: %d", len(flag.said))
	}
}

// A hold that lasts days said the same warning every minute, and drowned the journal. Once an
// hour is enough to find the reason; the backups still wait every minute.
func TestAHeldTickSaysItOncePerHour(t *testing.T) {
	schedulerInTempDir(t)
	flag := upgradeFlagIn(t)
	flag.present = true

	_ = SaveBackupSchedules([]BackupSchedule{daily(time.Time{})})

	clock := clockAt(t, at(10, 7, 0))
	runner := &fakeRunner{}

	RunDueBackups(context.Background(), at(10, 7, 0), runner)
	*clock = clock.Add(time.Minute)
	RunDueBackups(context.Background(), at(10, 7, 1), runner)

	if len(flag.said) != 1 {
		t.Fatalf("two held ticks a minute apart say it once: %d", len(flag.said))
	}

	*clock = clock.Add(61 * time.Minute)
	RunDueBackups(context.Background(), at(10, 8, 2), runner)

	if len(flag.said) != 2 {
		t.Fatalf("and again an hour later: %d", len(flag.said))
	}
	if len(runner.ran) != 0 {
		t.Fatalf("saying less is not running more: %v", runner.ran)
	}
}

func threeDue() []BackupSchedule {
	first, second, third := daily(time.Time{}), daily(time.Time{}), daily(time.Time{})
	first.App, second.App, third.App = "immich", "jellyfin", "nextcloud"

	return []BackupSchedule{first, second, third}
}

// openingRunner opens the upgrade while its first backup runs, the way one the owner
// starts can between two backups of a tick.
type openingRunner struct {
	fakeRunner
	flag *upgradeFlag
}

func (o *openingRunner) Run(ctx context.Context, schedule BackupSchedule, stamp string) (BackupManifest, error) {
	o.flag.present = true

	return o.fakeRunner.Run(ctx, schedule, stamp)
}

// A tick can hold hours of backups, so the flag is looked at before each of them, not
// once at the top.
func TestAnUpgradeThatOpensBetweenTwoBackupsHoldsTheSecond(t *testing.T) {
	schedulerInTempDir(t)
	flag := upgradeFlagIn(t)

	_ = SaveBackupSchedules(threeDue())

	runner := &openingRunner{flag: flag}
	RunDueBackups(context.Background(), at(10, 7, 0), runner)

	if len(runner.ran) != 1 || !strings.HasPrefix(runner.ran[0], "immich@") {
		t.Fatalf("the first one was already running: %v", runner.ran)
	}
	if len(flag.said) != 1 {
		t.Fatalf("one line for the cycle: %d", len(flag.said))
	}
	if waiting, _ := flag.field(0, "waiting"); waiting.Integer != 2 {
		t.Fatalf("two are left waiting, not three: %+v", waiting)
	}

	// what ran is recorded as having run; what did not is still owed
	schedules, _ := BackupSchedules()
	for _, schedule := range schedules {
		if ran := !schedule.LastRun.IsZero(); ran != (schedule.App == "immich") {
			t.Fatalf("%s: recorded as run = %v", schedule.App, ran)
		}
	}

	flag.present = false
	catchUp := &fakeRunner{}
	RunDueBackups(context.Background(), at(10, 7, 1), catchUp)
	if len(catchUp.ran) != 2 {
		t.Fatalf("the other two run once the upgrade is closed: %v", catchUp.ran)
	}
}

// A tick that finds nothing due costs one file read, and a box with nothing scheduled
// has nothing to say about an upgrade.
func TestATickWithNothingDueSaysNothingOfTheUpgrade(t *testing.T) {
	schedulerInTempDir(t)
	flag := upgradeFlagIn(t)
	flag.present = true

	RunDueBackups(context.Background(), at(10, 7, 0), &fakeRunner{})

	// ran at 03:00 today: nothing is due at 07:00
	_ = SaveBackupSchedules([]BackupSchedule{daily(at(10, 3, 0))})
	RunDueBackups(context.Background(), at(10, 7, 0), &fakeRunner{})

	if len(flag.said) != 0 || len(flag.asked) != 0 {
		t.Fatalf("said %d, asked %v", len(flag.said), flag.asked)
	}
}

// The core writes the flag and then asks what runs; a backup lists itself as running and then
// looks for the flag. One of the two always sees the other. A backup that looked first and
// listed itself after would leave a gap in which it has seen no flag and the core has seen no
// backup, and it would run through the whole upgrade.
func TestABackupIsListedAsRunningBeforeTheFlagIsLookedFor(t *testing.T) {
	nextcloud := []AppOperation{{App: "nextcloud", Kind: "backup"}}

	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "free", true: "held"}[held], func(t *testing.T) {
			schedulerInTempDir(t)
			flag := upgradeFlagIn(t)
			flag.present = held

			var whenAsked []AppOperation
			flag.onAsk = func() { whenAsked = RunningOperations() }
			_ = SaveBackupSchedules([]BackupSchedule{daily(time.Time{})})

			var whileRunning []AppOperation
			runner := newGatedRunner()
			runner.letGo()
			runner.during = func(string) { whileRunning = RunningOperations() }

			RunDueBackups(context.Background(), at(10, 7, 0), runner)

			if !reflect.DeepEqual(whenAsked, nextcloud) {
				t.Fatalf("listed when the flag was looked for: %v", whenAsked)
			}
			if held && len(runner.apps()) != 0 {
				t.Fatalf("nothing runs through an upgrade: %v", runner.apps())
			}
			if !held && !reflect.DeepEqual(whileRunning, nextcloud) {
				t.Fatalf("listed while it runs: %v", whileRunning)
			}
			if left := RunningOperations(); len(left) != 0 {
				t.Fatalf("a backup that is held, or over, is not listed any more: %v", left)
			}
		})
	}
}

// forgetBackupHoldNotice makes the next held tick say so, as the first one after a start does.
func forgetBackupHoldNotice() {
	lastUpgradeHoldLogMu.Lock()
	defer lastUpgradeHoldLogMu.Unlock()

	lastUpgradeHoldLog = time.Time{}
}
