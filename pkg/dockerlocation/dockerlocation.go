// Package dockerlocation moves Docker's data, and containerd's where Docker
// keeps its images there (the containerd image store, Docker 29's default),
// to another folder. The folders Docker and containerd use stay where they
// are: /var/lib/docker and /var/lib/containerd become bind mounts of the new
// folder's docker and containerd folders, systemd mount units that the two
// services require. Neither service's configuration is touched.
//
// A move goes from the default place to a folder, or back: never from one
// folder to another, which would go through the default place anyway. It
// stops Docker, copies, switches the mounts, checks that the live folders are
// the new copy, starts Docker again and checks that what ran before runs
// again; anything wrong puts everything back as it was, and says where the
// data is when it could not. The old data is kept until the owner deletes it.
// A move cut short by a stop of this service is finished or undone at its
// next start (Recover).
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
	"sync/atomic"
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
	oldSuffix  = ".casaos-old"
	newSuffix  = ".casaos-new"
	bootSuffix = ".casaos-boot"
)

// Default is the target that puts Docker's data back in its default place,
// DefaultDir; a check of it reports DefaultDir as its path.
const (
	Default    = ""
	DefaultDir = "/var/lib/docker"
)

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

	// kept for Recover while a move runs: where it went from, and the
	// containers to start again
	From    string   `json:"from,omitempty"`
	Running []string `json:"running,omitempty"`
	// how many images Docker had before the move, checked again by Recover
	// on a switch it keeps
	Images int `json:"images,omitempty"`
	// set once a failed switch or start is being undone: a stop then leaves a
	// switch that may look complete, and must be undone, never kept
	Undoing bool `json:"undoing,omitempty"`
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
	// SameDir says whether two paths are the same folder (a bind mount shows
	// its source's device and inode); tests replace it.
	SameDir func(a, b string) bool
	// Wait is how long Docker and the containers get to come back after a
	// switch, Poll how often they are asked; zero: 2 minutes, 2 seconds.
	Wait, Poll time.Duration

	// moving is held from a Start (its check included) to the end of the
	// move, by a delete of the old copy and by Recover: one at a time
	moving sync.Mutex

	mu    sync.Mutex
	state State
	// the move a stop of this service cut short, for Recover
	interrupted State

	// what the areas take, measured at most this often for a check: a walk
	// of a big overlay tree takes minutes
	usedMu      sync.Mutex
	used        usage
	usedAt      time.Time
	usedRefresh atomic.Bool
}

// usage is what the areas hold: on disk (for the room needed) and as file
// sizes (what rsync counts as it copies).
type usage struct{ blocks, apparent int64 }

// usageFresh is how long a measure of the areas serves a check.
const usageFresh = 10 * time.Minute

func New(statePath string) *Mover {
	m := &Mover{StatePath: statePath, Run: run, StatFS: statFS}
	m.Copy = m.copyTree
	m.SameDir = m.sameDir
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

// load reads the state a previous run kept. A move cut short is reported
// failed until Recover has dealt with it: the mounts say where the data is.
func (m *Mover) load() {
	m.state = State{Phase: PhaseIdle}
	if b, err := os.ReadFile(m.StatePath); err == nil {
		_ = json.Unmarshal(b, &m.state)
	}
	if m.state.busy() {
		m.interrupted = m.state
		m.state.Phase = PhaseFailed
		m.state.Message = "the move was interrupted; check that the apps run"
	}
	m.state.Location = m.location()
}

// save writes the state next to its file and renames it over: a stop
// halfway leaves the old state or the new one, never an empty file that
// would hide a move to recover.
func (m *Mover) save() {
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(m.StatePath)
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.CreateTemp(dir, filepath.Base(m.StatePath)+".*")
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return
	}
	_ = os.Chmod(f.Name(), 0o644)
	if err := os.Rename(f.Name(), m.StatePath); err != nil {
		_ = os.Remove(f.Name())
		return
	}
	syncDir(dir)
}

func (m *Mover) set(f func(s *State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.state)
	m.save()
}

// show changes the state the dashboard reads without writing it: how far a
// copy is matters to nobody after a stop.
func (m *Mover) show(f func(s *State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.state)
}

// recovering says whether a move cut short waits for Recover.
func (m *Mover) recovering() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.interrupted.busy()
}

// State is a copy of the state now.
func (m *Mover) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	s.OldCopy = slices.Clone(s.OldCopy)
	s.Running = nil
	s.From = ""
	s.Undoing = false
	return s
}

// busy reports whether a move is under way.
func (s State) busy() bool {
	return s.Phase != PhaseIdle && s.Phase != PhaseDone && s.Phase != PhaseFailed && s.Phase != ""
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

// dockerMounts are what Docker and containerd mount in their folders: the
// file systems of containers, their /dev/shm, their network namespaces. A
// move unmounts these once Docker is stopped; anything else mounted there is
// somebody's disk, and refuses the move.
var dockerMounts = []string{"overlay", "tmpfs", "nsfs", "shm"}

// CheckTarget says whether path can take the data now in its place. It asks
// Docker where its data is, and measures it at most every usageFresh: the
// move measures again before it stops anything.
func (m *Mover) CheckTarget(ctx context.Context, path string) Check {
	c := Check{Path: filepath.Clean(path), Location: m.State().Location}
	u := m.estimate()
	c.UsedBytes = u.blocks
	c.NeedBytes = need(u.blocks)
	if out, err := m.Run(ctx, "docker", "ps", "-aq"); err == nil {
		c.Containers = len(strings.Fields(out))
	}

	fail := func(reason string) Check { c.Reason = reason; return c }

	if reason := m.dockerRootProblem(ctx); reason != "" {
		return fail(reason)
	}

	if path == Default {
		c.Path = DefaultDir
		if c.Location == "" {
			return fail("Docker's data is in its default place already")
		}
		fsType, free, err := m.StatFS(m.path("/var/lib"))
		if err != nil {
			return fail(err.Error())
		}
		c.FSType, c.FreeBytes = fsType, free
		for _, a := range areas {
			if _, err := os.Lstat(m.path(a.live + newSuffix)); err == nil {
				return fail(a.live + newSuffix + " is there: a move back did not finish")
			}
			if foreign := m.foreignMountsUnder(a.live); len(foreign) > 0 {
				return fail(fmt.Sprintf("a drive is mounted in %s (%s): its data would not be moved", a.live, strings.Join(foreign, ", ")))
			}
		}
		if c.FreeBytes < c.NeedBytes {
			return fail(fmt.Sprintf("the system disk has %d MB free, the move needs %d MB", c.FreeBytes>>20, c.NeedBytes>>20))
		}
		c.OK = true
		return c
	}

	// from a folder, only back: another folder is reached through the default place
	if c.Location != "" {
		return fail(fmt.Sprintf("Docker's data is in %s: move it back to the default place first", c.Location))
	}

	if !filepath.IsAbs(path) || !safePath.MatchString(c.Path) {
		return fail("give a folder path of letters, digits and . _ - + @ only")
	}

	// a link under /DATA may point anywhere: the folder it names is checked
	resolved, err := filepath.EvalSymlinks(m.path(c.Path))
	if err != nil {
		return fail("the folder does not exist")
	}
	if c.Path, err = m.unroot(resolved); err != nil {
		return fail(err.Error())
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
		// the live folder is renamed to this: one left by an earlier move would stop the switch after the whole copy
		if _, err := os.Lstat(m.path(a.live + oldSuffix)); err == nil {
			return fail(fmt.Sprintf("%s, the old copy of an earlier move, is still there: delete it first", a.live+oldSuffix))
		}
		// the live folder is renamed aside: a mount point cannot be
		if slices.ContainsFunc(m.mounts(), func(e mountEntry) bool { return e.point == a.live }) {
			return fail(fmt.Sprintf("%s is a mount point of its own: it cannot be moved this way", a.live))
		}
		if foreign := m.foreignMountsUnder(a.live); len(foreign) > 0 {
			return fail(fmt.Sprintf("a drive is mounted in %s (%s): its data would not be moved", a.live, strings.Join(foreign, ", ")))
		}
	}
	if c.FreeBytes < c.NeedBytes {
		return fail(fmt.Sprintf("the drive has %d MB free, the move needs %d MB", c.FreeBytes>>20, c.NeedBytes>>20))
	}

	c.OK = true
	return c
}

// need is the room a copy of used bytes needs: a tenth more, and 64 MB.
func need(used int64) int64 { return used + used/10 + 64<<20 }

// unroot turns a path on the host (under Root) back into the host's view.
func (m *Mover) unroot(p string) (string, error) {
	if m.Root == "" {
		return filepath.Clean(p), nil
	}
	root, err := filepath.EvalSymlinks(m.Root)
	if err != nil {
		root = m.Root
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New("the folder is outside this machine's folders")
	}
	return filepath.Join("/", rel), nil
}

// dockerRootProblem says why Docker's data cannot be moved from here, or "":
// Docker must answer, and keep its data in /var/lib/docker. A data-root set
// in daemon.json would leave this move copying an old or empty folder.
func (m *Mover) dockerRootProblem(ctx context.Context) string {
	out, err := m.Run(ctx, "docker", "info", "--format", "{{.DockerRootDir}}")
	if err != nil || strings.TrimSpace(out) == "" {
		return "Docker does not answer: it must run for its data to be moved"
	}
	if root := strings.TrimSpace(out); root != DefaultDir {
		return fmt.Sprintf("Docker keeps its data in %s (data-root in /etc/docker/daemon.json): this move only knows %s", root, DefaultDir)
	}
	// btrfs layers are subvolumes, file systems of their own that the copy
	// (rsync -x) does not enter: the images would come back empty
	if driver, _ := m.Run(ctx, "docker", "info", "--format", "{{.Driver}}"); strings.TrimSpace(driver) == "btrfs" {
		return "Docker stores its images as btrfs subvolumes (storage driver btrfs): this move cannot copy them"
	}
	return ""
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

// sameDir reports whether a and b are the same folder: a bind mount shows its
// source's device and inode.
func (m *Mover) sameDir(a, b string) bool {
	sa, err := os.Stat(m.path(a))
	if err != nil {
		return false
	}
	sb, err := os.Stat(m.path(b))
	if err != nil {
		return false
	}
	x, ok1 := sa.Sys().(*syscall.Stat_t)
	y, ok2 := sb.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && x.Dev == y.Dev && x.Ino == y.Ino
}

// mountPoint is the mount point of the file system holding path, from the
// mount table: a btrfs subvolume has a device of its own without being a
// mount, so the devices of folders cannot say it.
func (m *Mover) mountPoint(path string) string {
	best := "/"
	for _, e := range m.mounts() {
		if (path == e.point || strings.HasPrefix(path, strings.TrimSuffix(e.point, "/")+"/")) && len(e.point) > len(best) {
			best = e.point
		}
	}
	return best
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

// estimate is what the areas take, as measured at most usageFresh ago; an
// older measure serves while a new one is taken in the background.
func (m *Mover) estimate() usage {
	m.usedMu.Lock()
	u, at := m.used, m.usedAt
	m.usedMu.Unlock()

	if at.IsZero() {
		return m.measure()
	}
	if time.Since(at) > usageFresh && m.usedRefresh.CompareAndSwap(false, true) {
		go func() {
			defer m.usedRefresh.Store(false)
			m.measure()
		}()
	}
	return u
}

// recentMeasure is a measure at most maxAge old, taken now if there is none.
func (m *Mover) recentMeasure(maxAge time.Duration) usage {
	m.usedMu.Lock()
	u, at := m.used, m.usedAt
	m.usedMu.Unlock()
	if !at.IsZero() && time.Since(at) < maxAge {
		return u
	}
	return m.measure()
}

// measure walks the areas, not crossing into other file systems (the
// overlay mounts of running containers), and keeps the result.
func (m *Mover) measure() usage {
	var u usage
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
			u.blocks += int64(s.Blocks) * 512
			if info.Mode().IsRegular() {
				u.apparent += info.Size()
			}
			return nil
		})
	}

	m.usedMu.Lock()
	m.used, m.usedAt = u, time.Now()
	m.usedMu.Unlock()
	return u
}

// errBusy is the answer while a move, a delete or a recovery runs.
var errBusy = errors.New("a move is under way")

// lockIdle takes the move lock. A move records that it ended a moment before
// it lets go of the lock: when the state says a move just ended, the lock is
// waited for briefly instead of refusing the click that follows. Anything
// else holding it (a check under way, Recover) is refused at once.
func (m *Mover) lockIdle() bool {
	if m.moving.TryLock() {
		return true
	}
	if p := m.State().Phase; p != PhaseDone && p != PhaseFailed {
		return false
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		if m.moving.TryLock() {
			return true
		}
		if m.State().busy() {
			return false
		}
	}
	return false
}

// Start begins a move to target in the background, after checking it. From
// the check to the end of the move nothing else can start: a second click
// while a slow check runs is refused, not a second move into the same folder.
func (m *Mover) Start(target string) (State, error) {
	if !m.lockIdle() {
		return m.State(), errBusy
	}
	// a move cut short is Recover's first: it acts on what it read at start
	if m.State().busy() || m.recovering() {
		m.moving.Unlock()
		return m.State(), errBusy
	}

	c := m.CheckTarget(context.Background(), target)
	if !c.OK {
		m.moving.Unlock()
		return m.State(), errors.New(c.Reason)
	}

	// the check names DefaultDir, but the move back is asked for as Default
	to := c.Path
	if target == Default {
		to = Default
	}

	m.set(func(s *State) {
		*s = State{Phase: PhaseStopping, Location: s.Location, From: s.Location, Target: to, TotalBytes: c.UsedBytes, OldCopy: s.OldCopy, StartedAt: now()}
	})

	go func() {
		defer m.moving.Unlock()
		m.move(to)
	}()

	return m.State(), nil
}

// dst is where the copy of a goes for a move to target.
func dst(a area, target string) string {
	if target == Default {
		return a.live + newSuffix
	}
	return filepath.Join(target, a.name)
}

// move does the work; every step it cannot finish is undone, and what could
// not be undone is said with where the data is.
func (m *Mover) move(target string) {
	ctx := context.Background()
	from := m.State().Location

	fail := func(err error) {
		m.set(func(s *State) {
			s.Phase, s.Message, s.FinishedAt = PhaseFailed, err.Error(), now()
			s.Location, s.From, s.Running, s.Undoing = m.location(), "", nil, false
		})
	}

	// measured now, before anything stops: the check may have used an older
	// measure; one taken by the check of this Start serves
	u := m.recentMeasure(time.Minute)
	room := m.path("/var/lib")
	if target != Default {
		room = m.path(target)
	}
	if _, free, err := m.StatFS(room); err != nil || free < need(u.blocks) {
		fail(fmt.Errorf("the drive has %d MB free, the move needs %d MB", free>>20, need(u.blocks)>>20))
		return
	}

	// what runs now, to run again after
	running := m.runningContainers(ctx)
	images := m.imageCount(ctx)
	m.set(func(s *State) { s.TotalBytes, s.Running, s.Images = u.apparent, running, images })

	// Docker back where it was, with the containers that ran: a container
	// without a restart policy does not come back with Docker alone
	restart := func() {
		m.startServices(ctx)
		m.startContainers(ctx, running)
	}

	// undo puts the switch back; when it cannot, the copy stays and the
	// message says where the data is
	undoOrSay := func(undo func() error, copied []string, err error) error {
		// recorded before anything is undone, with why: Recover finishes the undo
		m.set(func(s *State) { s.Phase, s.Undoing, s.Message = PhaseSwitching, true, err.Error() })
		if undoErr := undo(); undoErr != nil {
			restart()
			return fmt.Errorf("%w; putting it back failed too (%v), so nothing was deleted: the data Docker had is in %s, the copy in %s",
				err, undoErr, strings.Join(m.dataNow(from), ", "), strings.Join(copied, ", "))
		}
		m.removeAll(copied)
		restart()
		return fmt.Errorf("%w; everything was put back as it was", err)
	}

	// a container's file system can stay mounted once Docker stops: it would
	// be copied as it looks from above, and pin the old folder
	if err := m.quiesce(ctx); err != nil {
		restart()
		fail(err)
		return
	}

	m.set(func(s *State) { s.Phase = PhaseCopying })
	var copied []string
	for _, a := range areas {
		if _, err := os.Stat(m.path(a.live)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		// the way back moves what was moved: a /var/lib/containerd made
		// since, on the system disk, has no mount to take away
		if target == Default && !exists(m.path(unitPath(a.unit))) {
			continue
		}
		d := dst(a, target)
		copied = append(copied, d)
		if err := m.copy(ctx, a.live, d); err != nil {
			m.removeAll(copied)
			restart()
			fail(fmt.Errorf("copy %s: %w", a.live, err))
			return
		}
	}

	// rsync does not fsync: the copy reaches the disk before anything points
	// at it, or a power cut soon after could keep a switch onto files the
	// disk never got, with the intact data offered for deletion
	for _, d := range copied {
		if _, err := m.Run(ctx, "sync", "-f", m.path(d)); err != nil {
			m.removeAll(copied)
			restart()
			fail(fmt.Errorf("flush the copy %s to disk: %w", d, err))
			return
		}
	}

	m.set(func(s *State) { s.Phase = PhaseSwitching })
	switchTo := m.switchMounts
	if target == Default {
		switchTo = m.switchToDefault
	}
	undo, err := switchTo(ctx, from, target)
	if err == nil {
		err = m.checkSwitched(from, target)
	}
	if err != nil {
		fail(undoOrSay(undo, copied, fmt.Errorf("switch to %s: %w", place(target), err)))
		return
	}

	m.set(func(s *State) { s.Phase = PhaseStarting })
	if err := m.startAndVerify(ctx, running, images); err != nil {
		// an undo under way from here on: a stop before it ends, even before
		// it starts, is undone by Recover, never kept as a move that worked
		m.set(func(s *State) {
			s.Phase, s.Undoing = PhaseSwitching, true
			s.Message = fmt.Sprintf("Docker did not come back in %s: %v", place(target), err)
		})
		if qerr := m.quiesce(ctx); qerr != nil {
			restart()
			fail(fmt.Errorf("Docker did not come back in %s: %w; it could not be stopped to put things back (%v), so nothing was undone: the data is in %s, the data Docker had before in %s",
				place(target), err, qerr, place(target), strings.Join(m.dataNow(from), ", ")))
			return
		}
		fail(undoOrSay(undo, copied, fmt.Errorf("Docker did not come back in %s: %w", place(target), err)))
		return
	}

	m.finish(from, target)
}

// finish records a move that ended well.
func (m *Mover) finish(from, target string) {
	old := m.oldCopy(from)
	m.set(func(s *State) {
		s.Phase, s.Message, s.FinishedAt = PhaseDone, "", now()
		// what earlier moves left stays listed until it is deleted, but never
		// what is live again: a folder moved back to is not an old copy
		for _, p := range old {
			if !slices.Contains(s.OldCopy, p) {
				s.OldCopy = append(s.OldCopy, p)
			}
		}
		s.Location, s.Target, s.From, s.Running, s.Undoing = target, "", "", nil, false
		s.OldCopy = slices.DeleteFunc(s.OldCopy, func(p string) bool { return m.inUse(p, target) })
	})
}

func place(target string) string {
	if target == Default {
		return DefaultDir
	}
	return target
}

// dataNow says where the data Docker had before the move is, for a message.
func (m *Mover) dataNow(from string) []string {
	var out []string
	for _, a := range areas {
		switch {
		case from != "":
			out = append(out, filepath.Join(from, a.name))
		case exists(m.path(a.live + oldSuffix)):
			out = append(out, a.live+oldSuffix)
		default:
			out = append(out, a.live)
		}
	}
	return out
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// checkSwitched looks at what the live folders are once switched: the new
// copy for a move to a folder, no mount at all for the way back. A mount
// that refused to stop (a shell in /var/lib/docker, a backup reading it)
// leaves Docker on the old data, whose delete would then delete live data.
func (m *Mover) checkSwitched(from, target string) error {
	for _, a := range areas {
		if target != Default {
			src := filepath.Join(target, a.name)
			if !exists(m.path(src)) {
				continue
			}
			if !m.SameDir(a.live, src) {
				return fmt.Errorf("%s is not %s once switched: something holds the old folder", a.live, src)
			}
			continue
		}
		if mps := m.mountsUnder(a.live, true); slices.Contains(mps, a.live) {
			return fmt.Errorf("%s is still mounted", a.live)
		}
		if from != "" && m.SameDir(a.live, filepath.Join(from, a.name)) {
			return fmt.Errorf("%s is still %s", a.live, filepath.Join(from, a.name))
		}
	}
	return nil
}

// inUse says whether p is what Docker uses with its data in location: a live
// folder, the location or under it, or a folder a live one is bound to.
func (m *Mover) inUse(p, location string) bool {
	p = filepath.Clean(p)
	for _, a := range areas {
		if p == a.live || strings.HasPrefix(a.live, p+"/") || m.SameDir(a.live, p) {
			return true
		}
	}
	if location != "" && (p == location || strings.HasPrefix(p, location+"/") || strings.HasPrefix(location, p+"/")) {
		return true
	}
	return false
}

func (m *Mover) stopServices(ctx context.Context) error {
	// the socket would start Docker again at the first request
	units := []string{"docker.socket", "docker.service", "containerd.service"}
	for _, unit := range units {
		if _, err := m.Run(ctx, "systemctl", "stop", unit); err != nil && unit != "docker.socket" {
			return err
		}
	}
	// a socket that would not stop (or a box without one) answers here, one
	// unit at a time: the output mixes in stderr, so a warning line cannot
	// shift one unit's state onto another
	for _, unit := range units {
		out, _ := m.Run(ctx, "systemctl", "is-active", unit)
		for _, state := range []string{"active", "activating", "reloading"} {
			if slices.Contains(strings.Fields(out), state) {
				return fmt.Errorf("%s is still %s", unit, state)
			}
		}
	}
	return nil
}

func (m *Mover) startServices(ctx context.Context) {
	units := []string{"containerd.service", "docker.socket", "docker.service"}
	// docker.service allows 3 starts a minute: a move, its undo and a retry,
	// or a boot and Recover, can pass that, and systemd then refuses the start
	_, _ = m.Run(ctx, "systemctl", append([]string{"reset-failed"}, units...)...)
	for _, unit := range units {
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
	// -x: a file system mounted inside is not the move's, as measure counts it
	cmd := exec.CommandContext(ctx, "rsync", "-aHAXSx", "--numeric-ids", "--info=progress2", "--no-inc-recursive",
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
			m.show(func(s *State) { s.CopiedBytes = base + n })
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

// undoAll runs undos newest first, all of them, and says what failed.
func undoAll(undos []func() error) error {
	var errs []error
	for i := len(undos) - 1; i >= 0; i-- {
		if err := undos[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// switchMounts points the live folders, in their default place, at target
// and returns how to undo it: the old folders are renamed aside, and the
// mount units bind target's copies on them.
func (m *Mover) switchMounts(ctx context.Context, from, target string) (func() error, error) {
	var undos []func() error
	undo := func() error { return undoAll(undos) }

	if from != "" {
		return undo, errors.New("a move goes from the default place: move the data back first")
	}

	stuckOf := map[string]*bool{}

	for _, a := range areas {
		if !exists(m.path(filepath.Join(target, a.name))) {
			continue
		}

		unitFile := m.path(unitPath(a.unit))
		dropIn := m.path(dropInPath(a.service))
		prevUnit, _ := os.ReadFile(unitFile)
		prevDrop, _ := os.ReadFile(dropIn)

		// set when the mount would not stop: the folder and its unit stay as they are
		stuck := new(bool)
		stuckOf[a.unit] = stuck

		live, aside := m.path(a.live), m.path(a.live+oldSuffix)
		if err := renameSynced(live, aside); err != nil {
			return undo, err
		}
		undos = append(undos, func() error {
			if *stuck {
				return fmt.Errorf("%s stays mounted, so the data stays in %s", a.live, a.live+oldSuffix)
			}
			if err := os.Remove(live); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s is not empty, so %s stays as it is: %w", a.live, a.live+oldSuffix, err)
			}
			return renameSynced(aside, live)
		})
		if err := mkdirSynced(live, 0o711); err != nil {
			return undo, err
		}

		// the units go, and systemd forgets them, before the folder comes back
		undos = append(undos, func() error {
			if *stuck {
				return nil
			}
			err := errors.Join(restore(unitFile, prevUnit), restore(dropIn, prevDrop))
			if _, rerr := m.Run(ctx, "systemctl", "daemon-reload"); rerr != nil {
				err = errors.Join(err, rerr)
			}
			return err
		})
		if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
			return undo, err
		}
		if err := writeSynced(unitFile, []byte(MountUnit(filepath.Join(target, a.name), a.live, target, a.service)), 0o644); err != nil {
			return undo, err
		}
		if err := writeSynced(dropIn, []byte(DropIn(a.live)), 0o644); err != nil {
			return undo, err
		}
	}

	if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return undo, err
	}

	for _, a := range areas {
		if !exists(m.path(filepath.Join(target, a.name))) {
			continue
		}
		stuck := stuckOf[a.unit]
		if _, err := m.Run(ctx, "systemctl", "enable", "--now", a.unit); err != nil {
			// it may have mounted before failing
			undos = append(undos, m.unmountUndo(ctx, a, stuck))
			return undo, err
		}
		undos = append(undos, m.unmountUndo(ctx, a, stuck))
	}

	return undo, nil
}

// unmountUndo stops and disables a mount unit; a stop that fails (the
// folder is busy) is an error, and marks the area stuck: the folder cannot
// come back over the mount, nor the unit go from under it.
func (m *Mover) unmountUndo(ctx context.Context, a area, stuck *bool) func() error {
	return func() error {
		if _, err := m.Run(ctx, "systemctl", "stop", a.unit); err != nil {
			*stuck = true
			return fmt.Errorf("unmount %s: %w", a.live, err)
		}
		_, _ = m.Run(ctx, "systemctl", "disable", a.unit)
		return nil
	}
}

// switchToDefault takes the mounts away and puts the copy made next to the
// live folders in their place; from's folders become the old copy.
func (m *Mover) switchToDefault(ctx context.Context, from, _ string) (func() error, error) {
	var undos []func() error
	undo := func() error { return undoAll(undos) }

	for _, a := range areas {
		if !exists(m.path(a.live + newSuffix)) {
			continue
		}

		unitFile := m.path(unitPath(a.unit))
		dropIn := m.path(dropInPath(a.service))
		prevUnit, _ := os.ReadFile(unitFile)
		prevDrop, _ := os.ReadFile(dropIn)

		// a mount that does not stop (the folder is busy) stops the switch:
		// the copy would go under a folder Docker never sees
		if _, err := m.Run(ctx, "systemctl", "stop", a.unit); err != nil {
			return undo, fmt.Errorf("unmount %s: %w", a.live, err)
		}
		undos = append(undos, func() error {
			_, err := m.Run(ctx, "systemctl", "start", a.unit)
			return err
		})

		_, _ = m.Run(ctx, "systemctl", "disable", a.unit)
		undos = append(undos, func() error {
			_, err := m.Run(ctx, "systemctl", "enable", a.unit)
			return err
		})

		_ = restore(unitFile, nil)
		_ = restore(dropIn, nil)
		// restored, and reloaded, before the enable and the start above run again
		undos = append(undos, func() error {
			err := errors.Join(restore(unitFile, prevUnit), restore(dropIn, prevDrop))
			if _, rerr := m.Run(ctx, "systemctl", "daemon-reload"); rerr != nil {
				err = errors.Join(err, rerr)
			}
			return err
		})

		// the live folder is the empty mount point now
		live, fresh := m.path(a.live), m.path(a.live+newSuffix)
		if err := os.Remove(live); err != nil {
			return undo, fmt.Errorf("%s is not empty once unmounted: %w", a.live, err)
		}
		if err := renameSynced(fresh, live); err != nil {
			_ = mkdirSynced(live, 0o711)
			return undo, err
		}
		undos = append(undos, func() error {
			if err := renameSynced(live, fresh); err != nil {
				return err
			}
			return mkdirSynced(live, 0o711)
		})
	}

	if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return undo, err
	}
	return undo, nil
}

// restore puts back a file as it was; nil content: it did not exist.
func restore(path string, content []byte) error {
	if content == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		syncDir(filepath.Dir(path))
		return nil
	}
	return writeSynced(path, content, 0o644)
}

// The units, drop-ins and folders a switch changes are on disk before the
// next step: a power cut seconds after must not boot on an empty unit file
// or a rename the file system had not written yet.

// writeSynced writes a file and syncs it and its folder.
func writeSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	if err := errors.Join(werr, serr, f.Close()); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// renameSynced renames and syncs the folders on both sides.
func renameSynced(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	syncDir(filepath.Dir(to))
	if filepath.Dir(from) != filepath.Dir(to) {
		syncDir(filepath.Dir(from))
	}
	return nil
}

// mkdirSynced makes a folder and syncs the one holding it.
func mkdirSynced(path string, perm os.FileMode) error {
	if err := os.Mkdir(path, perm); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir flushes a folder's entries; best effort.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}

func (m *Mover) startAndVerify(ctx context.Context, running []string, images int) error {
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
		if exists(m.path(p)) {
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

// mountEntry is a line of the mount table.
type mountEntry struct{ point, fstype string }

// mounts reads /proc/self/mountinfo.
func (m *Mover) mounts() []mountEntry {
	b, err := os.ReadFile(m.path("/proc/self/mountinfo"))
	if err != nil {
		return nil
	}

	var out []mountEntry
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := sc.Text()
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		e := mountEntry{point: strings.ReplaceAll(f[4], `\040`, " ")}
		// the file system type follows the " - " separator
		if _, rest, ok := strings.Cut(line, " - "); ok {
			if t := strings.Fields(rest); len(t) > 0 {
				e.fstype = t[0]
			}
		}
		out = append(out, e)
	}
	return out
}

// mountsUnder lists the mount points under dir, dir itself when self is
// set, deepest first.
func (m *Mover) mountsUnder(dir string, self bool) []string {
	var out []string
	for _, e := range m.mounts() {
		if (self && e.point == dir) || strings.HasPrefix(e.point, dir+"/") {
			out = append(out, e.point)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// foreignMountsUnder lists what is mounted under dir that Docker did not
// mount: a drive of somebody's.
func (m *Mover) foreignMountsUnder(dir string) []string {
	var out []string
	for _, e := range m.mounts() {
		if strings.HasPrefix(e.point, dir+"/") && !slices.Contains(dockerMounts, e.fstype) {
			out = append(out, e.point)
		}
	}
	return out
}

// unmountUnder unmounts what Docker and containerd mounted under dir (dir
// itself when self is set), deepest first. Anything else stays mounted: it
// is not the move's, and a delete that would cross into it refuses.
func (m *Mover) unmountUnder(ctx context.Context, dir string, self bool) error {
	var mine []string
	for _, e := range m.mounts() {
		if ((self && e.point == dir) || strings.HasPrefix(e.point, dir+"/")) && slices.Contains(dockerMounts, e.fstype) {
			mine = append(mine, e.point)
		}
	}
	slices.SortFunc(mine, func(a, b string) int { return len(b) - len(a) })

	for _, mp := range mine {
		if _, err := m.Run(ctx, "umount", mp); err != nil {
			return err
		}
	}
	var left []string
	for _, e := range m.mounts() {
		if slices.Contains(mine, e.point) {
			left = append(left, e.point)
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("still mounted: %s", strings.Join(left, ", "))
	}
	return nil
}

// removeTree deletes a folder a move made or left, first unmounting what
// Docker left mounted in it, and never crossing into another file system:
// what is mounted there is not the move's to delete.
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

// DeleteOldCopy deletes what the last move left behind. Each folder is
// looked at first: never one Docker uses, whatever the list says.
func (m *Mover) DeleteOldCopy() (State, error) {
	if !m.lockIdle() {
		return m.State(), errBusy
	}
	defer m.moving.Unlock()

	s := m.State()
	if s.busy() || m.recovering() {
		return s, errBusy
	}
	if len(s.OldCopy) == 0 {
		return s, errors.New("there is no old copy")
	}
	for _, p := range s.OldCopy {
		if m.inUse(p, s.Location) {
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

// Recover deals with a move a stop of this service cut short (an update, a
// crash, the machine turned off), at the next start:
//   - stopped or copying: nothing was switched; the partial copy goes and
//     Docker starts again with the containers that ran;
//   - switching: the switch is put back to where the data was, which the move
//     never touched;
//   - starting: the switch was done; Docker and the containers are started.
//
// Docker is never left stopped by an interrupted move.
func (m *Mover) Recover(ctx context.Context) {
	m.moving.Lock()
	defer m.moving.Unlock()

	m.mu.Lock()
	cut := m.interrupted
	m.interrupted = State{}
	m.mu.Unlock()
	if !cut.busy() {
		return
	}

	from, target := cut.From, cut.Target
	var why string
	var err error

	// a switch that had ended is kept: at boot Docker has run from the new
	// place already, and what the apps wrote since is there. Never one being
	// undone: it looks complete until the undo has run
	kept := cut.Phase == PhaseStarting
	if cut.Phase == PhaseSwitching && !cut.Undoing && m.switchDone(from, target) {
		kept = true
		err = m.enableUnits(ctx, target)
		why = "just after the switch to the new place, which was kept; Docker was started there"
	} else if cut.Phase == PhaseStarting {
		why = "while Docker was starting from the new place; it was started there"
	}

	if !kept {
		// at boot Docker starts before this service, from whatever the
		// folders are now: it stops, socket first, and lets go of its mounts
		// before any folder under it changes; if it cannot, nothing does
		var aside []string
		if qerr := m.quiesce(ctx); qerr != nil {
			err = qerr
			why = "and Docker could not be stopped to put it right, so nothing was changed"
		} else if aside, err = m.setAsideBootFolders(from); err != nil {
			why = "and a folder Docker started on at boot could not be set aside, so nothing else was changed"
		} else if cut.Phase == PhaseSwitching {
			if from == "" {
				err = m.backToDefault(ctx, target)
			} else {
				err = m.backToFolder(ctx, from)
			}
			if err == nil {
				err = m.dropCopies(ctx, from, target)
			}
			why = "while switching to the new place; it was put back where the data was"
			if cut.Undoing && cut.Message != "" {
				why += " (" + cut.Message + ")"
			}
		} else { // stopping, copying
			err = m.dropCopies(ctx, from, target)
			why = "while Docker was stopped for the copy; the partial copy was deleted and nothing else changed"
		}
		if len(aside) > 0 {
			why += fmt.Sprintf(" (Docker had started at boot on an empty folder; what it wrote there is kept in %s)", strings.Join(aside, ", "))
		}
	}

	// a kept switch was never checked: Docker must find the images it had
	var unchecked error
	if kept {
		unchecked = m.startAndVerify(ctx, cut.Running, cut.Images)
	} else {
		m.startServices(ctx)
		m.startContainers(ctx, cut.Running)
	}

	msg := "the move was interrupted " + why + ": check that the apps run"
	if err != nil {
		msg = fmt.Sprintf("the move was interrupted %s, and putting it right failed (%v): Docker was started again; check where its data is before anything else", why, err)
	}
	if kept {
		old := m.oldCopy(from)
		m.finish(from, m.location())
		if unchecked != nil {
			// the data from before may be the only good copy: it stays on
			// disk, and is not offered for deletion
			m.set(func(s *State) {
				s.OldCopy = slices.DeleteFunc(s.OldCopy, func(p string) bool { return slices.Contains(old, p) })
			})
			msg += fmt.Sprintf("; Docker could not be checked in its new place (%v): the data it had before is kept in %s", unchecked, strings.Join(old, ", "))
		}
	}
	m.set(func(s *State) {
		if !kept {
			s.Phase = PhaseFailed
		}
		s.Message, s.FinishedAt = msg, now()
		s.Location, s.Target, s.From, s.Running, s.Undoing = m.location(), "", "", nil, false
	})
}

// quiesce stops Docker and containerd, the socket first so that nothing
// wakes them, and unmounts what they left mounted in their folders.
func (m *Mover) quiesce(ctx context.Context) error {
	if err := m.stopServices(ctx); err != nil {
		return fmt.Errorf("stop Docker: %w", err)
	}
	for _, a := range areas {
		if err := m.unmountUnder(ctx, a.live, false); err != nil {
			return fmt.Errorf("unmount what Docker left mounted: %w", err)
		}
	}
	return nil
}

// switchDone says whether an interrupted switch had ended: every area is in
// the new place, its unit written (to a folder) or gone (back), and the live
// folders are what they should be.
func (m *Mover) switchDone(from, target string) bool {
	for _, a := range areas {
		unit, unitErr := os.ReadFile(m.path(unitPath(a.unit)))
		if target == Default {
			if exists(m.path(a.live+newSuffix)) || unitErr == nil {
				return false
			}
			// the copy in place, not an empty folder Docker started on
			if a.name == "docker" && emptyOrMissing(m.path(a.live)) {
				return false
			}
			continue
		}
		src := filepath.Join(target, a.name)
		if !exists(m.path(src)) {
			// a disk not there at boot shows no copy: never a switch that ended
			if a.name == "docker" || exists(m.path(a.live+oldSuffix)) {
				return false
			}
			continue
		}
		if unitErr != nil || unitValue(string(unit), "What") != src || !exists(m.path(dropInPath(a.service))) {
			return false
		}
	}
	return m.checkSwitched(from, target) == nil
}

// setAsideBootFolders moves aside a live folder that is no mount, holds
// something, and whose data waits next to it: .casaos-old on a move from the
// default place, .casaos-new on the way back. The folder was empty at a stop
// mid-switch, Docker started on it at boot, and putting the switch back would
// stop on it. It is kept, as <live>.casaos-boot, and its name returned for
// the message.
func (m *Mover) setAsideBootFolders(from string) ([]string, error) {
	var aside []string
	for _, a := range areas {
		waiting := a.live + oldSuffix
		if from != "" {
			waiting = a.live + newSuffix
		}
		if !exists(m.path(waiting)) {
			continue
		}
		if emptyOrMissing(m.path(a.live)) || slices.Contains(m.mountsUnder(a.live, true), a.live) {
			continue
		}
		to := a.live + bootSuffix
		for i := 2; exists(m.path(to)); i++ {
			to = fmt.Sprintf("%s%s-%d", a.live, bootSuffix, i)
		}
		if err := os.Rename(m.path(a.live), m.path(to)); err != nil {
			return aside, err
		}
		if err := os.Mkdir(m.path(a.live), 0o711); err != nil {
			return aside, err
		}
		syncDir(m.path(filepath.Dir(a.live)))
		aside = append(aside, to)
	}
	return aside, nil
}

// enableUnits makes sure the mount units of a switch to target are enabled
// and mounted; there are none for the way back.
func (m *Mover) enableUnits(ctx context.Context, target string) error {
	if target == Default {
		return nil
	}
	var errs []error
	for _, a := range areas {
		if !exists(m.path(filepath.Join(target, a.name))) {
			continue
		}
		if _, err := m.Run(ctx, "systemctl", "enable", "--now", a.unit); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// dropCopies deletes the copies an interrupted move made, never a folder
// Docker uses.
func (m *Mover) dropCopies(ctx context.Context, from, target string) error {
	var errs []error
	for _, a := range areas {
		d := dst(a, target)
		if !exists(m.path(d)) || m.inUse(d, m.location()) {
			continue
		}
		if err := m.removeTree(ctx, d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// backToDefault puts back what a switch from the default place did: the
// units go, and the folders set aside come back.
func (m *Mover) backToDefault(ctx context.Context, target string) error {
	var errs []error
	for _, a := range areas {
		// the unit and drop-in of every area go, switched or not yet
		if exists(m.path(unitPath(a.unit))) {
			if _, err := m.Run(ctx, "systemctl", "stop", a.unit); err != nil {
				errs = append(errs, fmt.Errorf("unmount %s: %w", a.live, err))
				continue
			}
			_, _ = m.Run(ctx, "systemctl", "disable", a.unit)
		}
		errs = append(errs, restore(m.path(unitPath(a.unit)), nil), restore(m.path(dropInPath(a.service)), nil))
		if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			errs = append(errs, err)
		}
		if !exists(m.path(a.live + oldSuffix)) {
			continue
		}
		if err := os.Remove(m.path(a.live)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%s is not empty, so %s stays as it is: %w", a.live, a.live+oldSuffix, err))
			continue
		}
		if err := renameSynced(m.path(a.live+oldSuffix), m.path(a.live)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// backToFolder puts back a way back cut short: every live folder is bound
// again to its copy in from, which the way back never changes.
func (m *Mover) backToFolder(ctx context.Context, from string) error {
	var errs []error
	for _, a := range areas {
		src := filepath.Join(from, a.name)
		if !exists(m.path(src)) {
			continue
		}
		live := m.path(a.live)
		// a live folder that was switched holds the new copy: it goes aside, to be deleted
		if !emptyOrMissing(live) && !m.SameDir(a.live, src) {
			if err := renameSynced(live, m.path(a.live+newSuffix)); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := os.MkdirAll(live, 0o711); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(m.path(dropInPath(a.service))), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs,
			writeSynced(m.path(unitPath(a.unit)), []byte(MountUnit(src, a.live, from, a.service)), 0o644),
			writeSynced(m.path(dropInPath(a.service)), []byte(DropIn(a.live)), 0o644))
	}
	if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		errs = append(errs, err)
	}
	for _, a := range areas {
		if !exists(m.path(filepath.Join(from, a.name))) {
			continue
		}
		if _, err := m.Run(ctx, "systemctl", "enable", "--now", a.unit); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return m.checkSwitched("", from)
}
