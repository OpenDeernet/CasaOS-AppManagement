// Package dockerlocation moves Docker's data, and containerd's where Docker
// keeps its images there (the containerd image store, Docker 29's default),
// to another folder. The folders Docker and containerd use stay where they
// are: /var/lib/docker and /var/lib/containerd become bind mounts of the new
// folder's docker and containerd folders, systemd mount units that the two
// services require. Neither service's configuration is touched.
//
// A move stops Docker, copies, switches the mounts, starts Docker again and
// checks that what ran before runs again; anything wrong puts everything back
// as it was. The old data is kept until the owner deletes it.
package dockerlocation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Phases of a move, as the dashboard shows them.
const (
	PhaseIdle      = "idle"
	PhaseStopping  = "stopping"
	PhaseCopying   = "copying"
	PhaseSwitching = "switching"
	PhaseStarting  = "starting"
	PhaseDone      = "done"
	PhaseFailed    = "failed" // nothing changed, or everything was put back
)

// Roots may hold Docker's data: the data folder and the drives mounted under
// /media.
var Roots = []string{"/DATA", "/media"}

// what is moved: the folder the service uses, and the name of its copy in
// the target folder
type area struct {
	live    string // /var/lib/docker
	name    string // docker
	service string // the service that uses it
	unit    string // its mount unit
}

var areas = []area{
	{live: "/var/lib/containerd", name: "containerd", service: "containerd.service", unit: "var-lib-containerd.mount"},
	{live: "/var/lib/docker", name: "docker", service: "docker.service", unit: "var-lib-docker.mount"},
}

// oldSuffix names the data a move from the default place leaves behind;
// newSuffix the copy a move back to it makes before it takes the place.
const (
	oldSuffix = ".casaos-old"
	newSuffix = ".casaos-new"
)

// Default is the target that puts Docker's data back in its default place.
const Default = ""

// State is where Docker's data is, and the move under way or the last one.
type State struct {
	Phase       string     `json:"phase"`
	Message     string     `json:"message,omitempty"`
	Location    string     `json:"location"` // the folder holding docker/ and containerd/; empty: the default place
	Target      string     `json:"target,omitempty"`
	TotalBytes  int64      `json:"total_bytes,omitempty"`
	CopiedBytes int64      `json:"copied_bytes,omitempty"`
	OldCopy     []string   `json:"old_copy,omitempty"` // what the last move left behind, to delete
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// Check says whether a folder can take Docker's data.
type Check struct {
	Path       string `json:"path"`
	FSType     string `json:"fs_type"`
	FreeBytes  int64  `json:"free_bytes"`
	NeedBytes  int64  `json:"need_bytes"`
	OK         bool   `json:"ok"`
	Reason     string `json:"reason,omitempty"`
	Location   string `json:"location"`
	UsedBytes  int64  `json:"used_bytes"`
	Containers int    `json:"containers"`
}

// Mover runs one move at a time and remembers where the data is.
type Mover struct {
	// Root prefixes every host path; empty on a real host, a folder in tests.
	Root string
	// StatePath is where State is kept across restarts.
	StatePath string
	// Run runs a command; tests replace it.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// StatFS says a folder's file system and free space; tests replace it.
	StatFS func(path string) (fsType string, freeBytes int64, err error)
	// Copy copies a live folder into its new place; tests replace it.
	Copy func(ctx context.Context, src, dst string) error
	// Wait is how long Docker and the containers get to come back after a
	// switch, Poll how often they are asked; zero: 2 minutes, 2 seconds.
	Wait, Poll time.Duration

	mu    sync.Mutex
	state State
}

func New(statePath string) *Mover {
	m := &Mover{StatePath: statePath, Run: run, StatFS: statFS}
	m.Copy = m.copyTree
	m.load()
	return m
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (m *Mover) path(p string) string { return filepath.Join(m.Root, p) }

func now() *time.Time {
	t := time.Now().UTC()
	return &t
}

// load reads the state a previous run kept. A move cut short by a stop or a
// crash is reported failed: the mounts say where the data really is.
func (m *Mover) load() {
	m.state = State{Phase: PhaseIdle}
	if b, err := os.ReadFile(m.StatePath); err == nil {
		_ = json.Unmarshal(b, &m.state)
	}
	if m.state.Phase != PhaseIdle && m.state.Phase != PhaseDone && m.state.Phase != PhaseFailed {
		m.state.Phase = PhaseFailed
		m.state.Message = "the move was interrupted; check that the apps run"
	}
	m.state.Location = m.location()
}

func (m *Mover) save() {
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err == nil {
		_ = os.MkdirAll(filepath.Dir(m.StatePath), 0o755)
		_ = os.WriteFile(m.StatePath, b, 0o644)
	}
}

func (m *Mover) set(f func(s *State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.state)
	m.save()
}

// State is a copy of the state now.
func (m *Mover) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	s.OldCopy = slices.Clone(s.OldCopy)
	return s
}

// busy reports whether a move is under way.
func (s State) busy() bool {
	return s.Phase != PhaseIdle && s.Phase != PhaseDone && s.Phase != PhaseFailed
}

// location is the folder the docker mount unit points into; empty when
// there is no such unit and Docker's data is in its default place.
func (m *Mover) location() string {
	b, err := os.ReadFile(m.path(unitPath("var-lib-docker.mount")))
	if err != nil {
		return ""
	}
	what := unitValue(string(b), "What")
	if what == "" {
		return ""
	}
	return filepath.Dir(what)
}

func unitPath(unit string) string { return filepath.Join("/etc/systemd/system", unit) }

func dropInPath(service string) string {
	return filepath.Join("/etc/systemd/system", service+".d", "50-casaos-data-location.conf")
}

func unitValue(unit, key string) string {
	sc := bufio.NewScanner(strings.NewReader(unit))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// MountUnit is the systemd unit binding what onto where, after the folder's
// own mount.
func MountUnit(what, where, target, before string) string {
	return fmt.Sprintf(`# Written by CasaOS: Docker's data lives in %s.
[Unit]
Description=Docker data in %s
RequiresMountsFor=%s
Before=%s

[Mount]
What=%s
Where=%s
Type=none
Options=bind

[Install]
WantedBy=local-fs.target
`, target, target, target, before, what, where)
}

// DropIn makes a service wait for its folder's mount.
func DropIn(where string) string {
	return fmt.Sprintf("# Written by CasaOS: Docker's data lives elsewhere, bind mounted here.\n[Unit]\nRequiresMountsFor=%s\n", where)
}

var safePath = regexp.MustCompile(`^/[A-Za-z0-9._/+@-]+$`)

// overlay needs a real Linux file system under it
var goodFS = map[int64]string{
	0xef53:     "ext4",
	0x58465342: "xfs",
	0x9123683e: "btrfs",
}

// CheckTarget says whether path can take the data now in its place.
func (m *Mover) CheckTarget(ctx context.Context, path string) Check {
	c := Check{Path: filepath.Clean(path), Location: m.State().Location}
	c.UsedBytes = m.usedBytes()
	c.NeedBytes = c.UsedBytes + c.UsedBytes/10 + 64<<20
	if out, err := m.Run(ctx, "docker", "ps", "-aq"); err == nil {
		c.Containers = len(strings.Fields(out))
	}

	fail := func(reason string) Check { c.Reason = reason; return c }

	if path == Default {
		c.Path = Default
		if c.Location == "" {
			return fail("Docker's data is in its default place already")
		}
		fsType, free, err := m.StatFS(m.path("/var/lib"))
		if err != nil {
			return fail(err.Error())
		}
		c.FSType, c.FreeBytes = fsType, free
		for _, a := range areas {
			if _, err := os.Stat(m.path(a.live + newSuffix)); err == nil {
				return fail(a.live + newSuffix + " is there: a move back did not finish")
			}
		}
		if c.FreeBytes < c.NeedBytes {
			return fail(fmt.Sprintf("the system disk has %d MB free, the move needs %d MB", c.FreeBytes>>20, c.NeedBytes>>20))
		}
		c.OK = true
		return c
	}

	if !filepath.IsAbs(path) || !safePath.MatchString(c.Path) {
		return fail("give a folder path of letters, digits and . _ - + @ only")
	}
	under := false
	for _, root := range Roots {
		if c.Path == root || strings.HasPrefix(c.Path, root+"/") {
			under = true
		}
	}
	if !under {
		return fail("Docker's data can go under " + strings.Join(Roots, " or "))
	}
	if c.Path == c.Location {
		return fail("Docker's data is there already")
	}

	st, err := os.Stat(m.path(c.Path))
	if err != nil || !st.IsDir() {
		return fail("the folder does not exist")
	}

	fsType, free, err := m.StatFS(m.path(c.Path))
	if err != nil {
		return fail(err.Error())
	}
	c.FSType, c.FreeBytes = fsType, free
	if !slices.Contains([]string{"ext4", "xfs", "btrfs"}, fsType) {
		return fail("the drive's file system cannot hold Docker's data: it takes ext4, xfs or btrfs")
	}

	// a drive mounted by hand, or by the dashboard without fstab, may not be
	// there yet when Docker starts at boot
	if mp := m.mountPoint(c.Path); mp != "/" && !m.inFstab(mp) {
		return fail(fmt.Sprintf("the drive at %s is not mounted at boot (it is not in /etc/fstab), so Docker could start without it", mp))
	}

	for _, a := range areas {
		if !emptyOrMissing(m.path(filepath.Join(c.Path, a.name))) {
			return fail(fmt.Sprintf("%s is not empty", filepath.Join(c.Path, a.name)))
		}
	}
	if c.FreeBytes < c.NeedBytes {
		return fail(fmt.Sprintf("the drive has %d MB free, the move needs %d MB", c.FreeBytes>>20, c.NeedBytes>>20))
	}

	c.OK = true
	return c
}

func statFS(path string) (string, int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return "", 0, err
	}
	name, ok := goodFS[int64(fs.Type)]
	if !ok {
		name = fmt.Sprintf("0x%x", fs.Type)
	}
	return name, int64(fs.Bavail) * int64(fs.Bsize), nil
}

// mountPoint is the mount point of the file system holding path: the
// highest folder on the same device.
func (m *Mover) mountPoint(path string) string {
	dev := func(p string) (uint64, bool) {
		st, err := os.Stat(m.path(p))
		if err != nil {
			return 0, false
		}
		s, ok := st.Sys().(*syscall.Stat_t)
		return uint64(s.Dev), ok
	}

	here, ok := dev(path)
	if !ok {
		return "/"
	}
	for path != "/" {
		parent := filepath.Dir(path)
		if d, ok := dev(parent); !ok || d != here {
			return path
		}
		path = parent
	}
	return "/"
}

// inFstab reports whether /etc/fstab mounts something at mp.
func (m *Mover) inFstab(mp string) bool {
	b, err := os.ReadFile(m.path("/etc/fstab"))
	if err != nil {
		return false
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && !strings.HasPrefix(f[0], "#") && filepath.Clean(strings.ReplaceAll(f[1], `\040`, " ")) == mp {
			return true
		}
	}
	return false
}

func emptyOrMissing(dir string) bool {
	entries, err := os.ReadDir(dir)
	return errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0)
}

// usedBytes is what the areas take on disk, not crossing into other file
// systems (the overlay mounts of running containers).
func (m *Mover) usedBytes() int64 {
	var total int64
	for _, a := range areas {
		root := m.path(a.live)
		var dev uint64
		if st, err := os.Lstat(root); err == nil {
			if s, ok := st.Sys().(*syscall.Stat_t); ok {
				dev = uint64(s.Dev)
			}
		}
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			s, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return nil
			}
			if info.IsDir() && uint64(s.Dev) != dev {
				return filepath.SkipDir
			}
			total += int64(s.Blocks) * 512
			return nil
		})
	}
	return total
}

// Start begins a move to target in the background, after checking it.
func (m *Mover) Start(target string) (State, error) {
	m.mu.Lock()
	if m.state.busy() {
		m.mu.Unlock()
		return m.State(), errors.New("a move is under way")
	}
	m.mu.Unlock()

	c := m.CheckTarget(context.Background(), target)
	if !c.OK {
		return m.State(), errors.New(c.Reason)
	}

	m.set(func(s *State) {
		*s = State{Phase: PhaseStopping, Location: s.Location, Target: c.Path, TotalBytes: c.UsedBytes, OldCopy: s.OldCopy, StartedAt: now()}
	})

	go m.move(c.Path)

	return m.State(), nil
}

// move does the work; every step it cannot finish is undone.
func (m *Mover) move(target string) {
	ctx := context.Background()
	from := m.State().Location

	fail := func(err error) {
		m.set(func(s *State) {
			s.Phase, s.Message, s.FinishedAt = PhaseFailed, err.Error(), now()
			s.Location = m.location()
		})
	}

	// what runs now, to run again after
	running := m.runningContainers(ctx)
	images := m.imageCount(ctx)

	// Docker back where it was, with the containers that ran: a container
	// without a restart policy does not come back with Docker alone
	restart := func() {
		m.startServices(ctx)
		m.startContainers(ctx, running)
	}

	if err := m.stopServices(ctx); err != nil {
		restart()
		fail(fmt.Errorf("stop Docker: %w", err))
		return
	}

	// a container's file system can stay mounted once Docker stops: it would
	// be copied as it looks from above, and pin the old folder
	for _, a := range areas {
		if err := m.unmountUnder(ctx, a.live, false); err != nil {
			restart()
			fail(fmt.Errorf("unmount what Docker left mounted: %w", err))
			return
		}
	}

	m.set(func(s *State) { s.Phase = PhaseCopying })
	var copied []string
	for _, a := range areas {
		if _, err := os.Stat(m.path(a.live)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		dst := filepath.Join(target, a.name)
		if target == Default {
			dst = a.live + newSuffix
		}
		copied = append(copied, dst)
		if err := m.copy(ctx, a.live, dst); err != nil {
			m.removeAll(copied)
			restart()
			fail(fmt.Errorf("copy %s: %w", a.live, err))
			return
		}
	}

	m.set(func(s *State) { s.Phase = PhaseSwitching })
	switchTo := m.switchMounts
	if target == Default {
		switchTo = m.switchToDefault
	}
	undo, err := switchTo(ctx, from, target)
	if err != nil {
		undo()
		m.removeAll(copied)
		restart()
		fail(fmt.Errorf("switch to %s: %w", target, err))
		return
	}

	m.set(func(s *State) { s.Phase = PhaseStarting })
	if err := m.startAndVerify(ctx, target, running, images); err != nil {
		_ = m.stopServices(ctx)
		undo()
		m.removeAll(copied)
		restart()
		fail(fmt.Errorf("Docker did not come back in %s, so everything is as it was: %w", target, err))
		return
	}

	old := m.oldCopy(from)
	m.set(func(s *State) {
		s.Phase, s.Message, s.FinishedAt = PhaseDone, "", now()
		// what earlier moves left stays listed until it is deleted
		for _, p := range old {
			if !slices.Contains(s.OldCopy, p) {
				s.OldCopy = append(s.OldCopy, p)
			}
		}
		s.Location, s.Target = target, ""
	})
}

func (m *Mover) stopServices(ctx context.Context) error {
	// the socket would start Docker again at the first request
	for _, unit := range []string{"docker.socket", "docker.service", "containerd.service"} {
		if _, err := m.Run(ctx, "systemctl", "stop", unit); err != nil && unit != "docker.socket" {
			return err
		}
	}
	return nil
}

func (m *Mover) startServices(ctx context.Context) {
	for _, unit := range []string{"containerd.service", "docker.socket", "docker.service"} {
		_, _ = m.Run(ctx, "systemctl", "start", unit)
	}
}

// copyTree copies src into dst keeping owners, modes, links, ACLs and
// extended attributes; rsync reports how far it is.
var progress2 = regexp.MustCompile(`^\s*([0-9,]+)\s+\d+%`)

func (m *Mover) copy(ctx context.Context, src, dst string) error {
	if m.Copy != nil {
		return m.Copy(ctx, src, dst)
	}
	return m.copyTree(ctx, src, dst)
}

func (m *Mover) copyTree(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(m.path(dst), 0o711); err != nil {
		return err
	}

	base := m.State().CopiedBytes
	cmd := exec.CommandContext(ctx, "rsync", "-aHAXS", "--numeric-ids", "--info=progress2", "--no-inc-recursive",
		m.path(src)+"/", m.path(dst)+"/")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// numbers as 1,234,567 whatever the host's language
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if err := cmd.Start(); err != nil {
		return err
	}

	r := bufio.NewReader(out)
	for {
		line, err := r.ReadString('\r')
		if n := ParseProgress(line); n > 0 {
			m.set(func(s *State) { s.CopiedBytes = base + n })
		}
		if err == io.EOF {
			break
		} else if err != nil {
			break
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("rsync: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ParseProgress reads the bytes copied from a line of rsync --info=progress2.
func ParseProgress(line string) int64 {
	mm := progress2.FindStringSubmatch(line)
	if mm == nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.ReplaceAll(mm[1], ",", ""), 10, 64)
	return n
}

// switchMounts points the live folders at target and returns how to undo
// it. From the default place the old folders are renamed aside; from another
// folder the units are rewritten, and the old folder stays as it was.
func (m *Mover) switchMounts(ctx context.Context, from, target string) (func(), error) {
	var undos []func()
	undo := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
		_, _ = m.Run(ctx, "systemctl", "daemon-reload")
	}

	for _, a := range areas {
		if _, err := os.Stat(m.path(filepath.Join(target, a.name))); errors.Is(err, os.ErrNotExist) {
			continue
		}

		unitFile := m.path(unitPath(a.unit))
		dropIn := m.path(dropInPath(a.service))
		prevUnit, _ := os.ReadFile(unitFile)
		prevDrop, _ := os.ReadFile(dropIn)

		if from != "" {
			_, _ = m.Run(ctx, "systemctl", "stop", a.unit)
			undos = append(undos, func() { _, _ = m.Run(ctx, "systemctl", "start", a.unit) })
		} else {
			live, aside := m.path(a.live), m.path(a.live+oldSuffix)
			if err := os.Rename(live, aside); err != nil {
				return undo, err
			}
			undos = append(undos, func() {
				_ = os.Remove(live)
				_ = os.Rename(aside, live)
			})
			if err := os.Mkdir(live, 0o711); err != nil {
				return undo, err
			}
		}

		undos = append(undos, func() {
			restore(unitFile, prevUnit)
			restore(dropIn, prevDrop)
		})
		if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
			return undo, err
		}
		if err := os.WriteFile(unitFile, []byte(MountUnit(filepath.Join(target, a.name), a.live, target, a.service)), 0o644); err != nil {
			return undo, err
		}
		if err := os.WriteFile(dropIn, []byte(DropIn(a.live)), 0o644); err != nil {
			return undo, err
		}
	}

	if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return undo, err
	}

	for _, a := range areas {
		if _, err := os.Stat(m.path(filepath.Join(target, a.name))); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := m.Run(ctx, "systemctl", "enable", "--now", a.unit); err != nil {
			return undo, err
		}
		undos = append(undos, func() {
			_, _ = m.Run(ctx, "systemctl", "stop", a.unit)
			if from == "" {
				_, _ = m.Run(ctx, "systemctl", "disable", a.unit)
			}
		})
	}

	return undo, nil
}

// switchToDefault takes the mounts away and puts the copy made next to the
// live folders in their place; from's folders become the old copy.
func (m *Mover) switchToDefault(ctx context.Context, from, _ string) (func(), error) {
	var undos []func()
	undo := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
	}

	for _, a := range areas {
		if _, err := os.Stat(m.path(a.live + newSuffix)); err != nil {
			continue
		}

		unitFile := m.path(unitPath(a.unit))
		dropIn := m.path(dropInPath(a.service))
		prevUnit, _ := os.ReadFile(unitFile)
		prevDrop, _ := os.ReadFile(dropIn)

		if _, err := m.Run(ctx, "systemctl", "stop", a.unit); err != nil {
			return undo, err
		}
		undos = append(undos, func() { _, _ = m.Run(ctx, "systemctl", "start", a.unit) })

		_, _ = m.Run(ctx, "systemctl", "disable", a.unit)
		undos = append(undos, func() { _, _ = m.Run(ctx, "systemctl", "enable", a.unit) })

		_ = os.Remove(unitFile)
		_ = os.Remove(dropIn)
		undos = append(undos, func() {
			restore(unitFile, prevUnit)
			restore(dropIn, prevDrop)
			_, _ = m.Run(ctx, "systemctl", "daemon-reload")
		})

		// the live folder is the empty mount point now
		live, fresh := m.path(a.live), m.path(a.live+newSuffix)
		if err := os.Remove(live); err != nil {
			return undo, fmt.Errorf("%s is not empty once unmounted: %w", a.live, err)
		}
		if err := os.Rename(fresh, live); err != nil {
			_ = os.Mkdir(live, 0o711)
			return undo, err
		}
		undos = append(undos, func() {
			_ = os.Rename(live, fresh)
			_ = os.Mkdir(live, 0o711)
		})
	}

	if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return undo, err
	}
	return undo, nil
}

// restore puts back a file as it was; nil content: it did not exist.
func restore(path string, content []byte) {
	if content == nil {
		_ = os.Remove(path)
		return
	}
	_ = os.WriteFile(path, content, 0o644)
}

func (m *Mover) startAndVerify(ctx context.Context, target string, running []string, images int) error {
	m.startServices(ctx)

	wait, poll := m.Wait, m.Poll
	if wait == 0 {
		wait = 2 * time.Minute
	}
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	var err error
	answered := false
	for !answered && time.Now().Before(deadline) {
		var out string
		if out, err = m.Run(ctx, "docker", "info", "--format", "{{.DockerRootDir}}"); err == nil && strings.TrimSpace(out) != "" {
			answered = true
			break
		}
		time.Sleep(poll)
	}
	if !answered {
		if err == nil {
			err = errors.New("no answer")
		}
		return fmt.Errorf("Docker does not answer: %w", err)
	}

	if n := m.imageCount(ctx); n < images {
		return fmt.Errorf("%d images before the move, %d after", images, n)
	}

	m.startContainers(ctx, running)
	for time.Now().Before(deadline) {
		now := m.runningContainers(ctx)
		if missing := without(running, now); len(missing) == 0 {
			return nil
		}
		time.Sleep(poll)
	}
	return fmt.Errorf("these containers did not start again: %s", strings.Join(without(running, m.runningContainers(ctx)), ", "))
}

func (m *Mover) runningContainers(ctx context.Context) []string {
	out, err := m.Run(ctx, "docker", "ps", "-q", "--no-trunc")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

func (m *Mover) imageCount(ctx context.Context) int {
	out, err := m.Run(ctx, "docker", "images", "-aq")
	if err != nil {
		return 0
	}
	return len(strings.Fields(out))
}

func (m *Mover) startContainers(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	_, _ = m.Run(ctx, "docker", append([]string{"start"}, ids...)...)
}

func without(want, have []string) []string {
	var missing []string
	for _, id := range want {
		if !slices.Contains(have, id) {
			missing = append(missing, id)
		}
	}
	return missing
}

// oldCopy is what a move from `from` left behind.
func (m *Mover) oldCopy(from string) []string {
	var old []string
	for _, a := range areas {
		p := a.live + oldSuffix
		if from != "" {
			p = filepath.Join(from, a.name)
		}
		if _, err := os.Stat(m.path(p)); err == nil {
			old = append(old, p)
		}
	}
	return old
}

func (m *Mover) removeAll(paths []string) {
	for _, p := range paths {
		_ = m.removeTree(context.Background(), p)
	}
}

// mountsUnder lists the mount points under dir, dir itself when self is
// set, deepest first, from /proc/self/mountinfo.
func (m *Mover) mountsUnder(dir string, self bool) []string {
	b, err := os.ReadFile(m.path("/proc/self/mountinfo"))
	if err != nil {
		return nil
	}

	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		mp := strings.ReplaceAll(f[4], `\040`, " ")
		if (self && mp == dir) || strings.HasPrefix(mp, dir+"/") {
			out = append(out, mp)
		}
	}

	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// unmountUnder unmounts every mount under dir, deepest first.
func (m *Mover) unmountUnder(ctx context.Context, dir string, self bool) error {
	for _, mp := range m.mountsUnder(dir, self) {
		if _, err := m.Run(ctx, "umount", mp); err != nil {
			return err
		}
	}
	if left := m.mountsUnder(dir, self); len(left) > 0 {
		return fmt.Errorf("still mounted: %s", strings.Join(left, ", "))
	}
	return nil
}

// removeTree deletes a folder a move made or left, first unmounting what is
// mounted in it, and never crossing into another file system: what is
// mounted there is not the move's to delete.
func (m *Mover) removeTree(ctx context.Context, p string) error {
	if err := m.unmountUnder(ctx, p, true); err != nil {
		return err
	}

	root := m.path(p)
	st, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	dev := st.Sys().(*syscall.Stat_t).Dev

	var crossed string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && info.Sys().(*syscall.Stat_t).Dev != dev {
			crossed = path
			return filepath.SkipAll
		}
		return nil
	})
	if crossed != "" {
		return fmt.Errorf("%s is another file system; nothing was deleted", crossed)
	}

	return os.RemoveAll(root)
}

// DeleteOldCopy deletes what the last move left behind.
func (m *Mover) DeleteOldCopy() (State, error) {
	s := m.State()
	if s.busy() {
		return s, errors.New("a move is under way")
	}
	if len(s.OldCopy) == 0 {
		return s, errors.New("there is no old copy")
	}
	for _, p := range s.OldCopy {
		// only what a move left: never the live folders or the location
		if p == "/var/lib/docker" || p == "/var/lib/containerd" || strings.HasPrefix(s.Location+"/", p+"/") {
			return s, fmt.Errorf("%s is in use", p)
		}
	}
	var kept []string
	var errs []string
	for _, p := range s.OldCopy {
		if err := m.removeTree(context.Background(), p); err != nil {
			kept = append(kept, p)
			errs = append(errs, err.Error())
		}
	}
	m.set(func(st *State) { st.OldCopy = kept })
	if len(errs) > 0 {
		return m.State(), fmt.Errorf("not all was deleted: %s", strings.Join(errs, "; "))
	}
	return m.State(), nil
}
