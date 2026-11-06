package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/logstore"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/ini.v1"
	"gotest.tools/v3/assert"
)

// fakeLogsDocker answers the collector with a scripted container list, TTY flag and log body.
type fakeLogsDocker struct {
	list []container.Summary
	tty  bool
	body func() io.ReadCloser

	mu   sync.Mutex
	opts []container.LogsOptions
}

func (f *fakeLogsDocker) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list, nil
}

func (f *fakeLogsDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{Config: &container.Config{Tty: f.tty}}, nil
}

func (f *fakeLogsDocker) ContainerLogs(_ context.Context, _ string, o container.LogsOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opts = append(f.opts, o)
	f.mu.Unlock()
	return f.body(), nil
}

func bodyOf(s string) func() io.ReadCloser {
	return func() io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
}

const testID = "0123456789abcdef0123"

var testLabels = map[string]string{api.ProjectLabel: "plex", api.ServiceLabel: "plex"}

// openLogs puts a store on a temporary root in place of the collector's.
func openLogs(t *testing.T) *logstore.Store {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := logstore.Open(ctx, t.TempDir())
	assert.NilError(t, err)
	SetLogs(s)
	t.Cleanup(func() {
		stopFollowers(nil)
		SetLogs(nil)
		assert.NilError(t, s.Close())
		cancel()
	})
	return s
}

func waitNoFollower(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(FollowedContainers()) > 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the follower did not end")
		}
	}
}

func dayFile(t *testing.T, s *logstore.Store) string {
	t.Helper()
	b, err := os.ReadFile(logstore.DayFile(s.Root, "plex", "plex", testID, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)))
	if os.IsNotExist(err) {
		return ""
	}
	assert.NilError(t, err)
	return string(b)
}

func TestLogTargetFollowsComposeAndV1ByDefault(t *testing.T) {
	for _, c := range []struct {
		labels       map[string]string
		name         string
		others       bool
		app, service string
		ok           bool
	}{
		{map[string]string{api.ProjectLabel: "jellyfin", api.ServiceLabel: "jellyfin"}, "/jellyfin-jellyfin-1", false, "jellyfin", "jellyfin", true},
		{map[string]string{api.ProjectLabel: "jellyfin", api.ServiceLabel: "db", api.OneoffLabel: "True"}, "/x", false, "", "", false},
		{map[string]string{"casaos": "casaos"}, "/nextcloud", false, "nextcloud", "main", true},
		{map[string]string{}, "/watchtower", false, "", "", false},
		{map[string]string{}, "/watchtower", true, "_other", "watchtower", true},
	} {
		app, service, ok := logTarget(c.labels, c.name, c.others)
		assert.Equal(t, app, c.app)
		assert.Equal(t, service, c.service)
		assert.Equal(t, ok, c.ok)
	}
}

func TestLogEventOfDockerActions(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		action events.Action
		held   bool
		kind   string
		exit   string
		by     string
		ok     bool
	}{
		{events.ActionCreate, false, "created", "", "docker", true},
		{events.ActionStart, false, "started", "", "docker", true},
		{events.ActionStop, true, "stopped", "", "operation", true},
		{events.ActionDie, false, "died", "137", "docker", true},
		{events.ActionOOM, false, "oom-killed", "", "docker", true},
		{events.ActionHealthStatusUnhealthy, false, "unhealthy", "", "docker", true},
		{events.ActionHealthStatusHealthy, false, "healthy", "", "docker", true},
		{events.ActionKill, false, "", "", "", false},
	} {
		m := events.Message{Action: c.action, TimeNano: at.UnixNano(), Actor: events.Actor{ID: testID, Attributes: map[string]string{"exitCode": "137"}}}
		e, ok := logEventOf(m, "plex", "plex", c.held)
		assert.Equal(t, ok, c.ok, string(c.action))
		if !ok {
			continue
		}
		assert.Equal(t, e.Kind, c.kind)
		assert.Equal(t, e.ExitCode, c.exit)
		assert.Equal(t, e.By, c.by)
		assert.Equal(t, e.Container, testID[:12])
		assert.Equal(t, e.App, "plex")
		assert.Equal(t, e.Service, "plex")
		assert.Assert(t, e.At.Equal(at))
	}
}

func TestSinceForAndTheBoundaryDedupe(t *testing.T) {
	last := time.Date(2026, 11, 5, 10, 0, 0, 500, time.UTC)
	assert.Equal(t, sinceFor(time.Time{}, false, time.Time{}), "")
	assert.Equal(t, sinceFor(last, true, time.Time{}), "2026-11-05T10:00:00.000000501Z")
	// the retention's start bounds a first follow, and a last line older than it
	floor := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	assert.Equal(t, sinceFor(time.Time{}, false, floor), "2026-10-29T10:00:00Z")
	assert.Equal(t, sinceFor(floor.Add(-time.Hour), true, floor), "2026-10-29T10:00:00Z")
	assert.Equal(t, sinceFor(last, true, floor), "2026-11-05T10:00:00.000000501Z")

	s := openLogs(t)
	w := s.Writer("plex", "plex", testID)
	assert.NilError(t, w.Write(last, "o", "same"))
	assert.NilError(t, s.CloseWriter(testID))

	docker := &fakeLogsDocker{tty: true, body: bodyOf(
		"2026-11-05T10:00:00.0000005Z same\n2026-11-05T10:00:01Z next\n")}
	follow(context.Background(), docker, testID, "plex", "plex", "plex", false)
	waitNoFollower(t)

	assert.Equal(t, docker.opts[0].Since, sinceFor(last, true, time.Time{}))
	assert.Equal(t, dayFile(t, s), "2026-11-05T10:00:00.0000005Z o same\n2026-11-05T10:00:01Z o next\n")
}

func TestATTYContainerIsReadRaw(t *testing.T) {
	s := openLogs(t)
	docker := &fakeLogsDocker{tty: true, body: bodyOf("2026-11-05T10:00:00.000000000Z hello\r\n")}
	follow(context.Background(), docker, testID, "plex", "plex", "plex", false)
	waitNoFollower(t)

	o := docker.opts[0]
	assert.Assert(t, o.ShowStdout && o.ShowStderr && o.Follow && o.Timestamps)
	// no line on disk: the follow starts at the retention's start, 7 days ago
	assert.Equal(t, config.AppInfo.LogsRetentionDays, 7)
	since, err := time.Parse(time.RFC3339Nano, o.Since)
	assert.NilError(t, err)
	ago := time.Since(since)
	assert.Assert(t, ago >= 7*24*time.Hour && ago < 7*24*time.Hour+time.Minute, ago)
	assert.Equal(t, dayFile(t, s), "2026-11-05T10:00:00Z o hello\n")
}

func TestAMultiplexedStreamSplitsStdoutAndStderr(t *testing.T) {
	s := openLogs(t)
	var framed bytes.Buffer
	_, err := stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write([]byte("2026-11-05T10:00:00Z a\n"))
	assert.NilError(t, err)
	_, err = stdcopy.NewStdWriter(&framed, stdcopy.Stderr).Write([]byte("2026-11-05T10:00:01Z b\n"))
	assert.NilError(t, err)

	follow(context.Background(), &fakeLogsDocker{body: bodyOf(framed.String())}, testID, "plex", "plex", "plex", false)
	waitNoFollower(t)

	assert.Equal(t, dayFile(t, s), "2026-11-05T10:00:00Z o a\n2026-11-05T10:00:01Z e b\n")
}

func TestDieFlushesBeforeTheEventIsRecorded(t *testing.T) {
	s := openLogs(t)
	ch, cancel := s.Subscribe(nil)
	defer cancel()

	// the stream ends a moment after the die, as Docker's does
	docker := &fakeLogsDocker{tty: true, body: func() io.ReadCloser {
		r, w := io.Pipe()
		go func() {
			_, _ = io.WriteString(w, "2026-11-05T10:00:00Z one\n2026-11-05T10:00:01Z two\n")
			time.Sleep(300 * time.Millisecond)
			_ = w.Close()
		}()
		return r
	}}
	ctx := context.Background()
	attrs := map[string]string{api.ProjectLabel: "plex", api.ServiceLabel: "plex", "name": "plex", "exitCode": "137"}
	logsOnEvent(ctx, docker, events.Message{Action: events.ActionStart, TimeNano: time.Now().UnixNano(), Actor: events.Actor{ID: testID, Attributes: attrs}})
	assert.DeepEqual(t, FollowedContainers(), map[string]bool{testID[:12]: true})
	logsOnEvent(ctx, docker, events.Message{Action: events.ActionDie, TimeNano: time.Now().UnixNano(), Actor: events.Actor{ID: testID, Attributes: attrs}})

	var seen []string
	for len(seen) < 4 {
		select {
		case m := <-ch:
			if m.Line != nil {
				seen = append(seen, m.Line.Text)
				continue
			}
			seen = append(seen, m.Event.Kind+" "+m.Event.ExitCode)
			if m.Event.Kind == "died" {
				assert.Equal(t, dayFile(t, s), "2026-11-05T10:00:00Z o one\n2026-11-05T10:00:01Z o two\n")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v", seen)
		}
	}
	assert.DeepEqual(t, seen, []string{"started ", "one", "two", "died 137"})
	assert.Equal(t, len(FollowedContainers()), 0)
}

func TestADieRecordedBeforeTheStreamEndsLosesNoLine(t *testing.T) {
	drain := dieDrain
	dieDrain = 200 * time.Millisecond
	t.Cleanup(func() { dieDrain = drain })
	s := openLogs(t)
	ch, cancel := s.Subscribe(nil)
	defer cancel()

	// the first run's last line comes 400 ms after the die, well past the drain even under -race;
	// the restart's run has nothing new
	runs := 0
	docker := &fakeLogsDocker{tty: true, body: func() io.ReadCloser {
		runs++
		if runs > 1 {
			return io.NopCloser(strings.NewReader(""))
		}
		r, w := io.Pipe()
		go func() {
			_, _ = io.WriteString(w, "2026-11-05T10:00:00Z one\n")
			time.Sleep(400 * time.Millisecond)
			_, _ = io.WriteString(w, "2026-11-05T10:00:01Z late\n")
			_ = w.Close()
		}()
		return r
	}}
	ctx := context.Background()
	attrs := map[string]string{api.ProjectLabel: "plex", api.ServiceLabel: "plex", "name": "plex", "exitCode": "1"}
	event := func(a events.Action) {
		logsOnEvent(ctx, docker, events.Message{Action: a, TimeNano: time.Now().UnixNano(), Actor: events.Actor{ID: testID, Attributes: attrs}})
	}
	event(events.ActionStart)
	event(events.ActionDie)
	event(events.ActionStart) // the restart, while the first run still drains

	var seen []string
	for len(seen) < 5 {
		select {
		case m := <-ch:
			if m.Line != nil {
				seen = append(seen, m.Line.Text)
			} else {
				seen = append(seen, m.Event.Kind)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v", seen)
		}
	}
	assert.DeepEqual(t, seen, []string{"started", "one", "died", "started", "late"})
	waitNoFollower(t)
	assert.Equal(t, len(docker.opts), 2) // the restart was followed once the first run ended
	assert.Equal(t, dayFile(t, s), "2026-11-05T10:00:00Z o one\n2026-11-05T10:00:01Z o late\n")
}

func TestAFrameLargerThanAChunkIsSplitIntoItsLines(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	var framed bytes.Buffer
	_, err := stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write([]byte("A a\nB " + long + "\nC c"))
	assert.NilError(t, err)
	framed.Write([]byte{1, 0, 0, 0, 0, 0, 0, 9, 'D'}) // a frame cut short: its partial line is kept

	var lines []string
	err = readFrames(&framed, func(b []byte) { lines = append(lines, string(b)) }, nil)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.DeepEqual(t, lines, []string{"A a", "B " + long, "C cD"})
}

func TestTheStoreWaitsForItsRoot(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "missing")
	assert.NilError(t, os.WriteFile(missing, nil, 0o600))

	path, retry := config.AppInfo.LogsPath, logsRetry
	config.AppInfo.LogsPath, logsRetry = filepath.Join(missing, "sub"), 20*time.Millisecond
	t.Cleanup(func() { config.AppInfo.LogsPath, logsRetry = path, retry })

	assert.NilError(t, StartLogs(context.Background(), &fakeLogsDocker{}))
	defer StopLogs()
	time.Sleep(50 * time.Millisecond)
	assert.Assert(t, Logs() == nil)

	assert.NilError(t, os.Remove(missing))
	assert.NilError(t, os.Mkdir(missing, 0o755))
	for deadline := time.Now().Add(5 * time.Second); Logs() == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the store did not open once its root could be made")
		}
	}
	StopLogs()
	assert.Assert(t, Logs() == nil)
}

func TestARunningContainerIsFollowedWithinATick(t *testing.T) {
	path, tick := config.AppInfo.LogsPath, logsTick
	config.AppInfo.LogsPath, logsTick = filepath.Join(t.TempDir(), "logs"), 20*time.Millisecond
	t.Cleanup(func() { config.AppInfo.LogsPath, logsTick = path, tick })

	docker := &fakeLogsDocker{tty: true, body: bodyOf("")}
	assert.NilError(t, StartLogs(context.Background(), docker))
	defer StopLogs()
	followed := func() int {
		docker.mu.Lock()
		defer docker.mu.Unlock()
		return len(docker.opts)
	}
	for deadline := time.Now().Add(5 * time.Second); Logs() == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the store did not open")
		}
	}
	assert.Equal(t, followed(), 0)

	// after a Docker restart the watch missed its start: only the list knows it runs
	docker.mu.Lock()
	docker.list = []container.Summary{{ID: testID, Names: []string{"/plex"}, Labels: testLabels}}
	docker.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); followed() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the running container was not followed")
		}
	}
}

func TestRecordAppEventIsAnOperationEvent(t *testing.T) {
	s := openLogs(t)
	RecordAppEvent("plex", "updated", "a:1", "a:2", "")

	got, err := s.Events(logstore.EventQuery{})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].Kind, "updated")
	assert.Equal(t, got[0].By, "operation")
	assert.Equal(t, got[0].App, "plex")
	assert.Equal(t, got[0].FromImage, "a:1")
	assert.Equal(t, got[0].ToImage, "a:2")

	SetLogs(nil)
	RecordAppEvent("plex", "updated", "a:1", "a:2", "") // no store: nothing, and no panic
}

func TestMainImageIsTheXCasaosMainService(t *testing.T) {
	services := types.Services{"web": {Name: "web", Image: "nginx:1.27"}, "db": {Name: "db", Image: "postgres:17"}}
	a := &ComposeApp{Services: services, Extensions: map[string]interface{}{common.ComposeExtensionNameXCasaOS: map[string]interface{}{"main": "web"}}}
	assert.Equal(t, mainImage(a), "nginx:1.27")
	assert.Equal(t, mainImage(&ComposeApp{Services: services}), "")
}

func TestApplyLogsSettingsValidatesAndPersists(t *testing.T) {
	saved := *config.AppInfo
	cfg, path := config.Cfg, config.ConfigFilePath
	config.Cfg, config.ConfigFilePath = ini.Empty(), filepath.Join(t.TempDir(), "app-management.conf")
	t.Cleanup(func() { *config.AppInfo, config.Cfg, config.ConfigFilePath = saved, cfg, path })

	err := ApplyLogsSettings(0, 512, true)
	assert.ErrorContains(t, err, "1 to 3650")
	assert.Assert(t, errors.Is(err, ErrLogsSettings))
	assert.ErrorContains(t, ApplyLogsSettings(10, 63, true), "64 to 1048576")

	assert.NilError(t, ApplyLogsSettings(10, 512, true))
	assert.Equal(t, config.AppInfo.LogsRetentionDays, 10)
	assert.Equal(t, config.AppInfo.LogsMaxSizeMB, 512)
	assert.Equal(t, config.AppInfo.LogsIncludeOthers, true)
	b, err := os.ReadFile(config.ConfigFilePath)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), "LogsRetentionDays = 10"), string(b))

	got := GetLogsSettings()
	assert.Equal(t, got.RetentionDays, 10)
	assert.Equal(t, got.IncludeOthers, true)

	config.ConfigFilePath = t.TempDir() // a directory: the save fails and nothing changes
	assert.Assert(t, ApplyLogsSettings(20, 1024, false) != nil)
	assert.Equal(t, GetLogsSettings(), got)
}
