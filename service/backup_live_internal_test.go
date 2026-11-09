package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/backupprogress"
)

// stallingCopier reports its stats, says it started, then holds its first copy until it is
// cancelled (the copy's error) or released (nil). Every copy after a release passes.
type stallingCopier struct {
	report   backupprogress.Stats
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
	released bool
}

func (s *stallingCopier) copy(ctx context.Context) error {
	if s.released {
		return nil
	}
	backupprogress.Report(ctx, s.report)
	s.once.Do(func() { s.started <- struct{}{} })

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		s.released = true
		return nil
	}
}

func (s *stallingCopier) CopyDirectory(ctx context.Context, _, _, _ string) error { return s.copy(ctx) }
func (s *stallingCopier) CopyFile(ctx context.Context, _, _, _ string) error      { return s.copy(ctx) }

func stalling() *stallingCopier {
	eta := int64(3390)

	return &stallingCopier{
		report:  backupprogress.Stats{Bytes: 1182844747, TotalBytes: 1926565931, Files: 9812, TotalFiles: 25488, Speed: 219371, ETA: &eta, Current: "cache/a.jpg"},
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func liveOf(t *testing.T, app string) LiveBackup {
	t.Helper()

	for _, live := range LiveBackups() {
		if live.App == app {
			return live
		}
	}
	t.Fatalf("%s is not listed: %+v", app, LiveBackups())

	return LiveBackup{}
}

func TestARunningBackupIsListedWithItsOperationAndStats(t *testing.T) {
	app, _ := appWithOneBindAndOneVolume(t)
	copier := stalling()

	done := make(chan error, 1)
	go func() {
		_, err := RunBackup(context.Background(), app, &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/x"}}, copier,
			BackupOptions{Destination: "offsite", Stamp: "2026-11-08T16-55-18Z"})
		done <- err
	}()
	<-copier.started

	live := liveOf(t, "demo")
	if live.ID == "" || live.Destination != "offsite" || live.Stamp != "2026-11-08T16-55-18Z" || live.Origin != OriginOnDemand {
		t.Fatalf("who and where: %+v", live)
	}
	if live.Operation != 1 || live.Operations < 2 || live.Source == "" || live.StartedAt.IsZero() {
		t.Fatalf("which operation: %+v", live)
	}
	if live.Stats == nil || live.Stats.Bytes != 1182844747 || live.Stats.TotalFiles != 25488 || live.StatsAt.IsZero() {
		t.Fatalf("its figures: %+v", live.Stats)
	}

	close(copier.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, live := range LiveBackups() {
		if live.App == "demo" {
			t.Fatalf("a finished backup is not listed: %+v", live)
		}
	}
}

func TestStoppingABackupEndsItAndSaysWhy(t *testing.T) {
	logInTempDir(t)
	app, _ := appWithOneBindAndOneVolume(t)
	copier := stalling()

	done := make(chan error, 1)
	go func() {
		_, err := BackupOnDemand(context.Background(), app, &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/x"}}, copier,
			BackupOptions{Destination: "offsite", Stamp: "run"})
		done <- err
	}()
	<-copier.started

	if err := StopLiveBackup(liveOf(t, "demo").ID); err != nil {
		t.Fatal(err)
	}

	err := <-done
	if !errors.Is(err, ErrStoppedFromInterface) {
		t.Fatalf("the run ends because it was stopped: %v", err)
	}
	records, _ := BackupRuns()
	if len(records) != 1 || !strings.Contains(records[0].Error, "stopped from the interface") {
		t.Fatalf("the history says it was stopped: %+v", records)
	}
}

func TestStoppingAHeldBackupBringsTheAppBack(t *testing.T) {
	holdNoteInTempDir(t)
	app, _ := appWithOneBindAndOneVolume(t)
	docker := &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/x"}}
	copier := stalling()

	done := make(chan error, 1)
	go func() {
		_, err := RunBackup(context.Background(), app, docker, copier, BackupOptions{
			Destination: "offsite", Stamp: "run", HoldStill: true,
			Containers: func(context.Context) (map[string][]codegen.ContainerSummary, error) {
				return map[string][]codegen.ContainerSummary{"app": {{ID: "c1", State: "running"}}}, nil
			},
		})
		done <- err
	}()
	<-copier.started

	if err := StopLiveBackup(liveOf(t, "demo").ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrStoppedFromInterface) {
		t.Fatalf("stopped: %v", err)
	}
	if strings.Join(docker.stopped, ",") != "c1" || strings.Join(docker.started, ",") != "c1" {
		t.Fatalf("the app went down for the copy and came back after the stop: stopped %v, started %v", docker.stopped, docker.started)
	}
}

func TestTwoBackupsAreListedApartAndStopApart(t *testing.T) {
	first, _ := appWithOneBindAndOneVolume(t)
	second, _ := appWithOneBindAndOneVolume(t)
	second.Name = "other"
	one, two := stalling(), stalling()

	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := RunBackup(context.Background(), first, &fakeBackupDocker{}, one, BackupOptions{Destination: "offsite", Stamp: "a"})
		firstDone <- err
	}()
	go func() {
		_, err := RunBackup(context.Background(), second, &fakeBackupDocker{}, two, BackupOptions{Destination: "offsite", Stamp: "b"})
		secondDone <- err
	}()
	<-one.started
	<-two.started

	a, b := liveOf(t, "demo"), liveOf(t, "other")
	if a.ID == b.ID {
		t.Fatalf("one id each: %q", a.ID)
	}

	if err := StopLiveBackup(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-firstDone; !errors.Is(err, ErrStoppedFromInterface) {
		t.Fatalf("the first was stopped: %v", err)
	}
	if liveOf(t, "other").ID != b.ID {
		t.Fatal("the second still runs")
	}

	close(two.release)
	if err := <-secondDone; err != nil {
		t.Fatalf("the second finished on its own: %v", err)
	}
}

func TestStoppingAnUnknownBackupIsNotFound(t *testing.T) {
	if err := StopLiveBackup("nope"); !errors.Is(err, ErrLiveBackupNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestProgressEventsCarryTheFractionAtMostEveryFiveSeconds(t *testing.T) {
	clock := clockAt(t, at(10, 3, 0))

	var mu sync.Mutex
	var fractions []float64
	was := publishProgressFraction
	publishProgressFraction = func(_ context.Context, fraction float64, _ string) {
		mu.Lock()
		defer mu.Unlock()
		fractions = append(fractions, fraction)
	}
	t.Cleanup(func() { publishProgressFraction = was })

	half := backupprogress.Stats{Bytes: 50, TotalBytes: 100}
	copier := &scriptedCopier{steps: []func(context.Context){
		func(ctx context.Context) { backupprogress.Report(ctx, half) },
		func(ctx context.Context) { *clock = clock.Add(time.Second); backupprogress.Report(ctx, half) },
		func(ctx context.Context) { *clock = clock.Add(5 * time.Second); backupprogress.Report(ctx, half) },
	}}
	app, _ := appWithOneBindAndOneVolume(t)
	manifest, err := RunBackup(context.Background(), app, &fakeBackupDocker{mountpoints: map[string]string{"demo_data": "/x"}}, copier,
		BackupOptions{Destination: "offsite", Stamp: "run"})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := 0.5 / float64(len(manifest.Operations)+1)
	if len(fractions) != 2 || fractions[0] != want || fractions[1] != want {
		t.Fatalf("two events of %v, got %v", want, fractions)
	}
}

// scriptedCopier runs its steps on its first copy, then passes every copy.
type scriptedCopier struct {
	steps []func(context.Context)
	done  bool
}

func (s *scriptedCopier) copy(ctx context.Context) error {
	if !s.done {
		s.done = true
		for _, step := range s.steps {
			step(ctx)
		}
	}

	return nil
}

func (s *scriptedCopier) CopyDirectory(ctx context.Context, _, _, _ string) error { return s.copy(ctx) }
func (s *scriptedCopier) CopyFile(ctx context.Context, _, _, _ string) error      { return s.copy(ctx) }

// originRunner remembers where each backup it ran was said to come from.
type originRunner struct {
	mu      sync.Mutex
	origins []BackupOrigin
}

func (o *originRunner) Run(ctx context.Context, _ BackupSchedule, _ string) (BackupManifest, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.origins = append(o.origins, backupOriginOf(ctx))

	return BackupManifest{}, nil
}

func (o *originRunner) Runs(_, _ string) ([]string, error)         { return nil, nil }
func (o *originRunner) Purge(_ context.Context, _, _ string) error { return nil }

func TestTheSchedulerAndAForceSayWhereABackupComesFrom(t *testing.T) {
	schedulerInTempDir(t)
	clockAt(t, at(10, 15, 30))

	if backupOriginOf(context.Background()) != OriginOnDemand {
		t.Fatal("a backup nobody scheduled was asked for")
	}

	scheduled := &originRunner{}
	runScheduled(context.Background(), at(10, 3, 0), daily(time.Time{}), scheduled)
	if len(scheduled.origins) != 1 || scheduled.origins[0] != OriginScheduled {
		t.Fatalf("the scheduler's: %v", scheduled.origins)
	}

	_ = SaveBackupSchedules([]BackupSchedule{daily(at(10, 3, 0))})
	forced := &originRunner{}
	if _, err := ForceSchedules(context.Background(), ForceRequest{}, forced); err != nil {
		t.Fatal(err)
	}
	forcedRuns.Wait()
	if len(forced.origins) != 1 || forced.origins[0] != OriginForced {
		t.Fatalf("a force's: %v", forced.origins)
	}
}
