package dockerlocation

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker stands in for systemd and Docker during a whole move: Docker
// answers while docker.service is started, stopping it stops every container
// (none has a restart policy, the worst case), and refuse makes it fail to
// come up from where its data is now.
type fakeDocker struct {
	mu      sync.Mutex
	m       *Mover
	up      bool
	running []string
	images  int
	refuse  func() bool
	calls   []string
	// the mount units mounted now, and the ones whose stop fails (the
	// folder is busy)
	mounted map[string]bool
	busy    map[string]bool
}

func newFakeDocker(m *Mover) *fakeDocker {
	d := &fakeDocker{m: m, up: true, running: []string{"c1", "c2"}, images: 3, mounted: map[string]bool{}, busy: map[string]bool{}}
	m.Run = d.run
	// a live folder is its copy while its unit is mounted, as a bind mount shows it
	m.SameDir = func(a, b string) bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, ar := range areas {
			if ar.live == a && d.mounted[ar.unit] {
				unit, _ := os.ReadFile(m.path(unitPath(ar.unit)))
				return unitValue(string(unit), "What") == b
			}
		}
		return a == b
	}
	m.Wait, m.Poll = 200*time.Millisecond, time.Millisecond
	return d
}

func (d *fakeDocker) run(_ context.Context, name string, args ...string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, name+" "+strings.Join(args, " "))

	switch {
	case name == "systemctl" && len(args) == 2 && args[1] == "docker.service":
		if args[0] == "stop" {
			d.up, d.running = false, nil
		} else if args[0] == "start" {
			d.up = true
		}
		return "", nil
	case name == "systemctl" && strings.HasSuffix(args[len(args)-1], ".mount"):
		unit := args[len(args)-1]
		switch args[0] {
		case "stop":
			if d.busy[unit] {
				return "", errors.New("umount: target is busy")
			}
			d.mounted[unit] = false
		case "start":
			d.mounted[unit] = true
		case "enable":
			if slices.Contains(args, "--now") {
				d.mounted[unit] = true
			}
		}
		return "", nil
	case name == "systemctl":
		return "", nil
	case name != "docker":
		return "", nil
	}

	if !d.up || (d.refuse != nil && d.refuse()) {
		return "", errors.New("Cannot connect to the Docker daemon")
	}
	switch args[0] {
	case "info":
		return "/var/lib/docker\n", nil
	case "ps":
		return strings.Join(d.running, "\n"), nil
	case "images":
		return strings.Repeat("sha256:x\n", d.images), nil
	case "start":
		for _, id := range args[1:] {
			if !slices.Contains(d.running, id) {
				d.running = append(d.running, id)
			}
		}
	}
	return "", nil
}

func (d *fakeDocker) state() (bool, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.up, slices.Clone(d.running)
}

// copyAll copies a tree as rsync would; failAfter > 0 stops with err after
// that many files, leaving a partial copy as a cut-off rsync does. A live
// folder is read where its mount unit points, as the bind mount would show it.
func copyAll(m *Mover, failAfter int, failErr error) func(context.Context, string, string) error {
	return func(_ context.Context, src, dst string) error {
		from := m.path(src)
		if loc := m.location(); loc != "" {
			for _, a := range areas {
				if a.live == src {
					from = m.path(filepath.Join(loc, a.name))
				}
			}
		}
		n := 0
		return filepath.WalkDir(from, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(from, p)
			out := filepath.Join(m.path(dst), rel)
			if e.IsDir() {
				return os.MkdirAll(out, 0o711)
			}
			if failAfter > 0 && n == failAfter {
				_ = os.WriteFile(out, []byte("half"), 0o644)
				return failErr
			}
			n++
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, 0o644)
		})
	}
}

// liveHost is a host with data in Docker's default place.
func liveHost(t *testing.T) *Mover {
	t.Helper()
	m := host(t)
	for _, f := range []string{"/var/lib/docker/volumes/v1/data", "/var/lib/docker/volumes/v2/data", "/var/lib/docker/image/db", "/var/lib/containerd/io/blob"} {
		if err := os.MkdirAll(filepath.Dir(m.path(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.path(f), []byte("live "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func runMove(t *testing.T, m *Mover, target string) State {
	t.Helper()
	if _, err := m.Start(target); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for m.State().busy() {
		if time.Now().After(deadline) {
			t.Fatalf("the move did not end: %+v", m.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
	return m.State()
}

func there(m *Mover, p string) bool {
	_, err := os.Stat(m.path(p))
	return err == nil
}

// asBefore checks a host is back as it was before a move from the default
// place: data in place and untouched, no units, no copies, Docker up with
// the same containers.
func asBefore(t *testing.T, m *Mover, d *fakeDocker, target string) {
	t.Helper()
	if b, err := os.ReadFile(m.path("/var/lib/docker/volumes/v1/data")); err != nil || string(b) != "live /var/lib/docker/volumes/v1/data" {
		t.Errorf("the live data changed: %q %v", b, err)
	}
	for _, p := range []string{
		unitPath("var-lib-docker.mount"), unitPath("var-lib-containerd.mount"),
		dropInPath("docker.service"), dropInPath("containerd.service"),
		"/var/lib/docker" + oldSuffix, "/var/lib/containerd" + oldSuffix,
		filepath.Join(target, "docker"), filepath.Join(target, "containerd"),
	} {
		if there(m, p) {
			t.Errorf("%s is left", p)
		}
	}
	if !there(m, target) {
		t.Errorf("the target folder itself went away")
	}
	if m.location() != "" {
		t.Errorf("location %q", m.location())
	}
	up, running := d.state()
	if !up || strings.Join(running, ",") != "c1,c2" {
		t.Errorf("Docker up %v, running %v", up, running)
	}
}

func TestMoveAndBack(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)

	s := runMove(t, m, "/DATA/big")
	if s.Phase != PhaseDone || s.Location != "/DATA/big" {
		t.Fatalf("%+v", s)
	}
	if b, _ := os.ReadFile(m.path("/DATA/big/docker/volumes/v1/data")); string(b) != "live /var/lib/docker/volumes/v1/data" {
		t.Errorf("copy: %q", b)
	}
	if strings.Join(s.OldCopy, ",") != "/var/lib/containerd.casaos-old,/var/lib/docker.casaos-old" {
		t.Errorf("old copy %v", s.OldCopy)
	}
	if _, running := d.state(); strings.Join(running, ",") != "c1,c2" {
		t.Errorf("running %v", running)
	}

	s = runMove(t, m, Default)
	if s.Phase != PhaseDone || s.Location != "" || m.location() != "" {
		t.Fatalf("back: %+v", s)
	}
	if b, _ := os.ReadFile(m.path("/var/lib/docker/volumes/v1/data")); string(b) != "live /var/lib/docker/volumes/v1/data" {
		t.Errorf("back: %q", b)
	}
	if !slices.Contains(s.OldCopy, "/DATA/big/docker") {
		t.Errorf("old copy after the way back %v", s.OldCopy)
	}
}

// A copy cut off halfway, by a full drive or anything else: the partial copy
// goes, Docker and the containers start again from where they were.
func TestMoveCopyFailsHalfway(t *testing.T) {
	for name, cut := range map[string]struct {
		after int
		err   error
	}{
		"disk full in docker":   {after: 2, err: errors.New(`rsync: exit status 11: rsync: write failed on "/DATA/big/docker/volumes/v2/data": No space left on device (28)`)},
		"cut off in containerd": {after: 1, err: errors.New("rsync: exit status 20: received SIGTERM")},
	} {
		t.Run(name, func(t *testing.T) {
			m := liveHost(t)
			d := newFakeDocker(m)
			m.Copy = copyAll(m, cut.after, cut.err)

			s := runMove(t, m, "/DATA/big")
			if s.Phase != PhaseFailed || !strings.Contains(s.Message, cut.err.Error()) || s.Location != "" {
				t.Fatalf("%+v", s)
			}
			asBefore(t, m, d, "/DATA/big")
		})
	}
}

// Docker does not come up from the new place: the switch is undone, the
// copy deleted, and Docker starts from the old place with the containers.
func TestMoveDockerRefusesNewPlace(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = copyAll(m, 0, nil)
	d.refuse = func() bool { return m.location() != "" }

	s := runMove(t, m, "/DATA/big")
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "did not come back") || s.Location != "" {
		t.Fatalf("%+v", s)
	}
	asBefore(t, m, d, "/DATA/big")
}

// Docker comes up but an image or a container is missing: same as above.
func TestMoveLosesAnImage(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.Copy = func(ctx context.Context, src, dst string) error {
		d.mu.Lock()
		d.images = 2
		d.mu.Unlock()
		return copyAll(m, 0, nil)(ctx, src, dst)
	}

	s := runMove(t, m, "/DATA/big")
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "3 images before the move, 2 after") {
		t.Fatalf("%+v", s)
	}
	d.images = 3
	asBefore(t, m, d, "/DATA/big")
}

// The way back from another folder, failing at each step: Docker stays on
// the folder it was moved to, the units stay, the copy next to the live
// folders goes.
func TestMoveBackFails(t *testing.T) {
	for name, setup := range map[string]func(m *Mover, d *fakeDocker){
		"copy cut off": func(m *Mover, d *fakeDocker) {
			m.Copy = copyAll(m, 1, errors.New("rsync: No space left on device (28)"))
		},
		"Docker refuses /var": func(m *Mover, d *fakeDocker) { d.refuse = func() bool { return m.location() == "" } },
	} {
		t.Run(name, func(t *testing.T) {
			m := liveHost(t)
			d := newFakeDocker(m)
			m.Copy = copyAll(m, 0, nil)
			if s := runMove(t, m, "/DATA/big"); s.Phase != PhaseDone {
				t.Fatalf("there: %+v", s)
			}
			// the live folders are mount points of /DATA/big now; here they
			// are left empty, as they look with the mounts stopped
			for _, a := range areas {
				_ = os.RemoveAll(m.path(a.live))
				_ = os.Mkdir(m.path(a.live), 0o711)
			}
			unit, _ := os.ReadFile(m.path(unitPath("var-lib-docker.mount")))

			setup(m, d)
			s := runMove(t, m, Default)
			if s.Phase != PhaseFailed || s.Location != "/DATA/big" || m.location() != "/DATA/big" {
				t.Fatalf("%+v", s)
			}
			if b, _ := os.ReadFile(m.path(unitPath("var-lib-docker.mount"))); string(b) != string(unit) {
				t.Errorf("the unit changed:\n%s", b)
			}
			for _, a := range areas {
				if there(m, a.live+newSuffix) {
					t.Errorf("%s is left", a.live+newSuffix)
				}
				if !there(m, a.live) {
					t.Errorf("%s, the mount point, is gone", a.live)
				}
			}
			if b, _ := os.ReadFile(m.path("/DATA/big/docker/volumes/v1/data")); string(b) != "live /var/lib/docker/volumes/v1/data" {
				t.Errorf("the data in /DATA/big changed: %q", b)
			}
			up, running := d.state()
			if !up || strings.Join(running, ",") != "c1,c2" {
				t.Errorf("Docker up %v, running %v", up, running)
			}
		})
	}
}

// A drive too small is refused before anything stops.
func TestMoveRefusedWhenTheDriveIsTooSmall(t *testing.T) {
	m := liveHost(t)
	d := newFakeDocker(m)
	m.StatFS = func(string) (string, int64, error) { return "ext4", 1 << 20, nil }

	if _, err := m.Start("/DATA/big"); err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("started: %v", err)
	}
	for _, c := range d.calls {
		if strings.HasPrefix(c, "systemctl stop") {
			t.Errorf("stopped something: %s", c)
		}
	}
}
