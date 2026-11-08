package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/compose-spec/compose-go/v2/types"
)

type copied struct {
	kind        string // "dir" or "file"
	source      string
	destination string
}

type fakeCopier struct {
	calls  []copied
	failOn string
}

func (f *fakeCopier) CopyDirectory(_ context.Context, source, _, destination string) error {
	if f.failOn == source {
		return errors.New("destination refused it")
	}
	f.calls = append(f.calls, copied{"dir", source, destination})

	return nil
}

func (f *fakeCopier) CopyFile(_ context.Context, source, _, destination string) error {
	if f.failOn == source {
		return errors.New("destination refused it")
	}
	f.calls = append(f.calls, copied{"file", source, destination})

	return nil
}

type fakeBackupDocker struct {
	fakeDocker
	mountpoints map[string]string
	asked       []string
}

func (f *fakeBackupDocker) VolumeMountpoints(_ context.Context, names []string) map[string]string {
	f.asked = append(f.asked, names...)
	out := map[string]string{}
	for _, name := range names {
		if m, ok := f.mountpoints[name]; ok {
			out[name] = m
		}
	}

	return out
}

func appWithOneBindAndOneVolume(t *testing.T) (*ComposeApp, string) {
	t.Helper()

	dir := t.TempDir()
	slash := filepath.ToSlash(dir)

	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("name: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// most apps have no .env; this one does, so the copy counts below include it
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}

	return &ComposeApp{
		Name:         "demo",
		WorkingDir:   slash,
		ComposeFiles: []string{slash + "/docker-compose.yml"},
		Volumes:      types.Volumes{"data": types.VolumeConfig{Name: "demo_data"}},
		Services: types.Services{
			"app": types.ServiceConfig{Name: "app", Volumes: []types.ServiceVolumeConfig{
				{Type: "bind", Source: "./config", Target: "/config"},
				{Type: "volume", Source: "data", Target: "/data"},
			}},
		},
	}, slash
}

func TestARunCopiesEveryOperationThenTheManifest(t *testing.T) {
	app, dir := appWithOneBindAndOneVolume(t)
	docker := &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/var/lib/docker/volumes/demo_data/_data"}}
	copier := &fakeCopier{}

	manifest, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{
		Destination: "offsite", Stamp: "2026-09-11T02-30-00Z",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Docker is asked by the name it knows the volume by, not by the compose name
	if strings.Join(docker.asked, ",") != "demo_data" {
		t.Fatalf("asked for %v", docker.asked)
	}

	// compose, .env, the bind, the volume, then the manifest
	if len(copier.calls) != 5 {
		t.Fatalf("want 5 copies, got %d: %+v", len(copier.calls), copier.calls)
	}

	last := copier.calls[len(copier.calls)-1]
	if last.kind != "file" || !strings.HasSuffix(last.destination, "demo/2026-09-11T02-30-00Z/manifest.json") {
		t.Fatalf("the manifest goes last, and at the root: %+v", last)
	}

	// the volume comes from where Docker put it
	found := false
	for _, call := range copier.calls {
		if call.source == "/var/lib/docker/volumes/demo_data/_data" {
			found = true
			if call.destination != "demo/2026-09-11T02-30-00Z/volumes/data" {
				t.Fatalf("volume destination: %q", call.destination)
			}
		}
	}
	if !found {
		t.Fatal("the volume was not copied from its mountpoint")
	}

	// the bind is a real folder on disk, so it is copied as one
	for _, call := range copier.calls {
		if call.source == dir+"/config" && call.kind != "dir" {
			t.Fatalf("a folder is copied as a folder: %+v", call)
		}
	}

	if manifest.ContainersStopped {
		t.Fatal("this run did not ask to hold the app still")
	}
}

// An interrupted run leaves files and no manifest, which reads as what it is --
// an incomplete backup -- rather than a complete one missing its contents.
func TestAFailedCopyWritesNoManifest(t *testing.T) {
	app, dir := appWithOneBindAndOneVolume(t)
	docker := &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/var/lib/docker/volumes/demo_data/_data"}}
	copier := &fakeCopier{failOn: dir + "/config"}

	_, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{
		Destination: "offsite", Stamp: "run",
	})
	if err == nil {
		t.Fatal("want the failure")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Fatalf("say what could not be copied: %v", err)
	}

	for _, call := range copier.calls {
		if strings.HasSuffix(call.destination, ManifestFileName) {
			t.Fatal("no manifest for a backup that did not finish")
		}
	}
}

// The app comes back whatever the copy did -- which is the whole reason the
// release is deferred rather than called at the end.
func TestTheAppComesBackEvenWhenTheCopyFails(t *testing.T) {
	app, dir := appWithOneBindAndOneVolume(t)
	docker := &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/x"}}
	copier := &fakeCopier{failOn: dir + "/config"}

	manifest, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{
		Destination: "offsite", Stamp: "run", HoldStill: true,
		Containers: func(context.Context) (map[string][]codegen.ContainerSummary, error) {
			return map[string][]codegen.ContainerSummary{
				"app": {{ID: "c1", State: "running"}},
			}, nil
		},
	})

	if err == nil {
		t.Fatal("the copy failed, so the run failed")
	}
	if !manifest.ContainersStopped {
		t.Fatal("the manifest records that the app was held still")
	}

	if strings.Join(docker.stopped, ",") != "c1" {
		t.Fatalf("the app was stopped: %v", docker.stopped)
	}
	if strings.Join(docker.started, ",") != "c1" {
		t.Fatalf("and it came back despite the failure: %v", docker.started)
	}
}

// Holding still is what makes a database backup restorable, so a run that asks
// for it and cannot have it must fail rather than quietly take a hot copy.
func TestARunThatCannotHoldTheAppStillDoesNotPretendItDid(t *testing.T) {
	app, _ := appWithOneBindAndOneVolume(t)
	docker := &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/x"}}
	copier := &fakeCopier{}

	_, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{
		Destination: "offsite", Stamp: "run", HoldStill: true,
		Containers: func(context.Context) (map[string][]codegen.ContainerSummary, error) {
			return nil, errors.New("daemon not answering")
		},
	})

	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "not held still") {
		t.Fatalf("say why it refused: %v", err)
	}
	if len(copier.calls) != 0 {
		t.Fatal("and nothing was copied from a running app")
	}
}

func TestARunNeedsADestinationAndAName(t *testing.T) {
	app, _ := appWithOneBindAndOneVolume(t)
	docker := &fakeBackupDocker{}
	copier := &fakeCopier{}

	if _, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{Stamp: "run"}); err == nil {
		t.Error("a backup with no destination goes nowhere")
	}
	if _, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{Destination: "offsite"}); err == nil {
		t.Error("a backup with no name cannot be found again")
	}
	if len(copier.calls) != 0 {
		t.Fatal("neither should have copied anything")
	}
}

// A volume the daemon cannot place is skipped with its reason rather than copied
// from a guess, and the run still produces a backup of everything else.
func TestAVolumeTheDaemonCannotPlaceDoesNotStopTheRest(t *testing.T) {
	app, _ := appWithOneBindAndOneVolume(t)
	docker := &fakeBackupDocker{mountpoints: map[string]string{}}
	copier := &fakeCopier{}

	manifest, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{
		Destination: "offsite", Stamp: "run",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(manifest.Skipped) != 1 || !strings.Contains(manifest.Skipped[0].Skip, "where this volume lives") {
		t.Fatalf("the reason travels with the backup: %+v", manifest.Skipped)
	}

	// compose, .env, the bind, and the manifest -- the volume is absent
	if len(copier.calls) != 4 {
		t.Fatalf("got %d copies: %+v", len(copier.calls), copier.calls)
	}
}

// The first backup ever run end to end, on a fresh install, failed on this:
// the plan listed the app's .env, the app had none, and rclone was sent after
// a file that was not there. An app without one backs up without one.
func TestAnAppWithNoEnvBacksUpWithoutOne(t *testing.T) {
	app, dir := appWithOneBindAndOneVolume(t)
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	docker := &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/var/lib/docker/volumes/demo_data/_data"}}
	copier := &fakeCopier{}

	manifest, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{
		Destination: "offsite", Stamp: "2026-09-13T02-29-56Z",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, call := range copier.calls {
		if strings.HasSuffix(call.source, "/.env") {
			t.Fatalf("a file that does not exist was sent for copying: %+v", call)
		}
	}
	// compose, the bind, the volume, then the manifest
	if len(copier.calls) != 4 {
		t.Fatalf("want 4 copies, got %d: %+v", len(copier.calls), copier.calls)
	}
	if len(manifest.Skipped) != 0 {
		t.Fatalf("nothing was skipped either -- there was nothing to skip: %+v", manifest.Skipped)
	}
}

// The size of a backup is what its operations read on this host, a file's size
// and the files under a folder added up, whichever engine carried it.
func TestARunAddsUpTheBytesItRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"config/a.ini": 3, "config/sub/b.db": 5, "settings.env": 9} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	slash := filepath.ToSlash(dir)

	manifest := BackupManifest{Operations: []BackupOperation{
		{Source: slash + "/config", Destination: "config", Directory: true},
		{Source: slash + "/settings.env", Destination: "settings.env"},
		// a source that is gone reads as nothing; the copy would have said so
		{Source: slash + "/missing", Destination: "missing"},
	}}
	if err := copyPlan(context.Background(), &manifest, &fakeCopier{}, "offsite", "demo/2026-09-12T20-20-29Z"); err != nil {
		t.Fatal(err)
	}

	if manifest.Bytes != 17 {
		t.Fatalf("3 + 5 + 9 bytes, got %d", manifest.Bytes)
	}
}
