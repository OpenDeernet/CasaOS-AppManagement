package dockerlocation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// wayBackCut is a way back cut short by a power cut after docker was
// switched (its unit gone, the fresh copy in place) and before containerd
// was. At boot Docker started from the fresh copy and runs.
func wayBackCut(t *testing.T) (*Mover, *fakeDocker) {
	t.Helper()
	m, d := moved(t)
	for _, a := range areas {
		_ = os.RemoveAll(m.path(a.live))
		_ = os.Mkdir(m.path(a.live), 0o711)
	}
	os.Remove(m.path(unitPath("var-lib-docker.mount")))
	os.Remove(m.path(dropInPath("docker.service")))
	d.mounted["var-lib-docker.mount"] = false
	os.WriteFile(m.path("/var/lib/docker/fresh"), []byte("written since boot"), 0o644)
	os.MkdirAll(m.path("/var/lib/containerd"+newSuffix), 0o711)
	if up, _ := d.state(); !up {
		t.Fatal("Docker should run, as at boot")
	}
	return m, d
}

func firstCall(calls []string, match func(string) bool) int {
	return slices.IndexFunc(calls, match)
}

// At boot Docker is up before Recover: it is stopped, socket first, before
// any folder under it changes.
func TestRecoverStopsARunningDockerBeforeTouchingFolders(t *testing.T) {
	m, d := wayBackCut(t)
	ctx := context.Background()
	var atStop []string
	d.hook = func(call string) {
		if call == "systemctl stop docker.socket" {
			if !there(m, "/var/lib/docker/fresh") {
				atStop = append(atStop, "the live folder was moved before Docker stopped")
			}
			if there(m, unitPath("var-lib-docker.mount")) {
				atStop = append(atStop, "the unit was written before Docker stopped")
			}
		}
	}

	interruptedAt(t, m, PhaseSwitching, "/DATA/big", Default)
	before := len(d.calls)
	m.Recover(ctx)
	calls := d.calls[before:]

	for _, e := range atStop {
		t.Error(e)
	}
	socket := firstCall(calls, func(c string) bool { return c == "systemctl stop docker.socket" })
	service := firstCall(calls, func(c string) bool { return c == "systemctl stop docker.service" })
	folders := firstCall(calls, func(c string) bool {
		return strings.Contains(c, ".mount") || strings.Contains(c, "daemon-reload")
	})
	if socket < 0 || service < socket || folders < service {
		t.Errorf("socket %d, service %d, first unit change %d: %v", socket, service, folders, calls)
	}

	if s := m.State(); s.Location != "/DATA/big" || !strings.Contains(s.Message, "put back") {
		t.Fatalf("%+v", s)
	}
	if up, running := d.state(); !up || strings.Join(running, ",") != "c1,c2" {
		t.Errorf("Docker up %v, running %v", up, running)
	}
}

// Docker that will not stop: nothing changes under it, and it is started again.
func TestRecoverChangesNothingWhenDockerWillNotStop(t *testing.T) {
	m, d := wayBackCut(t)
	d.stuck = map[string]bool{"docker.service": true}

	interruptedAt(t, m, PhaseSwitching, "/DATA/big", Default)
	before := len(d.calls)
	m.Recover(context.Background())

	s := m.State()
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "nothing was changed") {
		t.Fatalf("%+v", s)
	}
	if b, _ := os.ReadFile(m.path("/var/lib/docker/fresh")); string(b) != "written since boot" {
		t.Errorf("the live folder changed: %q", b)
	}
	if !there(m, "/var/lib/containerd"+newSuffix) || there(m, unitPath("var-lib-docker.mount")) {
		t.Error("the folders or units changed")
	}
	for _, c := range d.calls[before:] {
		if strings.Contains(c, ".mount") || strings.Contains(c, "daemon-reload") || strings.HasPrefix(c, "umount") {
			t.Errorf("changed: %s", c)
		}
	}
	if up, _ := d.state(); !up {
		t.Error("Docker was left stopped")
	}
}

// A way back cut short once both areas were switched: what the apps wrote
// since boot is kept, and the move ends.
func TestRecoverKeepsAWayBackThatHadEnded(t *testing.T) {
	m, d := moved(t)
	for _, a := range areas {
		_ = os.RemoveAll(m.path(a.live))
		_ = os.Mkdir(m.path(a.live), 0o711)
		os.Remove(m.path(unitPath(a.unit)))
		os.Remove(m.path(dropInPath(a.service)))
		d.mounted[a.unit] = false
	}
	os.WriteFile(m.path("/var/lib/docker/fresh"), []byte("written since boot"), 0o644)

	interruptedAt(t, m, PhaseSwitching, "/DATA/big", Default)
	before := len(d.calls)
	m.Recover(context.Background())

	s := m.State()
	if s.Phase != PhaseDone || s.Location != "" || !strings.Contains(s.Message, "kept") {
		t.Fatalf("%+v", s)
	}
	if b, _ := os.ReadFile(m.path("/var/lib/docker/fresh")); string(b) != "written since boot" {
		t.Errorf("what the apps wrote since boot: %q", b)
	}
	if !slices.Contains(s.OldCopy, "/DATA/big/docker") || !there(m, "/DATA/big/docker/volumes/v1/data") {
		t.Errorf("the old copy: %v", s.OldCopy)
	}
	if slices.Contains(d.calls[before:], "systemctl stop docker.service") {
		t.Error("a Docker running where it should was stopped")
	}
}

// A switch to a folder cut short once it was done: kept, and its units enabled.
func TestRecoverKeepsASwitchToAFolderThatHadEnded(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	ctx := context.Background()
	for _, a := range areas {
		_ = m.copy(ctx, a.live, filepath.Join("/DATA/big", a.name))
	}
	_, _ = d.run(ctx, "systemctl", "stop", "docker.service")
	if _, err := m.switchMounts(ctx, "", "/DATA/big"); err != nil {
		t.Fatal(err)
	}
	_, _ = d.run(ctx, "systemctl", "start", "docker.service")

	interruptedAt(t, m, PhaseSwitching, "", "/DATA/big")
	before := len(d.calls)
	m.Recover(ctx)

	s := m.State()
	if s.Phase != PhaseDone || s.Location != "/DATA/big" {
		t.Fatalf("%+v", s)
	}
	for _, a := range areas {
		if !slices.Contains(d.calls[before:], "systemctl enable --now "+a.unit) {
			t.Errorf("%s not enabled: %v", a.unit, d.calls[before:])
		}
		if !slices.Contains(s.OldCopy, a.live+oldSuffix) {
			t.Errorf("old copy %v", s.OldCopy)
		}
	}
	if slices.Contains(d.calls[before:], "systemctl stop docker.service") {
		t.Error("a Docker running where it should was stopped")
	}
}

// A start that fails is undone as a switch: a stop during the undo is put
// back, never taken for a move that ended.
func TestAFailedStartIsUndoneWhileSwitching(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	d.refuse = func() bool { return m.location() != "" }
	var phase string
	d.hook = func(call string) {
		if call == "systemctl stop var-lib-docker.mount" && phase == "" {
			var s State
			b, _ := os.ReadFile(m.StatePath)
			_ = json.Unmarshal(b, &s)
			phase = s.Phase
		}
	}

	s := runMove(t, m, "/DATA/big")
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "put back as it was") {
		t.Fatalf("%+v", s)
	}
	if phase != PhaseSwitching {
		t.Errorf("the state said %q during the undo", phase)
	}
	if strings.Contains(s.Message, "as it was:") {
		t.Errorf("says as it was before the undo: %s", s.Message)
	}
}

// The state is replaced whole, and how far a copy is never written.
func TestTheStateFileIsReplacedWhole(t *testing.T) {
	m := host(t)
	m.set(func(s *State) { s.Phase = PhaseCopying; s.Target = "/DATA/big" })
	m.show(func(s *State) { s.CopiedBytes = 12345 })

	entries, _ := os.ReadDir(filepath.Dir(m.StatePath))
	for _, e := range entries {
		if e.Name() != filepath.Base(m.StatePath) {
			t.Errorf("left next to the state: %s", e.Name())
		}
	}
	var s State
	b, _ := os.ReadFile(m.StatePath)
	if err := json.Unmarshal(b, &s); err != nil || s.Phase != PhaseCopying || s.CopiedBytes != 0 {
		t.Errorf("%+v %v", s, err)
	}
	if m.State().CopiedBytes != 12345 {
		t.Error("the dashboard does not see the progress")
	}
}

// A move cut short is Recover's: nothing else starts before it ran.
func TestNothingStartsBeforeRecover(t *testing.T) {
	m := liveHost(t)
	newFakeDocker(m)
	interruptedAt(t, m, PhaseCopying, "", "/DATA/big")

	if _, err := m.Start("/DATA/big"); !errors.Is(err, errBusy) {
		t.Errorf("start: %v", err)
	}
	m.state.OldCopy = []string{"/var/lib/docker" + oldSuffix}
	if _, err := m.DeleteOldCopy(); !errors.Is(err, errBusy) {
		t.Errorf("delete: %v", err)
	}

	m.Recover(context.Background())
	if m.recovering() {
		t.Error("still recovering")
	}
}

// The way back refuses a drive mounted under Docker's folder: rsync -x would
// leave it out, and without -x it would fill the system disk.
func TestTheWayBackRefusesADriveInside(t *testing.T) {
	m, _ := moved(t)
	writeMountinfo(t, m,
		"36 35 98:0 / /var/lib/docker rw,noatime - ext4 /dev/sda1 rw",
		"37 36 98:16 / /var/lib/docker/volumes/big rw,noatime - ext4 /dev/sdb1 rw")

	c := m.CheckTarget(context.Background(), Default)
	if c.OK || !strings.Contains(c.Reason, "a drive is mounted in /var/lib/docker") {
		t.Fatalf("%+v", c)
	}
}

// A /var/lib/containerd made after the move, on the system disk, has no
// mount to take away: the way back leaves it and moves docker alone.
func TestTheWayBackLeavesAContainerdMadeSince(t *testing.T) {
	m, d := moved(t)
	os.Remove(m.path(unitPath("var-lib-containerd.mount")))
	os.Remove(m.path(dropInPath("containerd.service")))
	d.mounted["var-lib-containerd.mount"] = false
	os.MkdirAll(m.path("/var/lib/containerd/io"), 0o711)
	os.WriteFile(m.path("/var/lib/containerd/io/new"), []byte("made since"), 0o644)

	var mu sync.Mutex
	var copied []string
	cp := copyAll(m, 0, nil)
	m.Copy = func(ctx context.Context, src, dst string) error {
		mu.Lock()
		copied = append(copied, src)
		mu.Unlock()
		return cp(ctx, src, dst)
	}

	s := runMove(t, m, Default)
	if s.Phase != PhaseDone || s.Location != "" {
		t.Fatalf("%+v", s)
	}
	if strings.Join(copied, ",") != "/var/lib/docker" {
		t.Errorf("copied %v", copied)
	}
	if b, _ := os.ReadFile(m.path("/var/lib/containerd/io/new")); string(b) != "made since" {
		t.Errorf("containerd's folder changed: %q", b)
	}
	if there(m, "/var/lib/containerd"+newSuffix) {
		t.Error("a copy of containerd was left")
	}
}

// docker.service allows 3 starts a minute: every start clears that count first.
func TestDockerIsStartedWithItsStartLimitCleared(t *testing.T) {
	m, d := moved(t)
	before := len(d.calls)
	if s := runMove(t, m, Default); s.Phase != PhaseDone {
		t.Fatalf("%+v", s)
	}
	calls := d.calls[before:]
	reset := firstCall(calls, func(c string) bool {
		return strings.HasPrefix(c, "systemctl reset-failed") && strings.Contains(c, "docker.service")
	})
	start := firstCall(calls, func(c string) bool { return c == "systemctl start docker.service" })
	if reset < 0 || start < reset {
		t.Errorf("reset-failed %d, start %d: %v", reset, start, calls)
	}
}
