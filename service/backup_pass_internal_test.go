package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// passRunner answers each app as it is told: an error, or a wait until the test lets it go.
type passRunner struct {
	mu      sync.Mutex
	fail    map[string]error
	hold    map[string]chan struct{}
	started chan string
}

func (p *passRunner) Run(_ context.Context, schedule BackupSchedule, _ string) (BackupManifest, error) {
	if p.started != nil {
		p.started <- schedule.App
	}
	p.mu.Lock()
	gate, err := p.hold[schedule.App], p.fail[schedule.App]
	p.mu.Unlock()
	if gate != nil {
		<-gate
	}

	return BackupManifest{Bytes: 1820}, err
}

func (p *passRunner) Runs(_, _ string) ([]string, error)         { return nil, nil }
func (p *passRunner) Purge(_ context.Context, _, _ string) error { return nil }

func passOf(t *testing.T, origin BackupOrigin) BackupPass {
	t.Helper()

	for _, pass := range BackupPasses() {
		if pass.Origin == origin {
			return pass
		}
	}
	t.Fatalf("no %s pass in %+v", origin, BackupPasses())

	return BackupPass{}
}

func states(pass BackupPass) string {
	out := ""
	for _, item := range pass.Items {
		out += item.App + ":" + string(item.State) + " "
	}

	return out
}

func TestAPassListsWhatWaitsRunsAndIsDone(t *testing.T) {
	schedulerInTempDir(t)
	_ = SaveBackupSchedules(threeDue())

	gate := make(chan struct{})
	runner := &passRunner{hold: map[string]chan struct{}{"jellyfin": gate}, started: make(chan string, 3)}
	done := make(chan struct{})
	go func() {
		RunDueBackups(context.Background(), at(10, 7, 0), runner)
		close(done)
	}()
	for app := range runner.started {
		if app == "jellyfin" {
			break
		}
	}

	pass := passOf(t, OriginScheduled)
	if got := states(pass); got != "immich:done jellyfin:running nextcloud:waiting " {
		t.Fatalf("mid-pass: %s", got)
	}
	if pass.Items[0].Bytes != 1820 || pass.Items[0].FinishedAt == nil || pass.Items[1].StartedAt == nil || pass.FinishedAt != nil {
		t.Fatalf("what is known mid-pass: %+v", pass)
	}

	close(gate)
	<-done
	pass = passOf(t, OriginScheduled)
	if got := states(pass); got != "immich:done jellyfin:done nextcloud:done " || pass.FinishedAt == nil {
		t.Fatalf("at the end: %s %+v", got, pass.FinishedAt)
	}
}

func TestAFailedAndAStoppedItemAreToldApart(t *testing.T) {
	schedulerInTempDir(t)
	_ = SaveBackupSchedules(threeDue())

	runner := &passRunner{fail: map[string]error{
		"immich":   errors.New("destination refused it"),
		"jellyfin": fmt.Errorf("%w: copying /x: context canceled", ErrStoppedFromInterface),
	}}
	RunDueBackups(context.Background(), at(10, 7, 0), runner)

	pass := passOf(t, OriginScheduled)
	if got := states(pass); got != "immich:failed jellyfin:stopped nextcloud:done " {
		t.Fatalf("got %s", got)
	}
	if pass.Items[0].Error != "destination refused it" {
		t.Fatalf("the failure says why: %q", pass.Items[0].Error)
	}
}

func TestAnUpgradeHoldsTheRestOfThePass(t *testing.T) {
	schedulerInTempDir(t)
	flag := upgradeFlagIn(t)
	_ = SaveBackupSchedules(threeDue())

	RunDueBackups(context.Background(), at(10, 7, 0), &openingRunner{flag: flag})

	if got := states(passOf(t, OriginScheduled)); got != "immich:done jellyfin:held nextcloud:held " {
		t.Fatalf("got %s", got)
	}
}

func TestAForcedChainIsAPassOfItsOwn(t *testing.T) {
	schedulerInTempDir(t)
	clockAt(t, at(10, 15, 30))
	_ = SaveBackupSchedules(threeDue()[:2])

	RunDueBackups(context.Background(), at(10, 7, 0), &passRunner{})
	if _, err := ForceSchedules(context.Background(), ForceRequest{}, &passRunner{}); err != nil {
		t.Fatal(err)
	}
	forcedRuns.Wait()

	if len(BackupPasses()) != 2 {
		t.Fatalf("one pass each: %+v", BackupPasses())
	}
	if got := states(passOf(t, OriginForced)); got != "immich:done jellyfin:done " {
		t.Fatalf("the forced chain: %s", got)
	}
	if passOf(t, OriginScheduled).StartedAt.After(time.Now()) {
		t.Fatal("the scheduled pass is still there")
	}
}
