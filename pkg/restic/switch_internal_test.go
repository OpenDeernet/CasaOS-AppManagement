package restic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// keep puts the key of a repository on this box, as Enable does.
func keep(t *testing.T, name string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(KeyDir, name+".key"), []byte("the-password"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchingBackKeepsTheKeyAndStopsOnlyTheNextBackups(t *testing.T) {
	e := engineWith(t, &fake{})
	keep(t, "nas")

	if err := e.Disable("nas"); err != nil {
		t.Fatal(err)
	}

	if e.Enabled("nas") {
		t.Fatal("the next backups still go to restic")
	}
	if !e.Held("nas") {
		t.Fatal("switching back threw the key away: the repository could not be opened again")
	}
	if _, err := os.Stat(filepath.Join(KeyDir, "nas.key")); err != nil {
		t.Fatalf("the key file is gone: %v", err)
	}
}

func TestSwitchingBackTwiceIsNotAnError(t *testing.T) {
	e := engineWith(t, &fake{})
	keep(t, "nas")

	for i := 0; i < 2; i++ {
		if err := e.Disable("nas"); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
}

func TestSwitchingBackADestinationThatNeverKeptARepositoryIsRefused(t *testing.T) {
	e := engineWith(t, &fake{})

	if err := e.Disable("nas"); !errors.Is(err, ErrNotKept) {
		t.Fatalf("err %v", err)
	}
	if err := e.Disable("a/b"); err == nil {
		t.Fatal("a name that is a path was accepted")
	}
	if _, err := os.Stat(filepath.Join(KeyDir, "nas.key.off")); err == nil {
		t.Fatal("a marker was left for a destination with no repository")
	}
}

func TestTakingARepositoryUpAgainChecksItOpensAndShowsNoPassword(t *testing.T) {
	f := &fake{}
	e := engineWith(t, f)
	keep(t, "nas")
	_ = e.Disable("nas")

	password, err := e.Enable(context.Background(), "nas", "")
	if err != nil || password != "" {
		t.Fatalf("password %q, err %v", password, err)
	}

	if !e.Enabled("nas") {
		t.Fatal("the next backups do not go to restic again")
	}
	if len(f.calls) != 1 || f.calls[0].args[3] != "cat" {
		t.Fatalf("expected one cat config and nothing else, got %v", f.calls)
	}
	stored, _ := os.ReadFile(filepath.Join(KeyDir, "nas.key"))
	if string(stored) != "the-password" {
		t.Fatalf("the key was rewritten: %q", stored)
	}
}

func TestARepositoryThatWentMissingIsNotReplacedByAnEmptyOne(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"cat": reply("", &ExitError{Code: exitNoRepository, Message: "unable to open config file"})}}
	e := engineWith(t, f)
	keep(t, "nas")
	_ = e.Disable("nas")

	if _, err := e.Enable(context.Background(), "nas", ""); err == nil {
		t.Fatal("a repository that is not there was taken up again")
	}

	for _, c := range f.calls {
		if c.args[3] == "init" {
			t.Fatal("an empty repository was made over the one that went missing")
		}
	}
	if e.Enabled("nas") {
		t.Fatal("the destination was switched on although its repository does not open")
	}
}

func TestTakingARepositoryUpAgainWithAWrongKeySaysSo(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"cat": reply("", &ExitError{Code: exitWrongKey})}}
	e := engineWith(t, f)
	keep(t, "nas")
	_ = e.Disable("nas")

	if _, err := e.Enable(context.Background(), "nas", ""); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("err %v", err)
	}
	if e.Enabled("nas") {
		t.Fatal("switched on with a key that does not open the repository")
	}
}

func TestAPasswordIsRefusedForADestinationWhoseKeyThisBoxStillHas(t *testing.T) {
	f := &fake{}
	e := engineWith(t, f)
	keep(t, "nas")
	_ = e.Disable("nas")

	if _, err := e.Enable(context.Background(), "nas", "another"); !errors.Is(err, ErrKeyKept) {
		t.Fatalf("err %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("restic was run for a request that was refused: %v", f.calls)
	}
	stored, _ := os.ReadFile(filepath.Join(KeyDir, "nas.key"))
	if string(stored) != "the-password" {
		t.Fatalf("the key was replaced: %q", stored)
	}
}

func TestAMarkerLeftWithoutItsKeyDoesNotSwitchOffANewRepository(t *testing.T) {
	f := &fake{replies: map[string]func() ([]byte, error){"cat": reply("", &ExitError{Code: exitNoRepository})}}
	e := engineWith(t, f)
	if err := os.WriteFile(filepath.Join(KeyDir, "nas.key.off"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Enable(context.Background(), "nas", ""); err != nil {
		t.Fatal(err)
	}

	if !e.Enabled("nas") {
		t.Fatal("a new repository stayed switched off by an old marker")
	}
}

func TestAnEnabledDestinationIsStillRefusedASecondEnable(t *testing.T) {
	e := engineWith(t, &fake{})
	keep(t, "nas")

	if _, err := e.Enable(context.Background(), "nas", ""); !errors.Is(err, ErrAlreadyEnabled) {
		t.Fatalf("err %v", err)
	}
}
