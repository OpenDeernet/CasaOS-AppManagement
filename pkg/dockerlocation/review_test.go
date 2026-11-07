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
	"time"
)

// moved is a host with its data moved to /DATA/big.
func moved(t *testing.T) (*Mover, *fakeDocker) {
	t.Helper()
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	if s := runMove(t, m, "/DATA/big"); s.Phase != PhaseDone {
		t.Fatalf("there: %+v", s)
	}
	return m, d
}

func writeMountinfo(t *testing.T, m *Mover, lines ...string) {
	t.Helper()
	os.MkdirAll(m.path("/proc/self"), 0o755)
	if err := os.WriteFile(m.path("/proc/self/mountinfo"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// From a folder, only back: a move to another folder goes through the default place.
func TestFolderToFolderIsRefused(t *testing.T) {
	m, d := moved(t)
	os.MkdirAll(m.path("/DATA/other"), 0o755)
	before := len(d.calls)

	c := m.CheckTarget(context.Background(), "/DATA/other")
	if c.OK || !strings.Contains(c.Reason, "move it back to the default place first") {
		t.Fatalf("%+v", c)
	}
	if _, err := m.Start("/DATA/other"); err == nil {
		t.Fatal("started")
	}
	for _, call := range d.calls[before:] {
		if strings.HasPrefix(call, "systemctl") {
			t.Errorf("touched systemd: %s", call)
		}
	}
	if _, err := m.switchMounts(context.Background(), "/DATA/big", "/DATA/other"); err == nil {
		t.Error("switchMounts went from a folder")
	}
}

// A slow check (a big tree) and a second click: one move, not two into the same folder.
func TestTwoStartsAtOnceMakeOneMove(t *testing.T) {
	m := liveHost(t)
	newFakeDocker(m)
	copies := 0
	var mu sync.Mutex
	m.Copy = func(ctx context.Context, src, dst string) error {
		mu.Lock()
		copies++
		mu.Unlock()
		return copyAll(m, 0, nil)(ctx, src, dst)
	}
	m.StatFS = func(string) (string, int64, error) {
		time.Sleep(50 * time.Millisecond)
		return "ext4", 100 << 30, nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Start("/DATA/big")
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, errBusy) {
			t.Errorf("the second start: %v, want %v", err, errBusy)
		}
	}
	if ok != 1 {
		t.Fatalf("%d moves started: %v", ok, errs)
	}

	deadline := time.Now().Add(10 * time.Second)
	for m.State().busy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s := m.State(); s.Phase != PhaseDone {
		t.Fatalf("%+v", s)
	}
	if copies != len(areas) {
		t.Errorf("%d copies, want %d", copies, len(areas))
	}
	// the move is over: the lock is free for the next
	if _, err := m.DeleteOldCopy(); err != nil {
		t.Errorf("delete after the move: %v", err)
	}
}

// A mount that does not stop on the way back: Docker stays on the folder, and
// the folder it uses is never listed as an old copy to delete.
func TestABusyMountStopsTheWayBack(t *testing.T) {
	m, d := moved(t)
	d.busy["var-lib-docker.mount"] = true

	s := runMove(t, m, Default)
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "busy") || s.Location != "/DATA/big" {
		t.Fatalf("%+v", s)
	}
	for _, p := range s.OldCopy {
		if strings.HasPrefix(p, "/DATA/big") {
			t.Errorf("the live folder is listed as old: %v", s.OldCopy)
		}
	}
	if b, _ := os.ReadFile(m.path("/DATA/big/docker/volumes/v1/data")); string(b) != "live /var/lib/docker/volumes/v1/data" {
		t.Errorf("the live data changed: %q", b)
	}
	if up, _ := d.state(); !up {
		t.Error("Docker was left stopped")
	}
}

// The mount does not take (enable --now did nothing): Docker would come back on
// the old folder. The switch is refused and undone.
func TestASwitchThatDidNotMountIsUndone(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	m.SameDir = func(a, b string) bool { return false }

	s := runMove(t, m, "/DATA/big")
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "is not /DATA/big/") {
		t.Fatalf("%+v", s)
	}
	asBefore(t, m, d, "/DATA/big")
}

// An undo that cannot finish keeps the copy and says where the data is.
func TestAnUndoThatFailsKeepsTheCopyAndSaysWhere(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	d.refuse = func() bool { return m.location() != "" }
	d.busy["var-lib-docker.mount"] = true

	s := runMove(t, m, "/DATA/big")
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "putting it back failed too") || !strings.Contains(s.Message, "nothing was deleted") {
		t.Fatalf("%+v", s)
	}
	// docker's mount stays: its data stays aside, as the message says; containerd's came back
	if !strings.Contains(s.Message, "/var/lib/docker"+oldSuffix) || !strings.Contains(s.Message, "/DATA/big/docker") {
		t.Errorf("the message does not say where the data is: %s", s.Message)
	}
	if !there(m, "/DATA/big/docker/volumes/v1/data") || !there(m, "/var/lib/docker"+oldSuffix+"/volumes/v1/data") {
		t.Error("a copy was deleted although the undo failed")
	}
	if !there(m, unitPath("var-lib-docker.mount")) {
		t.Error("the unit of the mount that stays went")
	}
	if up, _ := d.state(); !up {
		t.Error("Docker was left stopped")
	}
}

// After a move, an entry of the list that is the new place or under it goes:
// default → X → default → X would otherwise list X/docker, now live.
func TestAMoveDropsTheOldCopiesItLivesIn(t *testing.T) {
	m := host(t)
	m.SameDir = func(a, b string) bool { return false }
	m.state.OldCopy = []string{"/DATA/big/docker", "/DATA/big/containerd", "/media/old/docker"}
	m.finish("", "/DATA/big")
	if got := strings.Join(m.State().OldCopy, ","); got != "/media/old/docker" {
		t.Errorf("old copy %s", got)
	}
}

func TestDeleteOldCopyRefusesWhatDockerUses(t *testing.T) {
	m := host(t)
	os.WriteFile(m.path(unitPath("var-lib-docker.mount")), []byte(MountUnit("/DATA/big/docker", "/var/lib/docker", "/DATA/big", "docker.service")), 0o644)
	m.load()
	os.MkdirAll(m.path("/DATA/big/docker/x"), 0o755)
	os.MkdirAll(m.path("/media/bound/x"), 0o755)

	for _, p := range []string{"/DATA/big/docker", "/DATA/big", "/DATA", "/var/lib", "/media/bound"} {
		m.state.OldCopy = []string{p}
		// /media/bound is what /var/lib/containerd is bound to, whatever the list says
		m.SameDir = func(a, b string) bool { return a == "/var/lib/containerd" && b == "/media/bound" }
		if _, err := m.DeleteOldCopy(); err == nil || !strings.Contains(err.Error(), "in use") {
			t.Errorf("%s: %v", p, err)
		}
	}
	if !there(m, "/DATA/big/docker/x") || !there(m, "/media/bound/x") {
		t.Error("live data was deleted")
	}
}

// Docker keeps its data elsewhere (data-root), or does not answer: nothing is moved.
func TestTheDockerRootIsChecked(t *testing.T) {
	for answer, want := range map[string]string{
		"/mnt/docker-root\n": "data-root",
		"":                   "does not answer",
	} {
		m := liveHost(t)
		m.Run = func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "docker" && args[0] == "info" {
				if answer == "" {
					return "", errors.New("Cannot connect")
				}
				return answer, nil
			}
			return "", nil
		}
		if c := m.CheckTarget(context.Background(), "/DATA/big"); c.OK || !strings.Contains(c.Reason, want) {
			t.Errorf("%q: %+v", answer, c)
		}
	}
}

func TestCheckTargetRefusesWhatWouldStopTheSwitch(t *testing.T) {
	ctx := context.Background()

	m := host(t)
	os.MkdirAll(m.path("/var/lib/docker"+oldSuffix), 0o755)
	if c := m.CheckTarget(ctx, "/DATA/big"); c.OK || !strings.Contains(c.Reason, "earlier move") {
		t.Errorf("a leftover old copy: %+v", c)
	}

	m = host(t)
	writeMountinfo(t, m,
		"33 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw",
		"40 33 8:1 / /var/lib/docker/volumes rw - ext4 /dev/sda1 rw",
		"41 33 0:69 / /var/lib/docker/rootfs/overlayfs/abc rw - overlay overlay rw")
	if c := m.CheckTarget(ctx, "/DATA/big"); c.OK || !strings.Contains(c.Reason, "/var/lib/docker/volumes") || strings.Contains(c.Reason, "overlayfs") {
		t.Errorf("a disk mounted on volumes: %+v", c)
	}

	m = host(t)
	writeMountinfo(t, m,
		"33 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw",
		"40 33 8:1 / /var/lib/docker rw - ext4 /dev/sda1 rw")
	if c := m.CheckTarget(ctx, "/DATA/big"); c.OK || !strings.Contains(c.Reason, "mount point of its own") {
		t.Errorf("/var/lib/docker on a disk of its own: %+v", c)
	}
}

// A link is checked where it points.
func TestCheckTargetFollowsLinks(t *testing.T) {
	m := host(t)
	os.MkdirAll(m.path("/root/secret"), 0o700)
	os.Symlink(m.path("/root/secret"), m.path("/DATA/link"))
	if c := m.CheckTarget(context.Background(), "/DATA/link"); c.OK || !strings.Contains(c.Reason, "under") {
		t.Errorf("a link out of /DATA: %+v", c)
	}

	os.Symlink(m.path("/DATA/big"), m.path("/DATA/tobig"))
	if c := m.CheckTarget(context.Background(), "/DATA/tobig"); !c.OK || c.Path != "/DATA/big" {
		t.Errorf("a link within /DATA: %+v", c)
	}
}

// The mount point comes from the mount table: a btrfs subvolume, a device of
// its own, is no mount and needs no line in fstab.
func TestMountPointFromTheMountTable(t *testing.T) {
	m := host(t)
	writeMountinfo(t, m,
		"33 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw",
		"50 33 8:17 / /media/usb rw - btrfs /dev/sdb1 rw",
		"51 33 8:33 / /media/usb2 rw - ext4 /dev/sdc1 rw")
	for path, want := range map[string]string{
		"/media/usb/sub/vol": "/media/usb",
		"/media/usb2":        "/media/usb2",
		"/media/usb22/x":     "/",
		"/DATA/big":          "/",
	} {
		if got := m.mountPoint(path); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}

// Only what Docker mounted is unmounted before a copy.
func TestUnmountUnderLeavesOtherDisks(t *testing.T) {
	m := host(t)
	writeMountinfo(t, m,
		"33 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw",
		"40 33 8:1 / /var/lib/docker/volumes/big rw - ext4 /dev/sda1 rw",
		"41 33 0:69 / /var/lib/docker/rootfs/overlayfs/abc rw - overlay overlay rw",
		"42 33 0:70 / /var/lib/docker/containers/x/mounts/shm rw - tmpfs shm rw")
	var unmounted []string
	m.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "umount" {
			unmounted = append(unmounted, args[0])
		}
		return "", nil
	}
	_ = m.unmountUnder(context.Background(), "/var/lib/docker", false)
	if slices.Contains(unmounted, "/var/lib/docker/volumes/big") || len(unmounted) != 2 {
		t.Errorf("unmounted %v", unmounted)
	}
}

// The total the dashboard divides by is the size rsync counts, and a check
// uses a measure a few minutes old rather than walk the tree each time.
func TestTheMeasureServesChecksAndCountsLikeRsync(t *testing.T) {
	m := liveHost(t)
	u := m.measure()
	if u.apparent == 0 || u.blocks < u.apparent {
		t.Fatalf("%+v", u)
	}
	os.WriteFile(m.path("/var/lib/docker/big"), make([]byte, 1<<20), 0o644)
	if got := m.estimate(); got != u {
		t.Errorf("a fresh measure was taken again: %+v", got)
	}
	if got := m.measure(); got.apparent != u.apparent+1<<20 {
		t.Errorf("measure %+v", got)
	}
}

// --- an interrupted move, at the next start ---

func interruptedAt(t *testing.T, m *Mover, phase, from, target string) {
	t.Helper()
	b, _ := json.Marshal(State{Phase: phase, From: from, Target: target, Running: []string{"c1", "c2"}})
	os.WriteFile(m.StatePath, b, 0o644)
	m.load()
	if s := m.State(); s.Phase != PhaseFailed || !strings.Contains(s.Message, "interrupted") {
		t.Fatalf("before Recover: %+v", s)
	}
}

func TestRecoverACopyCutShort(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	_, _ = d.run(context.Background(), "systemctl", "stop", "docker.service")
	os.MkdirAll(m.path("/DATA/big/docker/volumes"), 0o711)
	os.WriteFile(m.path("/DATA/big/docker/volumes/half"), []byte("half"), 0o644)

	interruptedAt(t, m, PhaseCopying, "", "/DATA/big")
	m.Recover(context.Background())

	asBefore(t, m, d, "/DATA/big")
	if s := m.State(); s.Phase != PhaseFailed || !strings.Contains(s.Message, "partial copy was deleted") {
		t.Errorf("%+v", s)
	}
}

func TestRecoverASwitchFromTheDefaultPlaceCutShort(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	ctx := context.Background()
	for _, a := range areas {
		_ = m.copy(ctx, a.live, filepath.Join("/DATA/big", a.name))
	}
	_, _ = d.run(ctx, "systemctl", "stop", "docker.service")
	// cut short after the first area was switched
	if _, err := m.switchMounts(ctx, "", "/DATA/big"); err != nil {
		t.Fatal(err)
	}
	os.Rename(m.path("/var/lib/containerd"+oldSuffix), m.path("/var/lib/containerd.tmp"))
	os.RemoveAll(m.path("/var/lib/containerd"))
	os.Rename(m.path("/var/lib/containerd.tmp"), m.path("/var/lib/containerd"))
	os.Remove(m.path(unitPath("var-lib-containerd.mount")))
	d.mounted = map[string]bool{"var-lib-docker.mount": true}

	interruptedAt(t, m, PhaseSwitching, "", "/DATA/big")
	m.Recover(ctx)

	asBefore(t, m, d, "/DATA/big")
	if s := m.State(); s.Location != "" || !strings.Contains(s.Message, "put back") {
		t.Errorf("%+v", s)
	}
}

func TestRecoverAWayBackCutShort(t *testing.T) {
	m, d := moved(t)
	ctx := context.Background()
	for _, a := range areas {
		_ = os.RemoveAll(m.path(a.live))
		_ = os.Mkdir(m.path(a.live), 0o711)
	}
	_, _ = d.run(ctx, "systemctl", "stop", "docker.service")
	// docker was switched back, containerd not yet
	os.Remove(m.path(unitPath("var-lib-docker.mount")))
	os.Remove(m.path(dropInPath("docker.service")))
	d.mounted["var-lib-docker.mount"] = false
	os.WriteFile(m.path("/var/lib/docker/fresh"), []byte("new copy"), 0o644)
	os.MkdirAll(m.path("/var/lib/containerd"+newSuffix), 0o711)

	interruptedAt(t, m, PhaseSwitching, "/DATA/big", Default)
	m.Recover(ctx)

	s := m.State()
	if s.Location != "/DATA/big" || m.location() != "/DATA/big" || !strings.Contains(s.Message, "put back") {
		t.Fatalf("%+v", s)
	}
	for _, a := range areas {
		if !there(m, unitPath(a.unit)) {
			t.Errorf("%s is not there", a.unit)
		}
		if there(m, a.live+newSuffix) {
			t.Errorf("%s is left", a.live+newSuffix)
		}
	}
	if b, _ := os.ReadFile(m.path("/DATA/big/docker/volumes/v1/data")); string(b) != "live /var/lib/docker/volumes/v1/data" {
		t.Errorf("the data in /DATA/big changed: %q", b)
	}
	if up, running := d.state(); !up || strings.Join(running, ",") != "c1,c2" {
		t.Errorf("Docker up %v, running %v", up, running)
	}
}

func TestRecoverAStartCutShort(t *testing.T) {
	m, d := moved(t)
	_, _ = d.run(context.Background(), "systemctl", "stop", "docker.service")
	m.state.OldCopy = nil
	m.save()

	interruptedAt(t, m, PhaseStarting, "", "/DATA/big")
	m.Recover(context.Background())

	s := m.State()
	if s.Phase != PhaseDone || s.Location != "/DATA/big" || len(s.OldCopy) != 2 {
		t.Fatalf("%+v", s)
	}
	if up, running := d.state(); !up || strings.Join(running, ",") != "c1,c2" {
		t.Errorf("Docker up %v, running %v", up, running)
	}
}

// Nothing to recover: nothing is started.
func TestRecoverWithNothingCutShort(t *testing.T) {
	m := host(t)
	d := newFakeDocker(m)
	m.Recover(context.Background())
	if len(d.calls) != 0 {
		t.Errorf("%v", d.calls)
	}
}

// A move says it ended a moment before it lets go of the lock: the click that
// follows waits for it, while a lock held for anything else is refused.
func TestTheLockIsWaitedForRightAfterAMove(t *testing.T) {
	m := liveHost(t)
	newFakeDocker(m)

	m.moving.Lock()
	m.set(func(s *State) { s.Phase = PhaseDone })
	go func() {
		time.Sleep(50 * time.Millisecond)
		m.moving.Unlock()
	}()
	if _, err := m.DeleteOldCopy(); errors.Is(err, errBusy) {
		t.Errorf("refused right after a move: %v", err)
	}

	m.moving.Lock()
	defer m.moving.Unlock()
	m.set(func(s *State) { s.Phase = PhaseIdle })
	start := time.Now()
	if _, err := m.DeleteOldCopy(); !errors.Is(err, errBusy) {
		t.Errorf("a held lock with no move ended: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("waited for a lock held for something else")
	}
}
