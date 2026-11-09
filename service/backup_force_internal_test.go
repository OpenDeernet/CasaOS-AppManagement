package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateRunner is a runner whose backups wait for the test to let them go, safe to call from
// the goroutines a force starts.
type gateRunner struct {
	mu      sync.Mutex
	ran     []BackupSchedule
	stamps  []string
	purged  []string
	present []string
	fail    error
	started chan struct{} // one send per Run, when given
	release chan struct{} // Run waits for it to close, when given
}

func (g *gateRunner) Run(_ context.Context, schedule BackupSchedule, stamp string) (BackupManifest, error) {
	g.mu.Lock()
	g.ran = append(g.ran, schedule)
	g.stamps = append(g.stamps, stamp)
	g.mu.Unlock()

	if g.started != nil {
		g.started <- struct{}{}
	}
	if g.release != nil {
		<-g.release
	}
	if g.fail != nil {
		return BackupManifest{}, g.fail
	}

	return BackupManifest{Operations: []BackupOperation{{}}}, nil
}

func (g *gateRunner) Runs(_, _ string) ([]string, error) { return g.present, nil }

func (g *gateRunner) Purge(_ context.Context, _, remotePath string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.purged = append(g.purged, remotePath)

	return nil
}

func (g *gateRunner) runs() []BackupSchedule {
	g.mu.Lock()
	defer g.mu.Unlock()

	return append([]BackupSchedule(nil), g.ran...)
}

func apps(schedules []BackupSchedule) []string {
	out := []string{}
	for _, schedule := range schedules {
		out = append(out, schedule.App+"@"+schedule.Destination)
	}

	return out
}

func TestForceStartsTheScheduleWithItsOwnSettings(t *testing.T) {
	schedulerInTempDir(t)
	clockAt(t, at(10, 15, 30))

	schedule := daily(at(10, 3, 0)) // already ran today: forcing does not care
	schedule.HoldStill, schedule.Keep = true, 1
	_ = SaveBackupSchedules([]BackupSchedule{schedule})

	runner := &gateRunner{present: []string{"2026-09-09T03-00-00Z", "2026-09-10T15-30-00Z"}}
	result, err := ForceSchedules(context.Background(), ForceRequest{}, runner)
	if err != nil || len(result.Started) != 1 || len(result.Refused) != 0 {
		t.Fatalf("one started: %+v %v", result, err)
	}
	forcedRuns.Wait()

	ran := runner.runs()
	if len(ran) != 1 || !ran[0].HoldStill || ran[0].Destination != "offsite" {
		t.Fatalf("the schedule's own settings: %+v", ran)
	}
	if runner.stamps[0] != "2026-09-10T15-30-00Z" {
		t.Fatalf("stamped when it was forced: %s", runner.stamps[0])
	}
	if len(runner.purged) != 1 || runner.purged[0] != "nextcloud/2026-09-09T03-00-00Z" {
		t.Fatalf("its retention applies after a success: %v", runner.purged)
	}

	records, _ := BackupRuns()
	if len(records) != 1 || !records[0].Scheduled || !records[0].Succeeded() {
		t.Fatalf("recorded as a scheduled run: %+v", records)
	}

	// and after a failure nothing is deleted
	failing := &gateRunner{fail: errors.New("nope"), present: runner.present}
	if _, err := ForceSchedules(context.Background(), ForceRequest{}, failing); err != nil {
		t.Fatal(err)
	}
	forcedRuns.Wait()
	if len(failing.purged) != 0 {
		t.Fatalf("nothing deleted after a failure: %v", failing.purged)
	}
}

func TestForceDoesNotTouchLastRun(t *testing.T) {
	schedulerInTempDir(t)
	clockAt(t, at(10, 15, 30))

	lastRun := at(10, 3, 0)
	_ = SaveBackupSchedules([]BackupSchedule{daily(lastRun)})

	if _, err := ForceSchedules(context.Background(), ForceRequest{}, &gateRunner{}); err != nil {
		t.Fatal(err)
	}
	forcedRuns.Wait()

	schedules, _ := BackupSchedules()
	if !schedules[0].LastRun.Equal(lastRun) {
		t.Fatalf("last_run moved: %v", schedules[0].LastRun)
	}
}

func TestForceRefusedWhileAnUpgradeIsOpen(t *testing.T) {
	schedulerInTempDir(t)
	upgradeFlagIn(t).present = true
	_ = SaveBackupSchedules(threeDue())

	runner := &gateRunner{}
	result, err := ForceSchedules(context.Background(), ForceRequest{}, runner)
	if !errors.Is(err, ErrUpgradeOpen) {
		t.Fatalf("want ErrUpgradeOpen, got %v", err)
	}
	forcedRuns.Wait()

	if len(runner.runs()) != 0 || len(result.Started) != 0 || result.Refused == nil {
		t.Fatalf("nothing runs: %v %+v", runner.runs(), result)
	}
	if len(RunningOperations()) != 0 || forcedInFlight("immich", "offsite") {
		t.Fatal("and nothing stays claimed")
	}
}

func TestForceRefusesAnAppWithAnOperationInProgress(t *testing.T) {
	schedulerInTempDir(t)
	_ = SaveBackupSchedules(threeDue())

	done := markInProgress("immich", "update")
	defer done()

	runner := &gateRunner{}
	result, err := ForceSchedules(context.Background(), ForceRequest{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	forcedRuns.Wait()

	if len(result.Refused) != 1 || result.Refused[0].App != "immich" ||
		result.Refused[0].Reason != "another operation is running on this app (update)" {
		t.Fatalf("immich is refused with the reason: %+v", result.Refused)
	}
	if got := strings.Join(apps(runner.runs()), ","); got != "jellyfin@offsite,nextcloud@offsite" {
		t.Fatalf("the others start: %s", got)
	}
}

func TestTwoForcesOnTheSameCoupleStartOnce(t *testing.T) {
	schedulerInTempDir(t)
	_ = SaveBackupSchedules([]BackupSchedule{daily(time.Time{})})

	runner := &gateRunner{started: make(chan struct{}, 2), release: make(chan struct{})}

	results := make([]ForceResult, 2)
	var calls sync.WaitGroup
	for i := range results {
		calls.Add(1)
		go func() {
			defer calls.Done()
			results[i], _ = ForceSchedules(context.Background(), ForceRequest{App: "nextcloud"}, runner)
		}()
	}
	calls.Wait()

	started, refused := 0, 0
	for _, result := range results {
		started += len(result.Started)
		for _, refusal := range result.Refused {
			if refusal.Reason == "already running" {
				refused++
			}
		}
	}
	if started != 1 || refused != 1 {
		t.Fatalf("one starts, the other is told it already runs: %+v", results)
	}

	// while it runs, the couple is in flight and the app is listed
	<-runner.started
	if !forcedInFlight("nextcloud", "offsite") {
		t.Fatal("in flight while it runs")
	}
	if ops := RunningOperations(); len(ops) != 1 || ops[0].Kind != "backup" {
		t.Fatalf("listed as a backup: %+v", ops)
	}

	close(runner.release)
	forcedRuns.Wait()

	if len(runner.runs()) != 1 || forcedInFlight("nextcloud", "offsite") || len(RunningOperations()) != 0 {
		t.Fatalf("one run, then nothing claimed: %v", runner.runs())
	}
}

func TestForceUnreadableOrUninstalledScheduleIsRefusedWithReason(t *testing.T) {
	schedulerInTempDir(t)

	broken := daily(time.Time{})
	broken.App, broken.At = "immich", "three"
	gone := daily(time.Time{})
	gone.App = "uninstalled"
	_ = SaveBackupSchedules([]BackupSchedule{broken, gone})

	runner := &gateRunner{fail: errors.New("`uninstalled` is scheduled for backup but is not installed any more")}
	result, err := ForceSchedules(context.Background(), ForceRequest{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	forcedRuns.Wait()

	if len(result.Refused) != 1 || result.Refused[0].App != "immich" ||
		!strings.HasPrefix(result.Refused[0].Reason, "this schedule cannot be read: ") {
		t.Fatalf("the unreadable one is refused with why: %+v", result.Refused)
	}
	if got := apps(runner.runs()); len(got) != 1 || got[0] != "uninstalled@offsite" {
		t.Fatalf("only the readable one runs: %v", got)
	}

	records, _ := BackupRuns()
	if len(records) != 1 || records[0].Succeeded() || !strings.Contains(records[0].Error, "not installed") {
		t.Fatalf("the uninstalled app is a failed run, as for the tick: %+v", records)
	}
}

func TestNoMatchingScheduleIs404(t *testing.T) {
	schedulerInTempDir(t)
	_ = SaveBackupSchedules([]BackupSchedule{daily(time.Time{})})

	for _, request := range []ForceRequest{{App: "immich"}, {Destination: "elsewhere"}, {App: "nextcloud", Destination: "elsewhere"}} {
		if _, err := ForceSchedules(context.Background(), request, &gateRunner{}); !errors.Is(err, ErrNoSuchSchedule) {
			t.Fatalf("%+v: want ErrNoSuchSchedule, got %v", request, err)
		}
	}

	// a filter that finds only a switched-off schedule says so instead
	off := daily(time.Time{})
	off.Enabled = false
	_ = SaveBackupSchedules([]BackupSchedule{off})

	runner := &gateRunner{}
	result, err := ForceSchedules(context.Background(), ForceRequest{App: "nextcloud"}, runner)
	forcedRuns.Wait()
	if err != nil || len(result.Started) != 0 || len(result.Refused) != 1 ||
		result.Refused[0].Reason != "this schedule is switched off" || len(runner.runs()) != 0 {
		t.Fatalf("refused as switched off: %+v %v", result, err)
	}
}

func TestNoBodyForcesEveryEnabledSchedule(t *testing.T) {
	schedulerInTempDir(t)

	schedules := threeDue()
	schedules[1].Enabled = false // jellyfin
	_ = SaveBackupSchedules(schedules)

	runner := &gateRunner{}
	result, err := ForceSchedules(context.Background(), ForceRequest{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	forcedRuns.Wait()

	if got := strings.Join(apps(result.Started), ","); got != "immich@offsite,nextcloud@offsite" {
		t.Fatalf("the enabled ones start: %s", got)
	}
	if len(result.Refused) != 0 {
		t.Fatalf("a whole-box force does not list what is switched off: %+v", result.Refused)
	}
	if len(runner.runs()) != 2 {
		t.Fatalf("%v", apps(runner.runs()))
	}
}

func TestTickLeavesAForcedCoupleForTheNextTick(t *testing.T) {
	schedulerInTempDir(t)
	_ = SaveBackupSchedules([]BackupSchedule{daily(time.Time{})})

	forcedRunner := &gateRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	if _, err := ForceSchedules(context.Background(), ForceRequest{}, forcedRunner); err != nil {
		t.Fatal(err)
	}
	<-forcedRunner.started

	tickRunner := &fakeRunner{}
	RunDueBackups(context.Background(), at(10, 7, 0), tickRunner)

	if len(tickRunner.ran) != 0 {
		t.Fatalf("the tick leaves the forced couple alone: %v", tickRunner.ran)
	}
	schedules, _ := BackupSchedules()
	if !schedules[0].LastRun.IsZero() {
		t.Fatalf("and records nothing: %v", schedules[0].LastRun)
	}

	close(forcedRunner.release)
	forcedRuns.Wait()

	// the next tick finds the slot still unrun
	RunDueBackups(context.Background(), at(10, 7, 1), tickRunner)
	if len(tickRunner.ran) != 1 {
		t.Fatalf("the next tick runs it: %v", tickRunner.ran)
	}
}
