package v2

import (
	"encoding/json"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
)

// Go's zero value for a bool is false, and false here means a copy taken from a
// running app -- so a request that simply forgot the field would quietly get the
// kind of backup that does not restore.
func TestAnOmittedHoldStillMeansStopTheApp(t *testing.T) {
	if !holdStillFrom(codegen.BackupRequest{Destination: "offsite"}) {
		t.Fatal("nil must mean the safe answer, not the zero one")
	}

	yes, no := true, false
	if !holdStillFrom(codegen.BackupRequest{HoldStill: &yes}) {
		t.Fatal("asked to stop")
	}
	if holdStillFrom(codegen.BackupRequest{HoldStill: &no}) {
		t.Fatal("asked not to: a hot copy is the caller's to choose")
	}
}

// A stamp names a run inside a destination, is read back by people in file
// browsers, and has to sort.
func TestTheStampSortsAndSurvivesAFilesystem(t *testing.T) {
	earlier := time.Date(2026, 9, 11, 2, 30, 0, 0, time.UTC).Format(stampLayout)
	later := time.Date(2026, 9, 11, 14, 5, 59, 0, time.UTC).Format(stampLayout)

	if earlier >= later {
		t.Fatalf("runs must sort by name: %q then %q", earlier, later)
	}

	// a colon is not a character every destination accepts in a path
	for _, bad := range []string{":", "/", "\\", " "} {
		if strings.Contains(later, bad) {
			t.Fatalf("%q contains %q", later, bad)
		}
	}
}

// The API's record and the service's record carry the same JSON tags, so the
// two encodings of one run must be identical. A field added to the record and
// forgotten in backupRunOut shows up here as a difference, which is how the
// restore flag went missing from the API on its first release.
func TestEveryFieldOfARunReachesTheAPI(t *testing.T) {
	now := time.Date(2026, 9, 13, 3, 7, 6, 0, time.UTC)
	record := service.BackupRunRecord{
		App: "smoke", Destination: "offsite", Stamp: "2026-09-13T03-06-54Z",
		StartedAt: now, FinishedAt: now.Add(3 * time.Second),
		Scheduled: true, ContainersStopped: true, Copied: 2, SkippedCount: 1, Bytes: 3,
		Error: "nope", Restore: true,
	}

	// keys, not bytes: the generated type lists its fields alphabetically
	asMap := func(v interface{}) map[string]interface{} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]interface{}{}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	want, got := asMap(record), asMap(backupRunOut(record))
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("a field was dropped on the way out -- service: %v -- api: %v", want, got)
	}
}

// An enabled schedule says when it last and next goes off, with the box's UTC
// offset so the interface does not have to guess the zone; a disabled one, or one
// whose time of day cannot be read, says nothing rather than failing the listing.
func TestSchedulesSayTheirSlots(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	now := time.Date(2026, 9, 9, 7, 0, 0, 0, paris)

	schedule := service.BackupSchedule{
		App: "nextcloud", Destination: "offsite",
		Every: service.ScheduleDaily, At: "03:00", Enabled: true,
	}

	out := toCodegenSchedule(schedule, now)
	if out.LastSlot == nil || out.NextSlot == nil {
		t.Fatalf("an enabled schedule gave %v and %v", out.LastSlot, out.NextSlot)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"last_slot":"2026-09-09T03:00:00+02:00"`, `"next_slot":"2026-09-10T03:00:00+02:00"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s lacks %s", raw, want)
		}
	}

	schedule.Enabled = false
	if out := toCodegenSchedule(schedule, now); out.LastSlot != nil || out.NextSlot != nil {
		t.Fatalf("a disabled schedule gave %v and %v", out.LastSlot, out.NextSlot)
	}

	schedule.Enabled, schedule.At = true, "three"
	if out := toCodegenSchedule(schedule, now); out.LastSlot != nil || out.NextSlot != nil {
		t.Fatalf("an unreadable schedule gave %v and %v", out.LastSlot, out.NextSlot)
	}
}
