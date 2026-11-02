package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
)

type fakeChecker struct {
	destinations []string
	kept         map[string]bool
	fault        map[string]error
	checked      []string
}

func (f *fakeChecker) Destinations() ([]string, error) { return f.destinations, nil }
func (f *fakeChecker) Held(destination string) bool    { return f.kept[destination] }
func (f *fakeChecker) Check(_ context.Context, destination string) error {
	f.checked = append(f.checked, destination)

	return f.fault[destination]
}

// checksInTempDir gives the checks a file of their own and no operating system upgrade; the
// flag it returns is how a test opens one.
func checksInTempDir(t *testing.T) *upgradeFlag {
	t.Helper()

	logger.LogInitConsoleOnly()
	flag := upgradeFlagIn(t)

	was := ResticCheckPath
	ResticCheckPath = filepath.Join(t.TempDir(), "restic-checks.json")
	t.Cleanup(func() { ResticCheckPath = was })

	return flag
}

var checkDay = time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)

func TestOnlyTheDestinationsKeptWithResticAreChecked(t *testing.T) {
	checksInTempDir(t)
	checker := &fakeChecker{destinations: []string{"nas", "cloud"}, kept: map[string]bool{"nas": true}}

	RunDueResticChecks(context.Background(), checkDay, checker)

	if len(checker.checked) != 1 || checker.checked[0] != "nas" {
		t.Fatalf("a stamped-copy destination has no repository to check: %v", checker.checked)
	}
	checks, err := ResticChecks()
	if err != nil {
		t.Fatal(err)
	}
	if got := checks["nas"]; !got.OK || !got.At.Equal(checkDay) {
		t.Fatalf("the check is written down: %+v", got)
	}
}

func TestACheckIsDueAWeekAfterTheLastOneAndNotBefore(t *testing.T) {
	checksInTempDir(t)
	checker := &fakeChecker{destinations: []string{"nas"}, kept: map[string]bool{"nas": true}}

	RunDueResticChecks(context.Background(), checkDay, checker)
	RunDueResticChecks(context.Background(), checkDay.Add(6*24*time.Hour), checker)
	if len(checker.checked) != 1 {
		t.Fatalf("not yet a week: %v", checker.checked)
	}

	RunDueResticChecks(context.Background(), checkDay.Add(7*24*time.Hour), checker)
	if len(checker.checked) != 2 {
		t.Fatalf("a week: %v", checker.checked)
	}
}

func TestAFaultIsRecordedAndLookedAtAgainTheNextDay(t *testing.T) {
	checksInTempDir(t)
	checker := &fakeChecker{
		destinations: []string{"nas"}, kept: map[string]bool{"nas": true},
		fault: map[string]error{"nas": errors.New("pack 1a2b: does not exist")},
	}

	RunDueResticChecks(context.Background(), checkDay, checker)

	checks, _ := ResticChecks()
	if got := checks["nas"]; got.OK || got.Error != "pack 1a2b: does not exist" {
		t.Fatalf("what restic said is kept: %+v", got)
	}

	RunDueResticChecks(context.Background(), checkDay.Add(23*time.Hour), checker)
	if len(checker.checked) != 1 {
		t.Fatalf("a failing check is not repeated every tick: %v", checker.checked)
	}

	checker.fault = nil
	RunDueResticChecks(context.Background(), checkDay.Add(24*time.Hour), checker)
	checks, _ = ResticChecks()
	if len(checker.checked) != 2 || !checks["nas"].OK || checks["nas"].Error != "" {
		t.Fatalf("a day after the fault it is looked at again, and the mended repository says so: %v %+v", checker.checked, checks["nas"])
	}
}

func TestNothingIsCheckedWhileAnOperatingSystemUpgradeIsOpen(t *testing.T) {
	flag := checksInTempDir(t)
	flag.present = true
	checker := &fakeChecker{destinations: []string{"nas"}, kept: map[string]bool{"nas": true}}

	RunDueResticChecks(context.Background(), checkDay, checker)

	if len(checker.checked) != 0 {
		t.Fatalf("a check reads the repository while the machine is being upgraded: %v", checker.checked)
	}
}

func TestACheckAskedForByHandRefusesWhileAnotherRuns(t *testing.T) {
	checksInTempDir(t)
	checker := &fakeChecker{destinations: []string{"nas"}, kept: map[string]bool{"nas": true}}

	resticCheckBusy.Lock()
	_, started := StartResticCheck(context.Background(), checkDay, checker, "nas")
	resticCheckBusy.Unlock()
	if started || len(checker.checked) != 0 {
		t.Fatal("one check at a time: each reads the whole repository")
	}

	done, started := StartResticCheck(context.Background(), checkDay, checker, "nas")
	if !started {
		t.Fatal("when none runs, the check starts")
	}
	<-done
	if len(checker.checked) != 1 {
		t.Fatalf("and it runs: %v", checker.checked)
	}
	again, started := StartResticCheck(context.Background(), checkDay, checker, "nas")
	if !started {
		t.Fatal("the next one can start once the first is over")
	}
	<-again // a check left running would outlive the file this test gave it
}
