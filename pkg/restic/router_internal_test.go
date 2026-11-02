package restic

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
)

// stamped is the rclone side: the copies a destination held before restic.
type stamped struct {
	runs      map[string][]string
	runsError map[string]error
	fetched   []byte
	failure   error
	calls     []string
}

func (s *stamped) record(name string) { s.calls = append(s.calls, name) }

func (s *stamped) CopyDirectory(context.Context, string, string, string) error {
	s.record("CopyDirectory")
	return nil
}

func (s *stamped) CopyFile(context.Context, string, string, string) error {
	s.record("CopyFile")
	return nil
}

func (s *stamped) RestoreDirectory(context.Context, string, string, string) error {
	s.record("RestoreDirectory")
	return s.failure
}

func (s *stamped) RestoreFile(context.Context, string, string, string) error {
	s.record("RestoreFile")
	return s.failure
}

func (s *stamped) Fetch(context.Context, string, string) ([]byte, error) {
	s.record("Fetch")
	return s.fetched, s.failure
}

func (s *stamped) Runs(_ string, appPath string) ([]string, error) {
	s.record("Runs")
	if err := s.runsError[appPath]; err != nil {
		return nil, err
	}
	return s.runs[appPath], nil
}

func (s *stamped) Purge(context.Context, string, string) error {
	s.record("Purge")
	return s.failure
}

func (s *stamped) List(context.Context, string, string, string) ([]rclone.File, error) {
	s.record("List")
	return []rclone.File{{Name: "stamped.txt"}}, s.failure
}

func (s *stamped) CopyBack(context.Context, string, string, string, string) error {
	s.record("CopyBack")
	return s.failure
}

// routerWith holds the key of a repository at "nas" unless told not to.
func routerWith(t *testing.T, f *fake, s *stamped, repository bool) *Router {
	t.Helper()

	e := engineWith(t, f)
	if repository {
		keep(t, "nas")
	}

	return &Router{Restic: e, legacy: s}
}

func listing() map[string]func() ([]byte, error) {
	return map[string]func() ([]byte, error){"snapshots": reply(held, nil)}
}

func TestADestinationWithoutARepositoryIsRcloneAlone(t *testing.T) {
	f := &fake{replies: listing()}
	s := &stamped{runs: map[string][]string{"": {"blog"}}}
	r := routerWith(t, f, s, false)

	got, err := r.Runs("nas", "")
	if err != nil || !reflect.DeepEqual(got, []string{"blog"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("restic was asked about a destination that has none: %v", f.calls)
	}
}

func TestTheAppsAreTheRepositorysAndTheStampedCopiesTogether(t *testing.T) {
	s := &stamped{runs: map[string][]string{"": {"casaos-restic", "blog", "mail"}}}
	r := routerWith(t, &fake{replies: listing()}, s, true)

	got, err := r.Runs("nas", "")
	if err != nil {
		t.Fatal(err)
	}

	// blog is in both, mail only in the stamped copies, wiki only in the repository,
	// and the repository's own folder is not an app
	if want := []string{"blog", "mail", "wiki"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTheRunsOfAnAppAreBothSetsOfStampsOnceEach(t *testing.T) {
	s := &stamped{runs: map[string][]string{"blog": {"20260901", "20261001"}}}
	r := routerWith(t, &fake{replies: listing()}, s, true)

	got, err := r.Runs("nas", "blog")
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"20260901", "20261001", "20261002"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnAppWithNoStampedFolderStillListsItsRepositoryRuns(t *testing.T) {
	s := &stamped{runsError: map[string]error{"wiki": errors.New("rclone: directory not found")}}
	r := routerWith(t, &fake{replies: listing()}, s, true)

	got, err := r.Runs("nas", "wiki")
	if err != nil || !reflect.DeepEqual(got, []string{"20261002"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestAnErrorOfEitherSideIsNotMistakenForAnEmptyList(t *testing.T) {
	unreachable := errors.New("rclone: couldn't connect")

	s := &stamped{runsError: map[string]error{"": unreachable}}
	if _, err := routerWith(t, &fake{replies: listing()}, s, true).Runs("nas", ""); !errors.Is(err, unreachable) {
		t.Fatalf("the stamped side failed and the list was given anyway: %v", err)
	}

	f := &fake{replies: map[string]func() ([]byte, error){"snapshots": reply("", &ExitError{Code: 1, Message: "repository is locked"})}}
	if _, err := routerWith(t, f, &stamped{}, true).Runs("nas", ""); err == nil {
		t.Fatal("the repository failed and a list was given anyway")
	}
}

func TestACopyGoesToRcloneOnceTheDestinationIsSwitchedBack(t *testing.T) {
	f := &fake{}
	s := &stamped{}
	r := routerWith(t, f, s, true)
	ctx := context.Background()

	if err := r.CopyDirectory(ctx, "/vol", "nas", "blog/20261003/volume"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].args[3] != "backup" {
		t.Fatalf("an enabled destination must back up with restic: %v", f.calls)
	}

	if err := r.Restic.Disable("nas"); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := r.CopyDirectory(ctx, "/vol", "nas", "blog/20261004/volume"); err != nil {
		t.Fatal(err)
	}
	if err := r.CopyFile(ctx, "/etc/app.conf", "nas", "blog/20261004/app.conf"); err != nil {
		t.Fatal(err)
	}

	if len(f.calls) != 0 || !reflect.DeepEqual(s.calls, []string{"CopyDirectory", "CopyFile"}) {
		t.Fatalf("restic calls %v, rclone calls %v", f.calls, s.calls)
	}
}

func TestACopyFromBeforeTheSwitchIsRestoredAndReadFromTheStampedFolder(t *testing.T) {
	// the repository holds nothing at this path: it is one of the stamped copies
	s := &stamped{fetched: []byte("manifest")}
	r := routerWith(t, &fake{replies: listing()}, s, true)
	ctx := context.Background()

	if err := r.RestoreDirectory(ctx, "nas", "blog/20260901/volume-data", "/vol"); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreFile(ctx, "nas", "blog/20260901/app.conf", "/etc/app.conf"); err != nil {
		t.Fatal(err)
	}
	content, err := r.Fetch(ctx, "nas", "blog/20260901/manifest.json")
	if err != nil || string(content) != "manifest" {
		t.Fatalf("got %q, %v", content, err)
	}

	if want := []string{"RestoreDirectory", "RestoreFile", "Fetch"}; !reflect.DeepEqual(s.calls, want) {
		t.Fatalf("rclone calls %v, want %v", s.calls, want)
	}
}

func TestACopyThatTheRepositoryHoldsIsNotAskedOfRclone(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){
		"snapshots": reply(held, nil),
		"dump":      reply("from the repository", nil),
	}}
	s := &stamped{}
	r := routerWith(t, f, s, true)

	content, err := r.Fetch(context.Background(), "nas", "blog/20261001/manifest.json")
	if err != nil || string(content) != "from the repository" {
		t.Fatalf("got %q, %v", content, err)
	}
	if err := r.RestoreDirectory(context.Background(), "nas", "blog/20261001/volume-data", t.TempDir()); err != nil {
		t.Fatal(err)
	}

	if len(s.calls) != 0 {
		t.Fatalf("rclone was asked for what the repository holds: %v", s.calls)
	}
}

func TestAFailureOfTheRepositoryIsNotCoveredUpByTheStampedCopies(t *testing.T) {
	locked := &ExitError{Code: 1, Message: "repository is locked"}
	f := &fake{replies: map[string]func() ([]byte, error){"snapshots": reply("", locked)}}
	s := &stamped{fetched: []byte("old")}
	r := routerWith(t, f, s, true)

	if _, err := r.Fetch(context.Background(), "nas", "blog/20261001/manifest.json"); !errors.Is(err, locked) {
		t.Fatalf("err %v", err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("rclone answered in place of a repository that failed: %v", s.calls)
	}
}

func TestDeletingARunTheRepositoryHoldsForgetsItAndLeavesRcloneAlone(t *testing.T) {
	f := &fake{replies: listing()}
	s := &stamped{}
	r := routerWith(t, f, s, true)

	if err := r.Purge(context.Background(), "nas", "blog/20261001"); err != nil {
		t.Fatal(err)
	}

	forgot := f.last()
	if forgot[0] != "forget" || !contains(forgot, "aaa") || !contains(forgot, "bbb") || contains(forgot, "ccc") {
		t.Fatalf("expected exactly the two snapshots of that run to be forgotten, got %v", forgot)
	}
	if len(s.calls) != 0 {
		t.Fatalf("rclone was asked to purge a run the repository held: %v", s.calls)
	}
}

func TestDeletingAStampedCopyPurgesItThroughRclone(t *testing.T) {
	f := &fake{replies: listing()}
	s := &stamped{}
	r := routerWith(t, f, s, true)

	if err := r.Purge(context.Background(), "nas", "blog/20260901"); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(s.calls, []string{"Purge"}) {
		t.Fatalf("rclone calls %v", s.calls)
	}
	for _, c := range f.calls {
		if c.args[3] == "forget" {
			t.Fatal("a snapshot was forgotten for a run the repository does not hold")
		}
	}
}

func TestDeletingARunNobodyHoldsAnyMoreIsNotAnError(t *testing.T) {
	s := &stamped{failure: errors.New("rclone: directory not found")}
	r := routerWith(t, &fake{replies: listing()}, s, true)

	if err := r.Purge(context.Background(), "nas", "blog/20250101"); err != nil {
		t.Fatalf("err %v", err)
	}
}

func TestARealFailureToPurgeStillReachesThePerson(t *testing.T) {
	broken := errors.New("rclone: couldn't connect")
	r := routerWith(t, &fake{replies: listing()}, &stamped{failure: broken}, true)

	if err := r.Purge(context.Background(), "nas", "blog/20260901"); !errors.Is(err, broken) {
		t.Fatalf("err %v", err)
	}
}

func TestAFolderOfARunInTheRepositoryIsListedFromTheRepository(t *testing.T) {
	s := &stamped{}
	f := &fake{replies: map[string]func() ([]byte, error){
		"snapshots": reply(held, nil),
		"ls":        reply(`{"name":"a.txt","type":"file","path":"/vol/data/a.txt","struct_type":"node"}`, nil),
	}}

	files, err := routerWith(t, f, s, true).List(context.Background(), "nas", "blog/20261002/volume-data", "")
	if err != nil || len(files) != 1 || files[0].Name != "a.txt" {
		t.Fatalf("%+v %v", files, err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("the stamped copies were asked too: %v", s.calls)
	}
}

func TestAStampedCopyIsListedAndTakenBackFromRcloneWhileTheRepositoryIsKept(t *testing.T) {
	s := &stamped{}
	r := routerWith(t, &fake{replies: listing()}, s, true)

	files, err := r.List(context.Background(), "nas", "blog/20260901/volume-data", "")
	if err != nil || len(files) != 1 || files[0].Name != "stamped.txt" {
		t.Fatalf("%+v %v", files, err)
	}
	if err := r.CopyBack(context.Background(), "nas", "blog/20260901/volume-data", "a", "/host/a"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.calls, []string{"List", "CopyBack"}) {
		t.Fatalf("got %v", s.calls)
	}
}

func TestADestinationWithoutARepositoryListsAndTakesBackFromRcloneAlone(t *testing.T) {
	s := &stamped{}
	f := &fake{replies: listing()}
	r := routerWith(t, f, s, false)

	if _, err := r.List(context.Background(), "nas", "blog/20260901/volume-data", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.CopyBack(context.Background(), "nas", "blog/20260901/volume-data", "", "/host"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("restic was asked about a destination that has none: %v", f.calls)
	}
}

func TestAFailureOfTheRepositoryIsNotCoveredUpWhenListing(t *testing.T) {
	s := &stamped{}
	broken := errors.New("restic: wrong password")
	f := &fake{replies: map[string]func() ([]byte, error){"snapshots": reply("", broken)}}

	if _, err := routerWith(t, f, s, true).List(context.Background(), "nas", "blog/20261002/volume-data", ""); !errors.Is(err, broken) {
		t.Fatalf("err %v", err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("fell back to the stamped copies: %v", s.calls)
	}
}
