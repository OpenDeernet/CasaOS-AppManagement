package service

import (
	"testing"
	"time"
)

func at(day int, hour, minute int) time.Time {
	// a Wednesday, so the weekly arithmetic has somewhere to step back to
	return time.Date(2026, 9, day, hour, minute, 0, 0, time.UTC)
}

func daily(lastRun time.Time) BackupSchedule {
	return BackupSchedule{
		App: "nextcloud", Destination: "offsite",
		Every: ScheduleDaily, At: "03:00", Enabled: true, LastRun: lastRun,
	}
}

// A box asleep at 3am must find its 3am slot still unfired when it wakes at 7 and
// run then, rather than skipping the day because the moment has passed. That is
// why the slot is looked for BACKWARDS.
func TestAMissedSlotCatchesUp(t *testing.T) {
	// last ran yesterday morning; it is now 07:00, well past today's 03:00
	due, err := IsDue(daily(at(9, 3, 0)), at(10, 7, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("today's slot has not run")
	}
}

func TestASlotAlreadyRunDoesNotRunAgain(t *testing.T) {
	// ran at 03:00 today, and it is 07:00 today
	due, err := IsDue(daily(at(10, 3, 0)), at(10, 7, 0))
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Fatal("a run recorded exactly at its slot has run it")
	}

	// and an hour later, still not
	due, _ = IsDue(daily(at(10, 3, 0)), at(10, 23, 59))
	if due {
		t.Fatal("once a day means once")
	}
}

// Before 03:00 the day's slot has not arrived, so yesterday's is the one that
// counts -- and it already ran.
func TestBeforeTheTimeOfDayNothingIsDue(t *testing.T) {
	due, err := IsDue(daily(at(9, 3, 0)), at(10, 2, 59))
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Fatal("yesterday's slot ran, today's has not come")
	}
}

// A schedule that has never run has its most recent slot outstanding, which is
// what makes a new one start rather than wait a whole period.
func TestASchedulePicksUpItsFirstSlotImmediately(t *testing.T) {
	due, err := IsDue(daily(time.Time{}), at(10, 7, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("a new schedule runs at the next check, not the next period")
	}
}

func TestWeeklyStepsBackToItsWeekday(t *testing.T) {
	// 2026-09-10 is a Thursday; ask for Mondays at 03:00
	weekly := BackupSchedule{
		App: "a", Destination: "b", Every: ScheduleWeekly, At: "03:00",
		Weekday: time.Monday, Enabled: true,
	}

	// last ran on the Monday, and it is Thursday: that slot is done
	weekly.LastRun = at(7, 3, 0)
	if due, _ := IsDue(weekly, at(10, 7, 0)); due {
		t.Fatal("Monday's slot ran; the next is next Monday")
	}

	// last ran the week before: this Monday is outstanding
	weekly.LastRun = at(1, 3, 0)
	if due, _ := IsDue(weekly, at(10, 7, 0)); !due {
		t.Fatal("this week's Monday has not run")
	}
}

func TestADisabledScheduleIsNeverDue(t *testing.T) {
	schedule := daily(time.Time{})
	schedule.Enabled = false

	if due, _ := IsDue(schedule, at(10, 7, 0)); due {
		t.Fatal("disabled is disabled")
	}
}

// Silently ignoring a schedule nobody can parse is how a box backs nothing up for
// months while its interface says it is scheduled.
func TestABrokenScheduleIsReportedRatherThanSkipped(t *testing.T) {
	good := daily(time.Time{})
	bad := daily(time.Time{})
	bad.At = "3am"
	worse := daily(time.Time{})
	worse.Every = "hourly"
	nameless := daily(time.Time{})
	nameless.App = ""

	due, broken := DueSchedules([]BackupSchedule{good, bad, worse, nameless}, at(10, 7, 0))

	if len(due) != 1 || due[0].App != "nextcloud" {
		t.Fatalf("the good one still runs: %+v", due)
	}
	if len(broken) != 3 {
		t.Fatalf("want three complaints, got %d: %v", len(broken), broken)
	}
}

func TestTheTimeOfDayIsReadStrictly(t *testing.T) {
	for _, bad := range []string{"", "3", "3am", "25:00", "03:60", "03:0x", "0300", "03:00:00"} {
		schedule := daily(time.Time{})
		schedule.At = bad
		if _, err := IsDue(schedule, at(10, 7, 0)); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}

	for _, good := range []string{"00:00", "03:00", "23:59", " 03:00 "} {
		schedule := daily(time.Time{})
		schedule.At = good
		if _, err := IsDue(schedule, at(10, 7, 0)); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

// Two runs of the same list answer in the same order, or a log of what ran reads
// differently every time.
func TestDueSchedulesAnswerInAStableOrder(t *testing.T) {
	schedules := []BackupSchedule{
		{App: "zzz", Destination: "a", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: "aaa", Destination: "z", Every: ScheduleDaily, At: "03:00", Enabled: true},
		{App: "aaa", Destination: "a", Every: ScheduleDaily, At: "03:00", Enabled: true},
	}

	for run := 0; run < 20; run++ {
		due, _ := DueSchedules(schedules, at(10, 7, 0))
		if len(due) != 3 {
			t.Fatalf("want 3, got %d", len(due))
		}
		if due[0].App != "aaa" || due[0].Destination != "a" || due[2].App != "zzz" {
			t.Fatalf("run %d: %+v", run, due)
		}
	}
}

// inParis makes the box's zone Paris for one test, because the slots are wall-clock
// times and a DST change only exists in a zone that has one.
func inParis(t *testing.T) *time.Location {
	t.Helper()

	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}

	was := time.Local
	time.Local = paris
	t.Cleanup(func() { time.Local = was })

	return paris
}

func TestADailyScheduleSaysItsLastAndNextSlot(t *testing.T) {
	paris := inParis(t)
	now := time.Date(2026, 9, 9, 7, 0, 0, 0, paris)

	last, next, err := ScheduleSlots(daily(time.Time{}), now)
	if err != nil {
		t.Fatal(err)
	}

	if want := time.Date(2026, 9, 9, 3, 0, 0, 0, paris); !last.Equal(want) {
		t.Fatalf("last %v, want %v", last, want)
	}
	if want := time.Date(2026, 9, 10, 3, 0, 0, 0, paris); !next.Equal(want) {
		t.Fatalf("next %v, want %v", next, want)
	}
}

func TestAWeeklyScheduleSaysItsLastAndNextSlot(t *testing.T) {
	paris := inParis(t)
	// a Wednesday, on a schedule for Mondays
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, paris)
	schedule := BackupSchedule{Every: ScheduleWeekly, Weekday: time.Monday, At: "03:00", Enabled: true}

	last, next, err := ScheduleSlots(schedule, now)
	if err != nil {
		t.Fatal(err)
	}

	if want := time.Date(2026, 9, 7, 3, 0, 0, 0, paris); !last.Equal(want) {
		t.Fatalf("last %v, want %v", last, want)
	}
	if want := time.Date(2026, 9, 14, 3, 0, 0, 0, paris); !next.Equal(want) {
		t.Fatalf("next %v, want %v", next, want)
	}
}

func TestSlotsOfAnUnreadableTimeOfDayAreAnError(t *testing.T) {
	inParis(t)

	schedule := daily(time.Time{})
	schedule.At = "three"

	if _, _, err := ScheduleSlots(schedule, at(10, 7, 0)); err == nil {
		t.Fatal("want an error")
	}
}

// The slot is a wall-clock time. The night the clocks go forward is 23 hours long,
// and "03:00 tomorrow" is still 03:00 on the wall, not 04:00.
func TestTheNextSlotKeepsTheWallClockAcrossDST(t *testing.T) {
	paris := inParis(t)
	now := time.Date(2026, 3, 28, 7, 0, 0, 0, paris) // the day before spring-forward

	last, next, err := ScheduleSlots(daily(time.Time{}), now)
	if err != nil {
		t.Fatal(err)
	}

	if want := time.Date(2026, 3, 29, 3, 0, 0, 0, paris); !next.Equal(want) {
		t.Fatalf("next %v, want %v", next, want)
	}
	if next.Sub(last) != 23*time.Hour {
		t.Fatalf("the day is %v long, want 23h", next.Sub(last))
	}
}

// 02:30 does not exist on 2026-03-29 in Paris. Whatever Go makes of it, the slot
// has to stay on that calendar day, and the days around it must not inherit the
// shifted hour.
func TestASlotInTheHourThatDoesNotExistStaysOnItsDay(t *testing.T) {
	paris := inParis(t)
	schedule := daily(time.Time{})
	schedule.At = "02:30"

	// after the missing hour: the slot of the day itself
	last, next, err := ScheduleSlots(schedule, time.Date(2026, 3, 29, 12, 0, 0, 0, paris))
	if err != nil {
		t.Fatal(err)
	}
	if y, m, d := last.Date(); y != 2026 || m != 3 || d != 29 {
		t.Fatalf("last %v is not on the 29th", last)
	}
	if want := time.Date(2026, 3, 30, 2, 30, 0, 0, paris); !next.Equal(want) {
		t.Fatalf("next %v, want %v", next, want)
	}

	// before it: the slot of the day before, at its own 02:30, and the next one on the 29th
	last, next, err = ScheduleSlots(schedule, time.Date(2026, 3, 29, 1, 0, 0, 0, paris))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 3, 28, 2, 30, 0, 0, paris); !last.Equal(want) {
		t.Fatalf("last %v, want %v", last, want)
	}
	if y, m, d := next.Date(); y != 2026 || m != 3 || d != 29 {
		t.Fatalf("next %v is not on the 29th", next)
	}
}
