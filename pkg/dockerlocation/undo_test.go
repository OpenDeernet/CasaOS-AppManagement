package dockerlocation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// switchedToFolder is a host whose data was switched to /DATA/big, both
// areas, with Docker up again on it.
func switchedToFolder(t *testing.T) (*Mover, *fakeDocker) {
	t.Helper()
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	ctx := context.Background()
	for _, a := range areas {
		if err := m.copy(ctx, a.live, filepath.Join("/DATA/big", a.name)); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = d.run(ctx, "systemctl", "stop", "docker.service")
	if _, err := m.switchMounts(ctx, "", "/DATA/big"); err != nil {
		t.Fatal(err)
	}
	_, _ = d.run(ctx, "systemctl", "start", "docker.service")
	return m, d
}

func interruptedUndoing(t *testing.T, m *Mover, from, target string) {
	t.Helper()
	b, _ := json.Marshal(State{Phase: PhaseSwitching, From: from, Target: target, Running: []string{"c1", "c2"}, Undoing: true})
	os.WriteFile(m.StatePath, b, 0o644)
	m.load()
}

// A failed start cut before its undo started looks like a switch that ended:
// it is undone, never kept as a move that worked.
func TestAnUndoCutBeforeItStartedIsNeverKept(t *testing.T) {
	m, d := switchedToFolder(t)

	interruptedUndoing(t, m, "", "/DATA/big")
	m.Recover(context.Background())

	asBefore(t, m, d, "/DATA/big")
	if s := m.State(); s.Phase != PhaseFailed || s.Location != "" || len(s.OldCopy) != 0 {
		t.Errorf("%+v", s)
	}
}

// The undo of a failed start is recorded before Docker is even stopped.
func TestAFailedStartRecordsTheUndoFirst(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	d.refuse = func() bool { return m.location() != "" }
	var recorded *State
	stops := 0
	d.hook = func(call string) {
		if call != "systemctl stop docker.socket" {
			return
		}
		// the first stop is the move's own; the second follows the failed start
		if stops++; stops == 2 {
			var s State
			b, _ := os.ReadFile(m.StatePath)
			_ = json.Unmarshal(b, &s)
			recorded = &s
		}
	}

	if s := runMove(t, m, "/DATA/big"); s.Phase != PhaseFailed {
		t.Fatalf("%+v", s)
	}
	if recorded == nil || recorded.Phase != PhaseSwitching || !recorded.Undoing {
		t.Errorf("the state before the undo: %+v", recorded)
	}
	if b, _ := os.ReadFile(m.StatePath); strings.Contains(string(b), "undoing") {
		t.Errorf("left undoing once failed: %s", b)
	}
}

// Docker that will not stop after a failed start: nothing is undone under it,
// and the message says where the data is.
func TestAFailedStartIsNotUndoneUnderARunningDocker(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	d.refuse = func() bool {
		if m.location() == "" {
			return false
		}
		d.stuck = map[string]bool{"docker.service": true}
		return true
	}

	s := runMove(t, m, "/DATA/big")
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "nothing was undone") || s.Location != "/DATA/big" {
		t.Fatalf("%+v", s)
	}
	for _, c := range d.calls {
		if c == "systemctl stop var-lib-docker.mount" {
			t.Errorf("undone anyway: %v", d.calls)
		}
	}
}

// A disk missing at boot shows no copy: the switch is not taken as ended,
// and the data comes back from where it was set aside.
func TestASwitchToADiskMissingAtBootIsNeverKept(t *testing.T) {
	m, d := switchedToFolder(t)
	for _, a := range areas {
		os.RemoveAll(m.path(filepath.Join("/DATA/big", a.name)))
		d.mounted[a.unit] = false
	}
	if m.switchDone("", "/DATA/big") {
		t.Fatal("switchDone without the copies")
	}

	interruptedAt(t, m, PhaseSwitching, "", "/DATA/big")
	m.Recover(context.Background())

	asBefore(t, m, d, "/DATA/big")
	if s := m.State(); s.Phase != PhaseFailed || s.Location != "" {
		t.Errorf("%+v", s)
	}
}

// Cut mid-switch from the default place: containerd switched, docker set
// aside and its unit not yet written. Docker started at boot on the empty
// folder and wrote there. Docker running, stopped first; the folder it wrote
// is kept aside and the data comes back.
func TestRecoverSetsAsideAFolderDockerStartedOnAtBoot(t *testing.T) {
	m, d := switchedToFolder(t)
	ctx := context.Background()
	// docker not switched yet: its unit gone, its folder an empty one
	os.Remove(m.path(unitPath("var-lib-docker.mount")))
	os.Remove(m.path(dropInPath("docker.service")))
	d.mounted["var-lib-docker.mount"] = false
	os.RemoveAll(m.path("/DATA/big/docker"))
	os.WriteFile(m.path("/var/lib/docker/network.db"), []byte("written at boot"), 0o644)
	if up, _ := d.state(); !up {
		t.Fatal("Docker should run")
	}

	interruptedAt(t, m, PhaseSwitching, "", "/DATA/big")
	before := len(d.calls)
	m.Recover(ctx)
	calls := d.calls[before:]

	socket := firstCall(calls, func(c string) bool { return c == "systemctl stop docker.socket" })
	units := firstCall(calls, func(c string) bool { return strings.Contains(c, ".mount") || strings.Contains(c, "daemon-reload") })
	if socket < 0 || units < socket {
		t.Errorf("socket %d, first unit change %d: %v", socket, units, calls)
	}

	asBefore(t, m, d, "/DATA/big")
	if b, _ := os.ReadFile(m.path("/var/lib/docker" + bootSuffix + "/network.db")); string(b) != "written at boot" {
		t.Errorf("what Docker wrote at boot: %q", b)
	}
	if s := m.State(); !strings.Contains(s.Message, "/var/lib/docker"+bootSuffix) {
		t.Errorf("%+v", s)
	}
}

// The way back cut while undoing, with docker's live folder empty and
// written at boot: set aside, and the data bound again from the folder.
func TestRecoverSetsAsideABootFolderOnTheWayBack(t *testing.T) {
	m, d := moved(t)
	for _, a := range areas {
		_ = os.RemoveAll(m.path(a.live))
		_ = os.Mkdir(m.path(a.live), 0o711)
		os.Remove(m.path(unitPath(a.unit)))
		os.Remove(m.path(dropInPath(a.service)))
		d.mounted[a.unit] = false
		os.MkdirAll(m.path(a.live+newSuffix), 0o711)
	}
	os.WriteFile(m.path("/var/lib/docker/network.db"), []byte("written at boot"), 0o644)

	interruptedUndoing(t, m, "/DATA/big", Default)
	m.Recover(context.Background())

	s := m.State()
	if s.Location != "/DATA/big" || m.location() != "/DATA/big" {
		t.Fatalf("%+v", s)
	}
	if !there(m, "/var/lib/docker"+bootSuffix+"/network.db") || !strings.Contains(s.Message, bootSuffix) {
		t.Errorf("not set aside: %+v", s)
	}
	for _, a := range areas {
		if there(m, a.live+newSuffix) {
			t.Errorf("%s is left", a.live+newSuffix)
		}
	}
	if b, _ := os.ReadFile(m.path("/DATA/big/docker/volumes/v1/data")); string(b) != "live /var/lib/docker/volumes/v1/data" {
		t.Errorf("the data in /DATA/big changed: %q", b)
	}
}

// The way out with Docker running at boot, in the real order: containerd
// switched to the folder, docker not yet.
func TestRecoverBackToDefaultWithDockerRunning(t *testing.T) {
	m, d := switchedToFolder(t)
	ctx := context.Background()
	os.Remove(m.path(unitPath("var-lib-docker.mount")))
	os.Remove(m.path(dropInPath("docker.service")))
	d.mounted["var-lib-docker.mount"] = false
	os.RemoveAll(m.path("/var/lib/docker"))
	os.Rename(m.path("/var/lib/docker"+oldSuffix), m.path("/var/lib/docker"))

	interruptedAt(t, m, PhaseSwitching, "", "/DATA/big")
	before := len(d.calls)
	m.Recover(ctx)
	calls := d.calls[before:]

	socket := firstCall(calls, func(c string) bool { return c == "systemctl stop docker.socket" })
	units := firstCall(calls, func(c string) bool { return strings.Contains(c, ".mount") || strings.Contains(c, "daemon-reload") })
	if socket < 0 || units < socket {
		t.Errorf("socket %d, first unit change %d: %v", socket, units, calls)
	}
	asBefore(t, m, d, "/DATA/big")
	if there(m, "/var/lib/docker"+bootSuffix) || there(m, "/var/lib/containerd"+bootSuffix) {
		t.Error("a live folder with the data was set aside")
	}
}

// A socket still active after the stop is an error: it would wake Docker.
func TestASocketThatStaysActiveStopsTheMove(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	d.isActive = "active\ninactive\ninactive\n"

	s := runMove(t, m, "/DATA/big")
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "docker.socket is still active") {
		t.Fatalf("%+v", s)
	}
	if there(m, "/DATA/big/docker") || m.location() != "" {
		t.Error("the move went on")
	}
}

// btrfs layers are subvolumes the copy does not enter: refused.
func TestTheBtrfsDriverIsRefused(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	d.driver = "btrfs"

	if c := m.CheckTarget(context.Background(), "/DATA/big"); c.OK || !strings.Contains(c.Reason, "btrfs") {
		t.Fatalf("%+v", c)
	}
}

// The unit files and folders are written through to the disk: a written unit
// is whole when read back.
func TestUnitsAreWrittenWhole(t *testing.T) {
	m, _ := switchedToFolder(t)
	for _, a := range areas {
		b, err := os.ReadFile(m.path(unitPath(a.unit)))
		if err != nil || unitValue(string(b), "What") != filepath.Join("/DATA/big", a.name) {
			t.Errorf("%s: %q %v", a.unit, b, err)
		}
	}
	dir := t.TempDir()
	if err := writeSynced(filepath.Join(dir, "u"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renameSynced(filepath.Join(dir, "u"), filepath.Join(dir, "v")); err != nil {
		t.Fatal(err)
	}
	if err := mkdirSynced(filepath.Join(dir, "w"), 0o711); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"v", "w"}) {
		t.Errorf("%v", names)
	}
}
