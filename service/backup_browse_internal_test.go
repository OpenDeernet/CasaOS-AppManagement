package service

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
)

// A destination that can be looked into and taken from, and remembers how.
type browsableRestorer struct {
	*fakeRestorer
	listed  []string
	taken   []restoredCall
	content map[string][]rclone.File
}

func (b *browsableRestorer) List(_ context.Context, _, remotePath, sub string) ([]rclone.File, error) {
	b.listed = append(b.listed, path.Join(remotePath, sub))

	return b.content[path.Join(remotePath, sub)], nil
}

func (b *browsableRestorer) CopyBack(_ context.Context, _, remotePath, sub, hostPath string) error {
	b.taken = append(b.taken, restoredCall{kind: sub, remote: remotePath, host: hostPath})

	return nil
}

func browsableBackup(t *testing.T) (*browsableRestorer, *ComposeApp, string, BackupManifest) {
	t.Helper()
	logInTempDir(t)

	app, dir := appWithOneBindAndOneVolume(t)
	restorer, manifest := backupOf(t, app)

	return &browsableRestorer{fakeRestorer: restorer, content: map[string][]rclone.File{}}, app, dir, manifest
}

func operationOf(t *testing.T, manifest BackupManifest, kind BackupKind) BackupOperation {
	t.Helper()

	for _, operation := range manifest.Operations {
		if operation.Kind == kind {
			return operation
		}
	}
	t.Fatalf("the backup holds no %s", kind)

	return BackupOperation{}
}

func names(files []rclone.File) []string {
	out := []string{}
	for _, file := range files {
		out = append(out, file.Name)
	}

	return out
}

func TestTheTopOfABackupLeadsToItsCopiesAndNotToTheDefinitionOfTheApp(t *testing.T) {
	browser, _, _, manifest := browsableBackup(t)
	volume := operationOf(t, manifest, BackupKindVolume)

	files, err := BrowseBackup(context.Background(), browser, "offsite", "demo", restoreStamp, "")
	if err != nil {
		t.Fatal(err)
	}

	// one step towards each copy that is data
	want := map[string]bool{}
	for _, kind := range []BackupKind{BackupKindVolume, BackupKindBind} {
		top, _, _ := strings.Cut(operationOf(t, manifest, kind).Destination, "/")
		want[top] = true
	}
	got := map[string]bool{}
	for _, name := range names(files) {
		got[name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (volume at %s)", names(files), want, volume.Destination)
	}
	for _, file := range files {
		if !file.Directory {
			t.Fatalf("%+v is a step towards a copy, not a file", file)
		}
		if file.Name == "docker-compose.yml" || file.Name == ".env" {
			t.Fatalf("the definition of the app is not data: %+v", file)
		}
	}
	if len(browser.listed) != 0 {
		t.Fatalf("the top of a backup is its manifest, not a listing: %v", browser.listed)
	}
}

func TestInsideACopyTheEngineListsItSortedFoldersFirst(t *testing.T) {
	browser, _, _, manifest := browsableBackup(t)
	volume := operationOf(t, manifest, BackupKindVolume)
	remote := path.Join(RootFor("demo", restoreStamp), volume.Destination)
	browser.content[path.Join(remote, "docs")] = []rclone.File{{Name: "b.txt"}, {Name: "sub", Directory: true}, {Name: "a.txt"}}

	files, err := BrowseBackup(context.Background(), browser, "offsite", "demo", restoreStamp, volume.Destination+"/docs")
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"sub", "a.txt", "b.txt"}; !reflect.DeepEqual(names(files), want) {
		t.Fatalf("got %v, want %v", names(files), want)
	}
	if want := []string{path.Join(remote, "docs")}; !reflect.DeepEqual(browser.listed, want) {
		t.Fatalf("asked %v, want %v", browser.listed, want)
	}
}

func TestAPathThatClimbsOutOfTheBackupIsRefused(t *testing.T) {
	browser, _, _, manifest := browsableBackup(t)
	volume := operationOf(t, manifest, BackupKindVolume)

	for _, rel := range []string{"../other", volume.Destination + "/../../x", `..\x`} {
		if _, err := BrowseBackup(context.Background(), browser, "offsite", "demo", restoreStamp, rel); !errors.Is(err, ErrBackupPathOutside) {
			t.Fatalf("%q: err %v", rel, err)
		}
	}
	if len(browser.listed) != 0 {
		t.Fatalf("something was listed: %v", browser.listed)
	}
}

func TestAPathTheBackupDoesNotHoldSaysSo(t *testing.T) {
	browser, _, _, _ := browsableBackup(t)

	if _, err := BrowseBackup(context.Background(), browser, "offsite", "demo", restoreStamp, "nowhere/at/all"); !errors.Is(err, ErrBackupPathNotHeld) {
		t.Fatalf("err %v", err)
	}
}

func TestSomePathsAreTakenBackToThisBoxsPlacesAndNothingElseIsTouched(t *testing.T) {
	browser, app, _, manifest := browsableBackup(t)
	volume := operationOf(t, manifest, BackupKindVolume)
	docker := &fakeBackupDocker{mountpoints: demoMountpoints()}

	report, err := RestoreBackup(context.Background(), app, docker, browser, noInstall, RestoreOptions{
		Destination: "offsite", App: "demo", Stamp: restoreStamp, Containers: running("c1"),
		Paths: []string{volume.Destination + "/docs/a.txt", volume.Destination + "/docs"},
	})
	if err != nil {
		t.Fatal(err)
	}

	remote := path.Join(RootFor("demo", restoreStamp), volume.Destination)
	want := []restoredCall{
		{kind: "docs/a.txt", remote: remote, host: filepath.Join(demoVolumePath, "docs", "a.txt")},
		{kind: "docs", remote: remote, host: filepath.Join(demoVolumePath, "docs")},
	}
	if !reflect.DeepEqual(browser.taken, want) {
		t.Fatalf("got %+v, want %+v", browser.taken, want)
	}
	// "as it was" is the whole-backup restore: it deletes what the backup does not hold
	if len(browser.restored) != 0 {
		t.Fatalf("something was restored as it was: %+v", browser.restored)
	}
	if len(report.Restored) != 2 || len(report.Missing) != 0 {
		t.Fatalf("%+v", report)
	}
}

func TestAPathNeverLeavesTheCopyItIsTakenInto(t *testing.T) {
	browser, app, _, manifest := browsableBackup(t)
	volume := operationOf(t, manifest, BackupKindVolume)

	_, err := RestoreBackup(context.Background(), app, &fakeBackupDocker{mountpoints: demoMountpoints()}, browser, noInstall, RestoreOptions{
		Destination: "offsite", App: "demo", Stamp: restoreStamp, Containers: running("c1"),
		Paths: []string{volume.Destination + "/../../../etc/passwd"},
	})
	if !errors.Is(err, ErrBackupPathOutside) {
		t.Fatalf("err %v", err)
	}
	if len(browser.taken) != 0 {
		t.Fatalf("something was taken: %+v", browser.taken)
	}
}

func TestSomeFilesOfAnAppThatIsNotInstalledAreNotInstalledFor(t *testing.T) {
	browser, _, _, manifest := browsableBackup(t)
	volume := operationOf(t, manifest, BackupKindVolume)

	_, err := RestoreBackup(context.Background(), nil, &fakeBackupDocker{mountpoints: demoMountpoints()}, browser, noInstall, RestoreOptions{
		Destination: "offsite", App: "demo", Stamp: restoreStamp, Paths: []string{volume.Destination + "/x"},
	})
	if !errors.Is(err, ErrRestorePathsNeedApp) {
		t.Fatalf("err %v", err)
	}
}

func TestACopyTheAppNoLongerDeclaresIsReportedNotWrittenSomewhereElse(t *testing.T) {
	browser, app, _, manifest := browsableBackup(t)
	volume := operationOf(t, manifest, BackupKindVolume)

	// the volume is not mounted any more: Docker has no mountpoint for it
	report, err := RestoreBackup(context.Background(), app, &fakeBackupDocker{}, browser, noInstall, RestoreOptions{
		Destination: "offsite", App: "demo", Stamp: restoreStamp, Containers: running("c1"),
		Paths: []string{volume.Destination + "/docs"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(browser.taken) != 0 || len(report.Missing) != 1 || report.Missing[0].Destination != volume.Destination {
		t.Fatalf("taken %+v, report %+v", browser.taken, report)
	}
}
