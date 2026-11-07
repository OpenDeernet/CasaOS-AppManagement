package service

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"path"
	"strings"
	"testing"
)

type fakeUnits struct {
	stopped, started, restarted []string
	failStop                    string
}

func (f *fakeUnits) Stop(unit string) error {
	if unit == f.failStop {
		return errors.New("systemd said no")
	}
	f.stopped = append(f.stopped, unit)

	return nil
}

func (f *fakeUnits) Start(unit string) error   { f.started = append(f.started, unit); return nil }
func (f *fakeUnits) Restart(unit string) error { f.restarted = append(f.restarted, unit); return nil }

// What the box is: a fixed list, planned like bind mounts, with the absent ones
// skipped and said so.
func TestTheBoxIsAFixedListAndAbsentPartsAreSkippedWithAReason(t *testing.T) {
	present := map[string]bool{"/etc/casaos": true, "/var/lib/casaos/db": true, "/root/.config/rclone/rclone.conf": true}
	manifest := PlanBackup(SystemBackupName, SystemBackupInventory(func(p string) bool { return present[p] }), nil)

	if len(manifest.Operations) != 3 {
		t.Fatalf("three present: %+v", manifest.Operations)
	}
	if len(manifest.Skipped) != 3 || !strings.Contains(manifest.Skipped[0].Skip, "not present") {
		t.Fatalf("three absent, with the reason: %+v", manifest.Skipped)
	}
	for _, op := range manifest.Operations {
		if op.Kind != BackupKindBind || !strings.HasPrefix(op.Destination, "binds/") {
			t.Fatalf("planned like a bind mount: %+v", op)
		}
	}
}

// The services holding the files are stopped for the copy, in order, and
// started again in reverse -- and started again when the copy fails, too.
func TestTheServicesAreStoppedForTheCopyAndStartedAfter(t *testing.T) {
	logInTempDir(t)
	units := &fakeUnits{}
	copier := &fakeCopier{}

	if _, err := RunSystemBackup(context.Background(), copier, units, SystemBackupOptions{
		Destination: "offsite", Stamp: restoreStamp, HoldStill: true,
	}); err != nil {
		t.Fatal(err)
	}

	if strings.Join(units.stopped, ",") != "casaos,casaos-user-service,casaos-local-storage" {
		t.Fatalf("stopped %v", units.stopped)
	}
	if strings.Join(units.started, ",") != "casaos-local-storage,casaos-user-service,casaos" {
		t.Fatalf("started back in reverse: %v", units.started)
	}

	// and the manifest went last, under the box's own name
	last := copier.calls[len(copier.calls)-1]
	if !strings.HasSuffix(last.destination, SystemBackupName+"/"+restoreStamp+"/manifest.json") {
		t.Fatalf("%+v", last)
	}
}

func TestAServiceThatWillNotStopMeansNoCopyAndTheOthersComeBack(t *testing.T) {
	logInTempDir(t)
	units := &fakeUnits{failStop: "casaos-user-service"}
	copier := &fakeCopier{}

	_, err := RunSystemBackup(context.Background(), copier, units, SystemBackupOptions{
		Destination: "offsite", Stamp: restoreStamp, HoldStill: true,
	})
	if err == nil || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("want the refusal, got %v", err)
	}
	if len(copier.calls) != 0 {
		t.Fatalf("nothing copied: %+v", copier.calls)
	}
	if strings.Join(units.started, ",") != "casaos" {
		t.Fatalf("what went down comes back: %v", units.started)
	}
}

// A restore puts things at this box's paths, never the manifest's, stops the
// services for it, and restarts rclone last when its configuration came back.
func TestABoxRestoreUsesThisBoxesPathsAndRestartsRcloneLast(t *testing.T) {
	logInTempDir(t)

	// a manifest as another box wrote it, with its own idea of where things were
	manifest := PlanBackup(SystemBackupName, SystemBackupInventory(func(string) bool { return true }), nil)
	for i := range manifest.Operations {
		manifest.Operations[i].Directory = !strings.HasSuffix(manifest.Operations[i].Source, ".conf")
		manifest.Operations[i].Source = "/elsewhere" + manifest.Operations[i].Source
	}
	raw, _ := stdjson.Marshal(manifest)
	root := RootFor(SystemBackupName, restoreStamp)
	restorer := &fakeRestorer{files: map[string][]byte{path.Join(root, ManifestFileName): raw}}
	units := &fakeUnits{}

	report, err := RestoreSystemBackup(context.Background(), restorer, units, SystemRestoreOptions{Destination: "offsite", Stamp: restoreStamp})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Restored) != len(systemBackupPaths()) {
		t.Fatalf("everything the backup held: %+v", report)
	}
	for _, call := range restorer.restored {
		if strings.HasPrefix(call.host, "/elsewhere") {
			t.Fatalf("the manifest's path was used: %+v", call)
		}
	}
	if strings.Join(units.stopped, ",") != "casaos,casaos-user-service,casaos-local-storage" || len(units.started) != 3 {
		t.Fatalf("held still and started again: stopped %v started %v", units.stopped, units.started)
	}
	if strings.Join(units.restarted, ",") != "rclone" {
		t.Fatalf("rclone restarted, last: %v", units.restarted)
	}
}

// A backup of an app is not a backup of the box, whatever it is asked to be.
func TestABackupOfAnAppIsNotPouredIntoTheBox(t *testing.T) {
	logInTempDir(t)

	manifest := BackupManifest{FormatVersion: BackupFormatVersion, App: "nextcloud"}
	raw, _ := stdjson.Marshal(manifest)
	root := RootFor(SystemBackupName, restoreStamp)
	restorer := &fakeRestorer{files: map[string][]byte{path.Join(root, ManifestFileName): raw}}

	_, err := RestoreSystemBackup(context.Background(), restorer, &fakeUnits{}, SystemRestoreOptions{Destination: "offsite", Stamp: restoreStamp})
	if err == nil || !strings.Contains(err.Error(), "not of this box") {
		t.Fatalf("want the refusal, got %v", err)
	}
}

// The numbers in a backup are given out over the parts that were present when
// it was taken. A box without a Samba config had its database folder as
// binds/2; paired by number with this box's second path, that folder went where
// the Samba config goes and the next one over the database. Each part goes back
// to its own path, whatever number the copy landed under.
func TestEachPartOfTheBoxGoesBackToItsOwnPathWhateverNumberItLandedUnder(t *testing.T) {
	logInTempDir(t)

	// taken on a box with no Samba config: five parts, numbered 1..5
	present := func(p string) bool { return p != "/etc/samba/smb.casa.conf" }
	manifest := PlanBackup(SystemBackupName, SystemBackupInventory(present), nil)
	for i := range manifest.Operations {
		manifest.Operations[i].Directory = !strings.HasSuffix(manifest.Operations[i].Source, ".conf")
	}
	raw, _ := stdjson.Marshal(manifest)
	root := RootFor(SystemBackupName, restoreStamp)
	restorer := &fakeRestorer{files: map[string][]byte{path.Join(root, ManifestFileName): raw}}

	report, err := RestoreSystemBackup(context.Background(), restorer, &fakeUnits{}, SystemRestoreOptions{Destination: "offsite", Stamp: restoreStamp})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Restored) != 5 || len(report.Missing) != 0 {
		t.Fatalf("five parts back, nothing missing: %+v", report)
	}
	for _, call := range restorer.restored {
		// binds/2 in this backup is the database folder, and it must land there
		if strings.HasSuffix(call.remote, "binds/2") && call.host != "/var/lib/casaos/db" {
			t.Fatalf("binds/2 went to %s", call.host)
		}
		if call.host == "/etc/samba/smb.casa.conf" {
			t.Fatalf("nothing was in the backup for the Samba config, so nothing may be written there: %+v", call)
		}
		if strings.HasSuffix(call.host, "rclone.conf") && call.kind != "file" {
			t.Fatalf("a file comes back as a file: %+v", call)
		}
	}
}
