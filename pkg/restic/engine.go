// Package restic is the other way to keep a backup: in a restic repository on a
// destination, instead of one stamped copy per run.
//
// A destination that has been switched to restic keeps the same names as the rest
// of the feature -- an app, a stamp, an operation's path under the run -- and this
// package answers the same seven calls the rclone client answers (copy a folder,
// copy a file, restore a folder, restore a file, read a file, list, purge). That is
// the whole integration: the planning, the hold-still, the manifest, the retention
// and the restore stay what they were, and only where the bytes go changes.
//
// What the names map to. Every copy is one restic snapshot, tagged with the app,
// the stamp and the path it was asked to land at; a run is the snapshots that share
// a stamp, and the manifest, written last, is the run's last snapshot. Deleting a
// run forgets those snapshots and prunes. What restic adds over a stamped copy is
// that a run costs what changed since the last one, not another full copy, and that
// ownership and permissions survive the trip.
//
// restic reaches the destination through the rclone remote it already is
// (`rclone:casaos-backup-<name>:...`), so credentials, S3, SFTP and the rest are
// not configured a second time.
package restic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
)

// KeyDir is where the repositories' passwords are kept, one file per destination.
// The box's own backup does not hold them: it may itself be kept in the repository
// a key opens, and a key that lies inside what it opens opens nothing on a new box.
//
// A destination switched back to rclone keeps its key and gains a marker next to
// it, `<name>.key.off`: the repository is still read, only no longer written to.
var KeyDir = "/var/lib/casaos/backups/restic"

// CacheDir is where restic keeps what it learns about a repository between runs.
// Given explicitly because the service has neither $HOME nor $XDG_CACHE_HOME,
// and without a place for it restic refuses to back anything up.
var CacheDir = "/var/cache/casaos/restic"

// RepositoryFolder is the folder a destination keeps its repository in. Next to
// the stamped copies, not in place of them: a destination switched to restic keeps
// whatever it held.
const RepositoryFolder = "casaos-restic"

const (
	tagOwner = "casaos"

	// restic's own exit codes, which tell "no repository there" from "wrong password"
	exitNoRepository = 10
	exitWrongKey     = 12
)

var (
	// ErrNotInstalled is the restic binary missing from this box.
	ErrNotInstalled = errors.New("restic is not installed on this box")
	// ErrAlreadyEnabled is a destination that is a restic repository already.
	ErrAlreadyEnabled = errors.New("this destination already keeps its backups with restic")
	// ErrKeyKept is a password given for a destination whose key this box still has.
	ErrKeyKept = errors.New("this box still has the key of this destination's repository; take it up again without a password")
	// ErrNotKept is a destination with no restic repository to go back from.
	ErrNotKept = errors.New("this destination does not keep its backups with restic")
	// ErrWrongPassword is a repository that is there and does not open with the
	// password it was given.
	ErrWrongPassword = errors.New("a restic repository is already there and this password does not open it")
	// ErrNothingHeld is a restore of something the repository does not hold.
	ErrNothingHeld = errors.New("the repository holds nothing at that path")

	errBadName = errors.New("not a destination name restic can keep a password for")
)

// runner runs the restic binary with these extra environment variables and returns
// what it printed on stdout. A seam for the tests.
type runner func(ctx context.Context, env []string, args ...string) ([]byte, error)

// ExitError is restic failing: its exit code, and the last of what it said.
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("restic exited with %d: %s", e.Code, e.Message)
}

func execRunner(ctx context.Context, env []string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "restic", args...)
	command.Env = append(os.Environ(), env...)

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			message := strings.TrimSpace(stderr.String())
			// the last lines are the ones that say why
			if lines := strings.Split(message, "\n"); len(lines) > 6 {
				message = strings.Join(lines[len(lines)-6:], "\n")
			}

			return nil, &ExitError{Code: exit.ExitCode(), Message: message}
		}

		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrNotInstalled
		}

		return nil, err
	}

	return stdout.Bytes(), nil
}

// Engine keeps backups in restic repositories.
type Engine struct {
	run        runner
	repository func(destination string) string
}

// New returns the engine that drives the restic binary on this box.
func New() *Engine {
	return &Engine{run: execRunner, repository: func(destination string) string {
		return "rclone:" + rclone.DestinationPrefix + destination + ":" + RepositoryFolder
	}}
}

func keyPath(destination string) (string, error) {
	if destination == "" || destination == "." || destination == ".." || strings.ContainsAny(destination, `/\`) {
		return "", errBadName
	}

	return filepath.Join(KeyDir, destination+".key"), nil
}

// offPath is the marker of a destination switched back to rclone.
func offPath(destination string) (string, error) {
	key, err := keyPath(destination)
	if err != nil {
		return "", err
	}

	return key + ".off", nil
}

// Held says whether this box has the key of a restic repository at a destination:
// what the repository holds can be listed and restored, whichever engine the next
// backups go to.
func (e *Engine) Held(destination string) bool {
	key, err := keyPath(destination)
	if err != nil {
		return false
	}

	_, err = os.Stat(key)

	return err == nil
}

// Enabled says whether the backups to come at a destination go into restic.
func (e *Engine) Enabled(destination string) bool {
	if !e.Held(destination) {
		return false
	}

	off, err := offPath(destination)
	if err != nil {
		return false
	}

	_, err = os.Stat(off)

	return errors.Is(err, os.ErrNotExist)
}

// Disable sends the next backups of a destination back to rclone. Nothing is
// deleted: the key stays, the repository stays, and Enable takes it up again.
func (e *Engine) Disable(destination string) error {
	off, err := offPath(destination)
	if err != nil {
		return err
	}
	if !e.Held(destination) {
		return ErrNotKept
	}

	return os.WriteFile(off, nil, 0o600)
}

// Available says whether the restic binary is on this box.
func Available() bool {
	_, err := exec.LookPath("restic")

	return err == nil
}

func (e *Engine) call(ctx context.Context, destination string, args ...string) ([]byte, error) {
	key, err := keyPath(destination)
	if err != nil {
		return nil, err
	}

	return e.run(ctx, []string{
		"RESTIC_REPOSITORY=" + e.repository(destination),
		"RESTIC_PASSWORD_FILE=" + key,
		"RESTIC_CACHE_DIR=" + CacheDir,
	}, append([]string{"--retry-lock", "5m", "--quiet"}, args...)...)
}

// Enable makes a destination a restic repository and returns the password that
// opens it. An empty password makes a new one; a given one is for a repository
// that is already there -- a destination added again on another box, which is
// how a backup is restored somewhere else.
//
// The password is returned here and kept nowhere a person can read it again
// except the box's own files: a repository whose password is lost is a pile of
// bytes, so the one time it is shown is the time to write it down.
func (e *Engine) Enable(ctx context.Context, destination, password string) (string, error) {
	key, err := keyPath(destination)
	if err != nil {
		return "", err
	}
	if e.Enabled(destination) {
		return "", ErrAlreadyEnabled
	}
	if e.Held(destination) {
		return "", e.resume(ctx, destination, password)
	}

	// a marker left without its key must not keep a new repository switched off
	if off, err := offPath(destination); err == nil {
		_ = os.Remove(off)
	}

	generated := password == ""
	if generated {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		password = base64.RawURLEncoding.EncodeToString(raw)
	}

	if err := os.MkdirAll(KeyDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(key, []byte(password), 0o600); err != nil {
		return "", err
	}

	if err := e.open(ctx, destination); err != nil {
		_ = os.Remove(key)

		return "", err
	}

	return password, nil
}

// resume takes a destination switched back to rclone up again, with the key this
// box kept. The repository must still open: a new one is never made here, which
// would hide a repository that went missing under an empty one.
func (e *Engine) resume(ctx context.Context, destination, password string) error {
	if password != "" {
		return ErrKeyKept
	}

	_, err := e.call(ctx, destination, "cat", "config")

	var exit *ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.Code == exitWrongKey:
		return ErrWrongPassword
	default:
		return err
	}

	off, err := offPath(destination)
	if err != nil {
		return err
	}

	return os.Remove(off)
}

// open makes sure there is a repository to use: the one that is there, or a new
// one.
func (e *Engine) open(ctx context.Context, destination string) error {
	_, err := e.call(ctx, destination, "cat", "config")

	var exit *ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.Code == exitWrongKey:
		return ErrWrongPassword
	case errors.As(err, &exit) && exit.Code == exitNoRepository:
		_, err = e.call(ctx, destination, "init")

		return err
	default:
		return err
	}
}

// Check asks restic whether the repository is sound: its index and snapshots read,
// and every pack file the index names is there with the size it should have.
//
// shortcut: it does not read the data inside the packs, which downloads the whole
// repository (and at a cloud provider, bills it); a bit that rots inside a pack that
// is the right size passes. Add `--read-data-subset` if that is ever felt.
func (e *Engine) Check(ctx context.Context, destination string) error {
	_, err := e.call(ctx, destination, "check")

	return err
}

// snapshot is the part of `restic snapshots --json` this package reads.
type snapshot struct {
	ID    string    `json:"id"`
	Time  time.Time `json:"time"`
	Paths []string  `json:"paths"`
	Tags  []string  `json:"tags"`
}

func (s snapshot) tag(name string) string {
	for _, tag := range s.Tags {
		if value, ok := strings.CutPrefix(tag, name+"="); ok {
			return value
		}
	}

	return ""
}

func (e *Engine) snapshots(ctx context.Context, destination string) ([]snapshot, error) {
	out, err := e.call(ctx, destination, "snapshots", "--json", "--tag", tagOwner)
	if err != nil {
		return nil, err
	}

	var all []snapshot
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("restic listed its snapshots in a form this box does not read: %w", err)
	}

	return all, nil
}

// find is the newest snapshot that was taken for this path under a run.
func (e *Engine) find(ctx context.Context, destination, remotePath string) (snapshot, error) {
	all, err := e.snapshots(ctx, destination)
	if err != nil {
		return snapshot{}, err
	}

	key := escape(remotePath)
	var found *snapshot
	for i := range all {
		if all[i].tag("key") != key || len(all[i].Paths) == 0 {
			continue
		}
		if found == nil || all[i].Time.After(found.Time) {
			found = &all[i]
		}
	}
	if found == nil {
		return snapshot{}, fmt.Errorf("%w: %s", ErrNothingHeld, remotePath)
	}

	return *found, nil
}

// escape keeps a comma out of a tag, where restic would read it as two.
func escape(value string) string {
	return strings.ReplaceAll(value, ",", "%2C")
}

// runOf splits a path under a run into its app and its stamp.
func runOf(remotePath string) (app, stamp string, err error) {
	parts := strings.SplitN(path.Clean(remotePath), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%q is not a path under an app's run", remotePath)
	}

	return parts[0], parts[1], nil
}

func (e *Engine) backup(ctx context.Context, source, destination, remotePath string) error {
	app, stamp, err := runOf(remotePath)
	if err != nil {
		return err
	}

	_, err = e.call(ctx, destination, "backup",
		"--tag", tagOwner, "--tag", "app="+escape(app), "--tag", "stamp="+escape(stamp), "--tag", "key="+escape(remotePath),
		source)

	return err
}

// CopyDirectory backs a folder up as one snapshot.
func (e *Engine) CopyDirectory(ctx context.Context, source, destinationName, destinationPath string) error {
	return e.backup(ctx, source, destinationName, destinationPath)
}

// CopyFile backs one file up as one snapshot.
func (e *Engine) CopyFile(ctx context.Context, source, destinationName, destinationPath string) error {
	return e.backup(ctx, source, destinationName, destinationPath)
}

// RestoreDirectory makes hostPath hold what the snapshot held, and nothing else.
//
// The folder the snapshot was taken of is read from the snapshot itself, never
// from a manifest: restic keeps the full path it saw, and `snapshot:folder` names
// that folder as the root to restore from.
func (e *Engine) RestoreDirectory(ctx context.Context, destinationName, remotePath, hostPath string) error {
	snap, err := e.find(ctx, destinationName, remotePath)
	if err != nil {
		return err
	}

	_, err = e.call(ctx, destinationName, "restore", snap.ID+":"+snap.Paths[0], "--target", hostPath, "--delete")

	return err
}

// RestoreFile puts one file from the repository at hostPath.
func (e *Engine) RestoreFile(ctx context.Context, destinationName, remotePath, hostPath string) error {
	content, err := e.Fetch(ctx, destinationName, remotePath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return err
	}

	// 0600 for a file that does not exist yet: it may be a .env, or rclone's own
	// config, and one that exists keeps the mode it has
	return os.WriteFile(hostPath, content, 0o600)
}

// Fetch reads one file held in the repository.
func (e *Engine) Fetch(ctx context.Context, destinationName, remotePath string) ([]byte, error) {
	snap, err := e.find(ctx, destinationName, remotePath)
	if err != nil {
		return nil, err
	}

	return e.call(ctx, destinationName, "dump", snap.ID, snap.Paths[0])
}

// Runs lists the apps a repository holds backups of, or an app's runs by stamp.
func (e *Engine) Runs(destinationName, appPath string) ([]string, error) {
	// the interface this answers has no context to pass
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	all, err := e.snapshots(ctx, destinationName)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for _, snap := range all {
		app, stamp := snap.tag("app"), snap.tag("stamp")
		if app == "" || stamp == "" {
			continue
		}

		if appPath == "" {
			seen[app] = true
		} else if app == escape(appPath) {
			seen[stamp] = true
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, strings.ReplaceAll(name, "%2C", ","))
	}
	sort.Strings(names)

	return names, nil
}

// Purge forgets one run -- every snapshot that shares its app and stamp -- and
// frees what nothing else uses.
func (e *Engine) Purge(ctx context.Context, destinationName, remotePath string) error {
	_, err := e.forget(ctx, destinationName, remotePath)

	return err
}

// forget is Purge that says whether the repository held the run at all, which is
// how the router knows the run is one of the stamped copies instead.
func (e *Engine) forget(ctx context.Context, destinationName, remotePath string) (bool, error) {
	app, stamp, err := runOf(remotePath)
	if err != nil {
		return false, err
	}

	all, err := e.snapshots(ctx, destinationName)
	if err != nil {
		return false, err
	}

	ids := []string{}
	for _, snap := range all {
		if snap.tag("app") == escape(app) && snap.tag("stamp") == escape(stamp) {
			ids = append(ids, snap.ID)
		}
	}
	if len(ids) == 0 {
		return false, nil
	}

	_, err = e.call(ctx, destinationName, append([]string{"forget", "--prune"}, ids...)...)

	return err == nil, err
}
