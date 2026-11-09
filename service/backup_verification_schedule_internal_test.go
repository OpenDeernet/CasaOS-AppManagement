package service

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
	"go.uber.org/zap"
)

// tourClient answers every app with a manifest of its own, writes down what it was asked to
// verify, and fails the test's overlap flag if two verifications ever run at once.
type tourClient struct {
	mu       sync.Mutex
	verified []string // app/stamp, in order
	active   int
	overlap  bool
	hold     chan struct{}    // when set, a verification waits for it to close
	during   func(app string) // when set, called while each verification runs
}

func (c *tourClient) FetchLimited(_ context.Context, _, remote string, _ int64) ([]byte, error) {
	parts := strings.Split(remote, "/")
	return stdjson.Marshal(BackupManifest{
		FormatVersion: BackupFormatVersion, App: parts[0],
		Operations: []BackupOperation{{Kind: BackupKindBind, Destination: "binds/1", Directory: true}},
	})
}

func (c *tourClient) InspectBackupPath(context.Context, string, string, bool) (rclone.VerificationSize, error) {
	return rclone.VerificationSize{Bytes: 4, Files: 1}, nil
}

func (c *tourClient) StagingFreeSpace(context.Context, string) (int64, error) {
	return 2 * verificationMaxBytes, nil
}

func (c *tourClient) VerifyBackupPath(_ context.Context, _, remote, local string, _ bool, _ int64) error {
	c.mu.Lock()
	c.active++
	if c.active > 1 {
		c.overlap = true
	}
	c.verified = append(c.verified, strings.Join(strings.Split(remote, "/")[:2], "/"))
	hold, during := c.hold, c.during
	c.mu.Unlock()
	if during != nil {
		during(strings.Split(remote, "/")[0])
	}
	if hold != nil {
		<-hold
	}
	time.Sleep(20 * time.Millisecond) // long enough for a tour that did not wait to start the next
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return os.WriteFile(filepath.Join(local, "data"), []byte("data"), 0o600)
}

func (c *tourClient) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.verified...)
}

// verificationTourInTempDir puts every file the tour reads or writes in a test directory,
// with no upgrade open and nothing kept with restic.
func verificationTourInTempDir(t *testing.T) *upgradeFlag {
	t.Helper()
	schedulesInTempDir(t)
	flag := upgradeFlagIn(t)
	dir := t.TempDir()
	wasSchedule, wasLog, wasHeld := VerificationSchedulePath, BackupVerificationLogPath, resticHeld
	VerificationSchedulePath = filepath.Join(dir, "verification-schedule.json")
	BackupVerificationLogPath = filepath.Join(dir, "verifications.json")
	resticHeld = func(string) bool { return false }
	backupVerificationTasks = backupVerificationManager{}
	verifiedThisTour, verifiedFor = map[[2]string]bool{}, time.Time{}
	t.Cleanup(func() {
		if done := backupVerificationTasks.running(); done != nil {
			<-done // a verification still writing to this test's directory
		}
		VerificationSchedulePath, BackupVerificationLogPath, resticHeld = wasSchedule, wasLog, wasHeld
		backupVerificationTasks = backupVerificationManager{}
	})
	return flag
}

// parisZone is where the test box is. Its times carry the zone rather than setting time.Local,
// which a library's goroutine reads at the same time (a data race under -race).
var parisZone = func() *time.Location {
	zone, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic(err)
	}
	return zone
}()

// paris is a wall-clock time in October 2026, whose last Sunday ends summer time.
func paris(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, parisZone)
}

func saveVerificationSchedule(t *testing.T, v VerificationSchedule) {
	t.Helper()
	raw, err := stdjson.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(VerificationSchedulePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordRuns(t *testing.T, runs ...BackupRunRecord) {
	t.Helper()
	for _, run := range runs {
		if err := RecordBackupRun(run); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerificationIsOffByDefault(t *testing.T) {
	verificationTourInTempDir(t)

	v, err := GetVerificationSchedule()
	if err != nil {
		t.Fatal(err)
	}
	if want := (VerificationSchedule{Enabled: false, EveryDays: 7, At: "03:00"}); v != want {
		t.Fatalf("got %+v, want %+v", v, want)
	}
	if due, err := VerificationDue(v, paris(8, 12, 0)); due || err != nil {
		t.Fatalf("off is never due: %v %v", due, err)
	}

	_ = SaveBackupSchedules([]BackupSchedule{{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true}})
	recordRuns(t, BackupRunRecord{App: "immich", Destination: "nas", Stamp: "s1", StartedAt: paris(1, 3, 0)})
	client := &tourClient{}
	RunDueVerifications(context.Background(), paris(8, 12, 0), client)
	if got := client.seen(); len(got) != 0 {
		t.Fatalf("nothing is verified by default: %v", got)
	}
	if _, err := os.Stat(VerificationSchedulePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a tour that was not due writes nothing: %v", err)
	}

	// a file that does not parse is not the default: reset silently, it would switch it off
	if err := os.WriteFile(VerificationSchedulePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GetVerificationSchedule(); err == nil {
		t.Fatal("an unreadable schedule is an error")
	}
}

func TestVerificationDueEveryNDays(t *testing.T) {
	v := VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00", LastRun: paris(1, 3, 1)}

	for _, c := range []struct {
		name string
		now  time.Time
		due  bool
	}{
		{"the 6th day", paris(7, 23, 0), false},
		{"the 7th day, before the hour", paris(8, 2, 59), false},
		{"the 7th day, at the hour", paris(8, 3, 0), true},
		{"later", paris(12, 9, 0), true},
	} {
		if due, err := VerificationDue(v, c.now); err != nil || due != c.due {
			t.Fatalf("%s: due %v (%v), want %v", c.name, due, err, c.due)
		}
	}

	// days are calendar days: the week that loses an hour to the clocks still counts seven
	dst := VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00", LastRun: paris(20, 3, 0)}
	if due, _ := VerificationDue(dst, paris(27, 3, 0)); !due {
		t.Fatal("seven calendar days across the end of summer time")
	}

	if due, _ := VerificationDue(VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00"}, paris(8, 12, 0)); !due {
		t.Fatal("enabled and never run is due")
	}
	if _, err := VerificationDue(VerificationSchedule{Enabled: true, EveryDays: 7, At: "3h"}, paris(8, 12, 0)); err == nil {
		t.Fatal("a time that cannot be read is an error")
	}
}

func TestEnablingWaitsForTheNextSlot(t *testing.T) {
	verificationTourInTempDir(t)
	clockAt(t, paris(8, 10, 0))

	if err := SetVerificationSchedule(VerificationSchedule{Enabled: true, EveryDays: 1, At: "03:00"}); err != nil {
		t.Fatal(err)
	}
	v, err := GetVerificationSchedule()
	if err != nil {
		t.Fatal(err)
	}
	if !v.LastRun.Equal(paris(8, 3, 0)) {
		t.Fatalf("enabling spends the slot already passed: %v", v.LastRun)
	}
	if due, _ := VerificationDue(v, paris(8, 10, 0)); due {
		t.Fatal("not straight away")
	}
	if due, _ := VerificationDue(v, paris(9, 3, 0)); !due {
		t.Fatal("at the next slot")
	}

	// re-saved while enabled, the slot spent stays spent
	if err := SetVerificationSchedule(VerificationSchedule{Enabled: true, EveryDays: 2, At: "04:00"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(8, 3, 0)) || v.EveryDays != 2 || v.At != "04:00" {
		t.Fatalf("re-saving keeps LastRun: %+v", v)
	}

	// enabled before today's slot: the slot passed is yesterday's
	clockAt(t, paris(10, 2, 0))
	_ = SetVerificationSchedule(VerificationSchedule{Enabled: false, EveryDays: 1, At: "03:00"})
	_ = SetVerificationSchedule(VerificationSchedule{Enabled: true, EveryDays: 1, At: "03:00"})
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(9, 3, 0)) {
		t.Fatalf("before today's slot, yesterday's: %v", v.LastRun)
	}
}

func TestTourVerifiesTheNewestSuccessfulRunOfEachScheduledCoupleOneAfterTheOther(t *testing.T) {
	verificationTourInTempDir(t)
	saveVerificationSchedule(t, VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00", LastRun: paris(1, 3, 0)})
	_ = SaveBackupSchedules([]BackupSchedule{
		{App: "jellyfin", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: SystemBackupName, Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
	})
	recordRuns(t,
		BackupRunRecord{App: "immich", Destination: "nas", Stamp: "old", StartedAt: paris(5, 3, 0)},
		BackupRunRecord{App: "immich", Destination: "nas", Stamp: "newest-ok", StartedAt: paris(6, 3, 0)},
		BackupRunRecord{App: "immich", Destination: "nas", Stamp: "failed", StartedAt: paris(7, 3, 0), Error: "boom"},
		BackupRunRecord{App: "immich", Destination: "nas", Stamp: "restored", StartedAt: paris(7, 9, 0), Restore: true},
		BackupRunRecord{App: "immich", Destination: "usb", Stamp: "elsewhere", StartedAt: paris(7, 10, 0)},
		BackupRunRecord{App: "jellyfin", Destination: "nas", Stamp: "j", StartedAt: paris(7, 3, 0)},
		BackupRunRecord{App: SystemBackupName, Destination: "nas", Stamp: "sys", StartedAt: paris(7, 3, 0)},
	)

	clockAt(t, paris(8, 3, 5))
	client := &tourClient{}
	RunDueVerifications(context.Background(), paris(8, 3, 0), client)

	want := []string{SystemBackupName + "/sys", "immich/newest-ok", "jellyfin/j"}
	if got := client.seen(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("verified %v, want %v", got, want)
	}
	if client.overlap {
		t.Fatal("two verifications ran at once")
	}
	records, _ := BackupVerifications()
	if len(records) != 3 {
		t.Fatalf("three verifications in the history: %+v", records)
	}
	for _, record := range records {
		if record.Status != "passed" {
			t.Fatalf("each one finished before the tour ended: %+v", record)
		}
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(8, 3, 0)) {
		t.Fatalf("the tour records its own start: %v", v.LastRun)
	}

	// and not again before seven days are up
	RunDueVerifications(context.Background(), paris(8, 3, 1), client)
	if got := client.seen(); len(got) != 3 {
		t.Fatalf("once per slot: %v", got)
	}
}

func TestTourSkipsResticAndDisabledAndNeverRunCouples(t *testing.T) {
	verificationTourInTempDir(t)
	resticHeld = func(name string) bool { return name == "vault" }
	saveVerificationSchedule(t, VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00"})
	_ = SaveBackupSchedules([]BackupSchedule{
		{App: "kept-by-restic", Destination: "vault", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: "switched-off", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: false},
		{App: "never-run", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: "only-failed", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: "verified", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
	})
	recordRuns(t,
		BackupRunRecord{App: "kept-by-restic", Destination: "vault", Stamp: "r", StartedAt: paris(7, 3, 0)},
		BackupRunRecord{App: "switched-off", Destination: "nas", Stamp: "s", StartedAt: paris(7, 3, 0)},
		BackupRunRecord{App: "only-failed", Destination: "nas", Stamp: "f", StartedAt: paris(7, 3, 0), Error: "boom"},
		BackupRunRecord{App: "verified", Destination: "nas", Stamp: "v", StartedAt: paris(7, 3, 0)},
	)

	client := &tourClient{}
	RunDueVerifications(context.Background(), paris(8, 3, 0), client)

	if got := client.seen(); strings.Join(got, ",") != "verified/v" {
		t.Fatalf("verified %v", got)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(8, 3, 0)) {
		t.Fatalf("the tour is recorded: %v", v.LastRun)
	}
}

func TestTourWaitsWhileAnUpgradeIsOpen(t *testing.T) {
	flag := verificationTourInTempDir(t)
	flag.present = true
	saveVerificationSchedule(t, VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00", LastRun: paris(1, 3, 0)})
	_ = SaveBackupSchedules([]BackupSchedule{{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true}})
	recordRuns(t, BackupRunRecord{App: "immich", Destination: "nas", Stamp: "s", StartedAt: paris(7, 3, 0)})

	client := &tourClient{}
	RunDueVerifications(context.Background(), paris(8, 3, 0), client)

	if got := client.seen(); len(got) != 0 {
		t.Fatalf("nothing verified during an upgrade: %v", got)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(1, 3, 0)) {
		t.Fatalf("LastRun unchanged: %v", v.LastRun)
	}

	// closed, the slot still owed runs
	flag.present = false
	RunDueVerifications(context.Background(), paris(8, 3, 1), client)
	if got := client.seen(); strings.Join(got, ",") != "immich/s" {
		t.Fatalf("verified once the upgrade closed: %v", got)
	}
}

// A verification somebody started by hand holds the only slot: the tour waits for it to end.
func TestTourWaitsForAManualVerification(t *testing.T) {
	verificationTourInTempDir(t)
	saveVerificationSchedule(t, VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00"})
	_ = SaveBackupSchedules([]BackupSchedule{{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true}})
	recordRuns(t, BackupRunRecord{App: "immich", Destination: "nas", Stamp: "s", StartedAt: paris(7, 3, 0)})

	client := &tourClient{hold: make(chan struct{})}
	if _, err := StartBackupVerification(BackupVerificationOptions{Destination: "nas", App: "manual", Stamp: "m"}, client); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		RunDueVerifications(context.Background(), paris(8, 3, 0), client)
		close(finished)
	}()
	time.Sleep(50 * time.Millisecond)
	close(client.hold)

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the tour did not end")
	}
	if got := client.seen(); strings.Join(got, ",") != "manual/m,immich/s" {
		t.Fatalf("verified %v", got)
	}
}

func TestRejectsEveryDaysOutOfRangeAndBadTime(t *testing.T) {
	verificationTourInTempDir(t)
	for _, v := range []VerificationSchedule{
		{EveryDays: 0, At: "03:00"},
		{EveryDays: 31, At: "03:00"},
		{EveryDays: -1, At: "03:00"},
		{EveryDays: 7, At: "25:00"},
		{EveryDays: 7, At: "3h"},
		{EveryDays: 7, At: ""},
	} {
		if err := SetVerificationSchedule(v); !errors.Is(err, ErrInvalidVerificationSchedule) {
			t.Fatalf("%+v: %v", v, err)
		}
	}
	if _, err := os.Stat(VerificationSchedulePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("nothing invalid is saved")
	}
	for _, days := range []int{1, 30} {
		if err := SetVerificationSchedule(VerificationSchedule{EveryDays: days, At: "23:59"}); err != nil {
			t.Fatalf("%d days: %v", days, err)
		}
	}
}

func TestPutIgnoresClientLastRun(t *testing.T) {
	verificationTourInTempDir(t)
	clockAt(t, paris(8, 10, 0))

	if err := SetVerificationSchedule(VerificationSchedule{EveryDays: 7, At: "03:00", LastRun: paris(2, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.IsZero() {
		t.Fatalf("disabled, nothing was run: %v", v.LastRun)
	}

	if err := SetVerificationSchedule(VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00", LastRun: paris(2, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(8, 3, 0)) {
		t.Fatalf("the client's LastRun is not the slot spent: %v", v.LastRun)
	}
}

// A hand-edited file with every_days out of range would otherwise be due every minute, and the
// box would download every backup again and again.
func TestACorruptEveryDaysNeverRunsAndIsSaidOnceAnHour(t *testing.T) {
	verificationTourInTempDir(t)
	said := 0
	was := logVerificationScheduleBroken
	logVerificationScheduleBroken = func(string, ...zap.Field) { said++ }
	t.Cleanup(func() { logVerificationScheduleBroken = was; lastVerificationScheduleBrokenLog = time.Time{} })
	_ = SaveBackupSchedules([]BackupSchedule{{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true}})
	recordRuns(t, BackupRunRecord{App: "immich", Destination: "nas", Stamp: "s", StartedAt: paris(7, 3, 0)})

	client := &tourClient{}
	for _, days := range []int{0, -3, 31} {
		lastVerificationScheduleBrokenLog = time.Time{}
		said = 0
		stored := VerificationSchedule{Enabled: true, EveryDays: days, At: "03:00", LastRun: paris(1, 3, 0)}
		saveVerificationSchedule(t, stored)
		if _, err := VerificationDue(stored, paris(8, 3, 0)); err == nil {
			t.Fatalf("every_days %d is an error", days)
		}

		clockAt(t, paris(8, 3, 0))
		RunDueVerifications(context.Background(), paris(8, 3, 0), client)
		clockAt(t, paris(8, 3, 1))
		RunDueVerifications(context.Background(), paris(8, 3, 1), client)
		clockAt(t, paris(8, 4, 1))
		RunDueVerifications(context.Background(), paris(8, 4, 1), client)

		if got := client.seen(); len(got) != 0 {
			t.Fatalf("every_days %d: nothing verified, got %v", days, got)
		}
		if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(1, 3, 0)) {
			t.Fatalf("every_days %d: nothing written, LastRun %v", days, v.LastRun)
		}
		if said != 2 {
			t.Fatalf("every_days %d: said %d times over three ticks across an hour, want 2", days, said)
		}
	}

	// repaired, it runs once, and a second tick in the same minute does not run it again
	saveVerificationSchedule(t, VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00", LastRun: paris(1, 3, 0)})
	RunDueVerifications(context.Background(), paris(8, 3, 0), client)
	RunDueVerifications(context.Background(), paris(8, 3, 0), client)
	if got := client.seen(); strings.Join(got, ",") != "immich/s" {
		t.Fatalf("verified %v", got)
	}
}

// Each couple's stamp is chosen just before it is verified: a backup that ran meanwhile (and
// whose retention may have deleted the stamp the tour began with) is the one verified.
func TestTourPicksEachCoupleStampJustBeforeVerifyingIt(t *testing.T) {
	verificationTourInTempDir(t)
	saveVerificationSchedule(t, VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00"})
	_ = SaveBackupSchedules([]BackupSchedule{
		{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: "jellyfin", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
	})
	recordRuns(t,
		BackupRunRecord{App: "immich", Destination: "nas", Stamp: "s", StartedAt: paris(7, 3, 0)},
		BackupRunRecord{App: "jellyfin", Destination: "nas", Stamp: "j-old", StartedAt: paris(7, 3, 0)},
	)

	client := &tourClient{}
	client.during = func(app string) {
		if app == "immich" {
			recordRuns(t, BackupRunRecord{App: "jellyfin", Destination: "nas", Stamp: "j-new", StartedAt: paris(8, 3, 0)})
		}
	}
	RunDueVerifications(context.Background(), paris(8, 3, 0), client)

	if got := client.seen(); strings.Join(got, ",") != "immich/s,jellyfin/j-new" {
		t.Fatalf("verified %v", got)
	}
}

// A couple whose app is being backed up is left for the next tick: no record, no failure, and
// the tour is not recorded until it is done; what was verified is not verified again.
func TestTourPostponesACoupleWhoseAppIsBackingUp(t *testing.T) {
	verificationTourInTempDir(t)
	saveVerificationSchedule(t, VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00", LastRun: paris(1, 3, 0)})
	_ = SaveBackupSchedules([]BackupSchedule{
		{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: "jellyfin", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true},
	})
	recordRuns(t,
		BackupRunRecord{App: "immich", Destination: "nas", Stamp: "s", StartedAt: paris(7, 3, 0)},
		BackupRunRecord{App: "jellyfin", Destination: "nas", Stamp: "j", StartedAt: paris(7, 3, 0)},
	)

	backingUp := markInProgress("jellyfin", "backup")
	client := &tourClient{}
	RunDueVerifications(context.Background(), paris(8, 3, 0), client)
	backingUp()

	if got := client.seen(); strings.Join(got, ",") != "immich/s" {
		t.Fatalf("verified %v", got)
	}
	if records, _ := BackupVerifications(); len(records) != 1 {
		t.Fatalf("the postponed couple has no record: %+v", records)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(1, 3, 0)) {
		t.Fatalf("a tour with a couple postponed is not recorded: %v", v.LastRun)
	}

	RunDueVerifications(context.Background(), paris(8, 3, 1), client)
	if got := client.seen(); strings.Join(got, ",") != "immich/s,jellyfin/j" {
		t.Fatalf("the next tick verifies only what was postponed: %v", got)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(8, 3, 1)) {
		t.Fatalf("then the tour is recorded: %v", v.LastRun)
	}
}

// The verification is listed as running on its app by the time it has started, so a forced
// backup of that app, and the core's upgrade, see it.
func TestAVerificationIsListedBeforeItsStartReturns(t *testing.T) {
	verificationTourInTempDir(t)
	_ = SaveBackupSchedules([]BackupSchedule{{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true}})

	client := &tourClient{hold: make(chan struct{})}
	if _, err := StartBackupVerification(BackupVerificationOptions{Destination: "nas", App: "immich", Stamp: "s"}, client); err != nil {
		t.Fatal(err)
	}
	listed := RunningOperations()
	result, err := ForceSchedules(context.Background(), ForceRequest{App: "immich"}, &fakeRunner{})
	close(client.hold)
	forcedRuns.Wait()

	if len(listed) != 1 || listed[0] != (AppOperation{App: "immich", Kind: "verification"}) {
		t.Fatalf("listed as soon as it started: %+v", listed)
	}
	if err != nil || len(result.Started) != 0 || len(result.Refused) != 1 || !strings.Contains(result.Refused[0].Reason, "verification") {
		t.Fatalf("a forced backup of the app being verified is refused: %+v %v", result, err)
	}
}

// A workspace a crash left behind blocks verification for two hours. The tour waits that out
// instead of skipping every couple and recording a tour that verified nothing.
func TestTourWaitsOutAWorkspaceLeftByACrash(t *testing.T) {
	verificationTourInTempDir(t)
	saveVerificationSchedule(t, VerificationSchedule{Enabled: true, EveryDays: 7, At: "03:00", LastRun: paris(1, 3, 0)})
	_ = SaveBackupSchedules([]BackupSchedule{{App: "immich", Destination: "nas", Every: ScheduleDaily, At: "03:00", Enabled: true}})
	recordRuns(t, BackupRunRecord{App: "immich", Destination: "nas", Stamp: "s", StartedAt: paris(7, 3, 0)})
	left := filepath.Join(filepath.Dir(BackupVerificationLogPath), "verification-work", "verify-OLD-123")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}

	client := &tourClient{}
	RunDueVerifications(context.Background(), paris(8, 3, 0), client)
	if got := client.seen(); len(got) != 0 {
		t.Fatalf("nothing can start: %v", got)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(1, 3, 0)) {
		t.Fatalf("a tour that could verify nothing is not recorded: %v", v.LastRun)
	}

	longAgo := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(left, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}
	RunDueVerifications(context.Background(), paris(8, 5, 0), client)
	if got := client.seen(); strings.Join(got, ",") != "immich/s" {
		t.Fatalf("once the workspace is reclaimed the tour runs: %v", got)
	}
	if v, _ := GetVerificationSchedule(); !v.LastRun.Equal(paris(8, 5, 0)) {
		t.Fatalf("and is recorded: %v", v.LastRun)
	}
}
