package restic

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type call struct {
	env  []string
	args []string
}

// fake answers each restic command with the first reply whose first word matches.
type fake struct {
	calls   []call
	replies map[string]func() ([]byte, error)
}

func (f *fake) run(_ context.Context, env []string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{env: env, args: args})

	// the global flags come first: --retry-lock 5m --quiet
	if reply, ok := f.replies[args[3]]; ok {
		return reply()
	}

	return nil, nil
}

func (f *fake) last() []string { return f.calls[len(f.calls)-1].args[3:] }

func engineWith(t *testing.T, f *fake) *Engine {
	t.Helper()
	KeyDir = t.TempDir()

	return &Engine{run: f.run, repository: func(d string) string { return "repo-of-" + d }}
}

func reply(out string, err error) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(out), err }
}

const held = `[
 {"id":"aaa","time":"2026-10-01T10:00:00Z","paths":["/vol/data"],"tags":["casaos","app=blog","stamp=20261001","key=blog/20261001/volume-data"]},
 {"id":"bbb","time":"2026-10-01T10:01:00Z","paths":["/tmp/m1/manifest.json"],"tags":["casaos","app=blog","stamp=20261001","key=blog/20261001/manifest.json"]},
 {"id":"ccc","time":"2026-10-02T10:00:00Z","paths":["/vol/data"],"tags":["casaos","app=blog","stamp=20261002","key=blog/20261002/volume-data"]},
 {"id":"ddd","time":"2026-10-02T10:01:00Z","paths":["/tmp/m2/manifest.json"],"tags":["casaos","app=blog","stamp=20261002","key=blog/20261002/manifest.json"]},
 {"id":"eee","time":"2026-10-02T11:00:00Z","paths":["/vol/other"],"tags":["casaos","app=wiki","stamp=20261002","key=wiki/20261002/volume-other"]}
]`

func TestEnableMakesARepositoryAndHandsBackThePassword(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"cat": reply("", &ExitError{Code: exitNoRepository})}}
	e := engineWith(t, f)

	password, err := e.Enable(context.Background(), "nas", "")
	if err != nil || len(password) < 40 {
		t.Fatalf("password %q, err %v", password, err)
	}

	stored, _ := os.ReadFile(filepath.Join(KeyDir, "nas.key"))
	if string(stored) != password {
		t.Fatalf("the key file holds %q, the caller was given %q", stored, password)
	}
	if info, _ := os.Stat(filepath.Join(KeyDir, "nas.key")); info.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Fatalf("key file mode %v", info.Mode().Perm())
	}
	if len(f.calls) != 2 || f.calls[1].args[3] != "init" {
		t.Fatalf("expected cat config then init, got %v", f.calls)
	}
	if !contains(f.calls[0].env, "RESTIC_CACHE_DIR="+CacheDir) {
		t.Fatalf("restic was not told where its cache goes: %v", f.calls[0].env)
	}
	if !contains(f.calls[0].env, "RESTIC_REPOSITORY=repo-of-nas") {
		t.Fatalf("the repository was not passed: %v", f.calls[0].env)
	}
	if !e.Enabled("nas") {
		t.Fatal("the destination is not enabled after Enable")
	}
	if _, err := e.Enable(context.Background(), "nas", ""); !errors.Is(err, ErrAlreadyEnabled) {
		t.Fatalf("a second Enable: %v", err)
	}
}

// The password can be read again while the box keeps the key, and only then.
func TestThePasswordCanBeReadBackWhileTheKeyIsKept(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"cat": reply("", &ExitError{Code: exitNoRepository})}}
	e := engineWith(t, f)

	if _, err := e.Password("nas"); !errors.Is(err, ErrNotKept) {
		t.Fatalf("no key yet: %v", err)
	}

	given, err := e.Enable(context.Background(), "nas", "")
	if err != nil {
		t.Fatal(err)
	}
	if read, err := e.Password("nas"); err != nil || read != given {
		t.Fatalf("read %q (%v), was given %q", read, err, given)
	}
	if _, err := e.Password("../nas"); !errors.Is(err, errBadName) {
		t.Fatalf("a path for a name: %v", err)
	}
}

func TestEnableOpensARepositoryThatIsAlreadyThere(t *testing.T) {
	f := &fake{}
	e := engineWith(t, f)

	password, err := e.Enable(context.Background(), "nas", "from-the-old-box")
	if err != nil || password != "from-the-old-box" {
		t.Fatalf("password %q, err %v", password, err)
	}
	for _, c := range f.calls {
		if c.args[3] == "init" {
			t.Fatal("an existing repository was initialised over")
		}
	}
}

func TestEnableWithAWrongPasswordLeavesNothingBehind(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"cat": reply("", &ExitError{Code: exitWrongKey})}}
	e := engineWith(t, f)

	if _, err := e.Enable(context.Background(), "nas", "nope"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("err %v", err)
	}
	if e.Enabled("nas") {
		t.Fatal("the key file of a repository that did not open was kept")
	}
}

func TestEnableRefusesANameThatIsAPath(t *testing.T) {
	e := engineWith(t, &fake{})
	for _, name := range []string{"", "..", "a/b", `a\b`} {
		if _, err := e.Enable(context.Background(), name, ""); err == nil {
			t.Fatalf("%q was accepted", name)
		}
		if e.Enabled(name) {
			t.Fatalf("%q reads as enabled", name)
		}
	}
}

func TestACopyIsOneTaggedSnapshot(t *testing.T) {
	f := &fake{}
	e := engineWith(t, f)

	if err := e.CopyDirectory(context.Background(), "/vol/data", "nas", "blog/20261001/volume,data"); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(f.last(), " ")
	want := "backup --tag casaos --tag app=blog --tag stamp=20261001 --tag key=blog/20261001/volume%2Cdata /vol/data"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestACopyOutsideARunIsRefused(t *testing.T) {
	e := engineWith(t, &fake{})
	if err := e.CopyFile(context.Background(), "/etc/x", "nas", "manifest.json"); err == nil {
		t.Fatal("a path with no app and stamp was accepted")
	}
}

func TestRunsAreTheAppsThenTheStamps(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"snapshots": reply(held, nil)}}
	e := engineWith(t, f)

	apps, _ := e.Runs("nas", "")
	if strings.Join(apps, ",") != "blog,wiki" {
		t.Fatalf("apps %v", apps)
	}
	stamps, _ := e.Runs("nas", "blog")
	if strings.Join(stamps, ",") != "20261001,20261002" {
		t.Fatalf("stamps %v", stamps)
	}
}

func TestPurgeForgetsExactlyOneRun(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"snapshots": reply(held, nil)}}
	e := engineWith(t, f)

	if err := e.Purge(context.Background(), "nas", "blog/20261002"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.last(), " "); got != "forget --prune ccc ddd" {
		t.Fatalf("got %q", got)
	}
}

func TestRestoreTakesTheFolderFromTheSnapshotNotFromTheCaller(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"snapshots": reply(held, nil)}}
	e := engineWith(t, f)

	if err := e.RestoreDirectory(context.Background(), "nas", "blog/20261002/volume-data", "/new/place"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.last(), " "); got != "restore ccc:/vol/data --target /new/place --delete" {
		t.Fatalf("got %q", got)
	}
}

func TestFetchDumpsTheFileOfTheNewestSnapshotForThatPath(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){
		"snapshots": reply(held, nil),
		"dump":      reply(`{"app":"blog"}`, nil),
	}}
	e := engineWith(t, f)

	content, err := e.Fetch(context.Background(), "nas", "blog/20261002/manifest.json")
	if err != nil || string(content) != `{"app":"blog"}` {
		t.Fatalf("%q %v", content, err)
	}
	if got := strings.Join(f.last(), " "); got != "dump ddd /tmp/m2/manifest.json" {
		t.Fatalf("got %q", got)
	}
}

func TestRestoringWhatTheRepositoryDoesNotHoldSaysSo(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"snapshots": reply(held, nil)}}
	e := engineWith(t, f)

	err := e.RestoreDirectory(context.Background(), "nas", "blog/20261002/volume-gone", "/x")
	if !errors.Is(err, ErrNothingHeld) {
		t.Fatalf("err %v", err)
	}
}

// With the real binary: back a folder up, change it, put it back.
func TestWithTheRealBinary(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}

	KeyDir = t.TempDir()
	CacheDir = filepath.Join(t.TempDir(), "cache")
	// the service runs with neither: restic found no place for its cache and refused
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	repository := filepath.Join(t.TempDir(), "repo")
	e := &Engine{run: execRunner, repository: func(string) string { return repository }}
	ctx := context.Background()

	if _, err := e.Enable(ctx, "local", ""); err != nil {
		t.Fatal(err)
	}

	source := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("db.sqlite", "version one")
	write("notes.txt", "keep me")

	manifest := filepath.Join(t.TempDir(), "manifest.json")
	_ = os.WriteFile(manifest, []byte(`{"app":"blog"}`), 0o600)

	for _, stamp := range []string{"20261001", "20261002"} {
		if err := e.CopyDirectory(ctx, source, "local", "blog/"+stamp+"/volume-data"); err != nil {
			t.Fatal(err)
		}
		if err := e.CopyFile(ctx, manifest, "local", "blog/"+stamp+"/manifest.json"); err != nil {
			t.Fatal(err)
		}
	}

	stamps, err := e.Runs("local", "blog")
	if err != nil || strings.Join(stamps, ",") != "20261001,20261002" {
		t.Fatalf("stamps %v, err %v", stamps, err)
	}

	content, err := e.Fetch(ctx, "local", "blog/20261002/manifest.json")
	if err != nil || string(content) != `{"app":"blog"}` {
		t.Fatalf("manifest %q, err %v", content, err)
	}

	write("db.sqlite", "version two, a mistake")
	write("stranger.txt", "was never in the backup")

	if err := e.RestoreDirectory(ctx, "local", "blog/20261001/volume-data", source); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(source, "db.sqlite")); string(got) != "version one" {
		t.Fatalf("db.sqlite is %q after the restore", got)
	}
	if _, err := os.Stat(filepath.Join(source, "stranger.txt")); err == nil {
		t.Fatal("a file that was not in the backup survived the restore")
	}

	if err := e.Purge(ctx, "local", "blog/20261001"); err != nil {
		t.Fatal(err)
	}
	if stamps, _ := e.Runs("local", "blog"); strings.Join(stamps, ",") != "20261002" {
		t.Fatalf("stamps after the purge %v", stamps)
	}
}

func contains(list []string, item string) bool {
	for _, s := range list {
		if s == item {
			return true
		}
	}

	return false
}

func TestCheckAsksResticToCheckTheRepositoryOfThatDestination(t *testing.T) {
	f := &fake{}
	e := engineWith(t, f)

	if err := e.Check(context.Background(), "nas"); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(f.last(), " "); got != "check" {
		t.Fatalf("want a plain check, got %q", got)
	}
	if !strings.Contains(strings.Join(f.calls[0].env, " "), "RESTIC_REPOSITORY=repo-of-nas") {
		t.Fatalf("the repository of the destination: %v", f.calls[0].env)
	}
}

func TestACheckThatFindsAFaultSaysWhatResticSaid(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){
		"check": reply("", &ExitError{Code: 1, Message: "pack 1a2b: does not exist"}),
	}}
	e := engineWith(t, f)

	err := e.Check(context.Background(), "nas")

	var exit *ExitError
	if !errors.As(err, &exit) || !strings.Contains(exit.Message, "pack 1a2b") {
		t.Fatalf("the fault must reach the person: %v", err)
	}
}
