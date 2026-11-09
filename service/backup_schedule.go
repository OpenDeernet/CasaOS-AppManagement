package service

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// When a backup runs by itself, and how many are kept.
//
// The arithmetic lives here, away from anything that reads a clock or a disk,
// because "was this due?" is the question a scheduler gets wrong quietly: a rule
// that misfires runs a backup twice, and one that never fires produces the worst
// outcome in this whole feature -- a box that has been backing nothing up for
// months, with an interface that says it is scheduled.

// ScheduleEvery is how often a schedule comes round.
type ScheduleEvery string

const (
	ScheduleDaily  ScheduleEvery = "daily"
	ScheduleWeekly ScheduleEvery = "weekly"
)

// BackupSchedule is one standing arrangement: this app, to this destination, at
// this time.
type BackupSchedule struct {
	App         string        `json:"app"`
	Destination string        `json:"destination"`
	Every       ScheduleEvery `json:"every"`
	// At is local time, "HH:MM". Local because somebody chose it looking at their
	// own clock, and "back up at 3am" means their 3am.
	At string `json:"at"`
	// Weekday applies to weekly only.
	Weekday time.Weekday `json:"weekday"`
	// Keep is how many runs survive at the destination. Zero keeps everything,
	// which is the safe reading of a field nobody filled in -- deleting backups is
	// not something to do because a number defaulted.
	Keep int `json:"keep"`
	// HoldStill stops the app for the copy.
	HoldStill bool `json:"hold_still"`
	Enabled   bool `json:"enabled"`
	// LastRun is when this last went off, and the only thing that stops it going
	// off again for the same slot.
	LastRun time.Time `json:"last_run"`
}

// parseAt reads "HH:MM".
func parseAt(at string) (hour int, minute int, err error) {
	parts := strings.Split(strings.TrimSpace(at), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("a time of day is written HH:MM, not %q", at)
	}

	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("%q is not an hour", parts[0])
	}

	minute, err = strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q is not a minute", parts[1])
	}

	return hour, minute, nil
}

// mostRecentSlot is the last moment this schedule was supposed to fire, at or
// before now.
//
// Looking backwards rather than forwards is what makes a missed run catch up: a
// box asleep at 3am finds its 3am slot still unfired when it wakes at 7 and runs
// then, instead of skipping the day because the moment has passed.
func mostRecentSlot(schedule BackupSchedule, now time.Time) (time.Time, error) {
	hour, minute, err := parseAt(schedule.At)
	if err != nil {
		return time.Time{}, err
	}

	// Every slot is built from its calendar day and the wall-clock time, never by
	// adding days to another slot: on the night the clocks go forward "02:30" does
	// not exist, comes out as 03:30, and AddDate would carry that hour to every
	// other day.
	slotOn := func(daysFromToday int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day()+daysFromToday, hour, minute, 0, 0, now.Location())
	}

	slot := slotOn(0)

	switch schedule.Every {
	case ScheduleDaily:
		if slot.After(now) {
			slot = slotOn(-1)
		}

		return slot, nil

	case ScheduleWeekly:
		// step back to the chosen weekday
		back := (int(slot.Weekday()) - int(schedule.Weekday) + 7) % 7
		slot = slotOn(-back)
		if slot.After(now) {
			slot = slotOn(-back - 7)
		}

		return slot, nil
	}

	return time.Time{}, fmt.Errorf("%q is not a frequency this understands", schedule.Every)
}

// ScheduleSlots is the last slot a schedule was due at, and the one after it.
func ScheduleSlots(schedule BackupSchedule, now time.Time) (last, next time.Time, err error) {
	last, err = mostRecentSlot(schedule, now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	// already read once by mostRecentSlot
	hour, minute, _ := parseAt(schedule.At)

	days := 1
	if schedule.Every == ScheduleWeekly {
		days = 7
	}

	// The calendar day plus the wall-clock time, not last.AddDate: when last is the
	// shifted slot of the missing hour, AddDate would carry the shift forward.
	next = time.Date(last.Year(), last.Month(), last.Day()+days, hour, minute, 0, 0, last.Location())

	return last, next, nil
}

// IsDue reports whether this schedule has a slot it has not run yet.
func IsDue(schedule BackupSchedule, now time.Time) (bool, error) {
	if !schedule.Enabled {
		return false, nil
	}
	if schedule.App == "" || schedule.Destination == "" {
		return false, fmt.Errorf("a schedule needs an app and a destination")
	}

	slot, err := mostRecentSlot(schedule, now)
	if err != nil {
		return false, err
	}

	// Before, not !After: a run recorded exactly at its slot has run it.
	return schedule.LastRun.Before(slot), nil
}

// DueSchedules is every schedule with an unrun slot, in a stable order.
//
// A schedule whose time of day cannot be read is reported rather than skipped:
// silently ignoring it is how a box backs nothing up for months while its
// interface says it is scheduled.
func DueSchedules(schedules []BackupSchedule, now time.Time) (due []BackupSchedule, broken []error) {
	for _, schedule := range schedules {
		isDue, err := IsDue(schedule, now)
		if err != nil {
			broken = append(broken, fmt.Errorf("%s to %s: %w", schedule.App, schedule.Destination, err))
			continue
		}

		if isDue {
			due = append(due, schedule)
		}
	}

	sort.Slice(due, func(i, j int) bool {
		if due[i].App != due[j].App {
			return due[i].App < due[j].App
		}

		return due[i].Destination < due[j].Destination
	})

	return due, broken
}
