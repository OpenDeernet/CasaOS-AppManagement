package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/logstore"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"go.uber.org/zap"
)

// The logs collector: every app container's lines and events, kept on /DATA by logstore.
// One follower goroutine per container reads Docker's log stream from where the files
// stop; the Docker watch hands it every container event, and the operations record theirs.

// logsDocker is what the collector needs of Docker; *client.Client satisfies it.
type logsDocker interface {
	ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error)
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerLogs(ctx context.Context, id string, o container.LogsOptions) (io.ReadCloser, error)
}

// logsRetry is how long the collector waits before trying to create its root again; a var for the tests.
var logsRetry = 30 * time.Second

// logsTick is how often the collector checks the cap and looks for running containers it does
// not follow (a Docker restart's start events are missed while the watch reconnects); a var for the tests.
var logsTick = time.Minute

// dieDrain bounds how long a die waits for its container's stream to end, so that the died
// event follows the container's last lines on disk; past it the event is recorded anyway and
// the follower still drains. A var for the tests.
var dieDrain = 2 * time.Second

// ErrLogsSettings wraps a settings value out of range: the caller's mistake, not a failure.
var ErrLogsSettings = errors.New("logs settings out of range")

var (
	logsStore atomic.Pointer[logstore.Store]

	logsMu     sync.Mutex // StartLogs and StopLogs, and the four below
	logsCancel context.CancelFunc
	logsDone   chan struct{}
	logsCtx    context.Context
	logsClient logsDocker

	logsSettingsMu sync.Mutex // the Logs* fields of config.AppInfo, which ApplyLogsSettings changes live

	followMu  sync.Mutex
	followers = map[string]*follower{} // by 12-char container id

	// set by StartLogs, cleared by StopLogs, called by the Docker watch's goroutine
	onDockerEvent    atomic.Pointer[func(events.Message)]
	onWatchPublished atomic.Pointer[func(published)]
)

type follower struct {
	cancel             context.CancelFunc
	done               chan struct{}
	app, service, name string
	again              bool // a start came while this run still drained: follow the next run too
}

// Logs is the open store, nil while it is not open (the logs routes answer 503).
func Logs() *logstore.Store { return logsStore.Load() }

// SetLogs puts s in place of the running store, for tests; nil means closed.
func SetLogs(s *logstore.Store) { logsStore.Store(s) }

// StartLogs returns at once: a goroutine opens the store, retrying while its root cannot
// be created, follows the running containers, and purges at open, daily and when over the cap.
func StartLogs(ctx context.Context, docker logsDocker) error {
	if docker == nil {
		return errors.New("logs: no Docker client")
	}
	logsMu.Lock()
	defer logsMu.Unlock()
	if logsCancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	onEvent := func(m events.Message) { logsOnEvent(ctx, docker, m) }
	onPublished := logsOnPublished
	onDockerEvent.Store(&onEvent)
	onWatchPublished.Store(&onPublished)
	go func() {
		defer close(done)
		runLogs(ctx, docker)
	}()
	logsCancel, logsDone, logsCtx, logsClient = cancel, done, ctx, docker
	return nil
}

// StopLogs stops the followers and closes the store; it does nothing when the collector is not running.
func StopLogs() {
	logsMu.Lock()
	defer logsMu.Unlock()
	if logsCancel == nil {
		return
	}
	onDockerEvent.Store(nil)
	onWatchPublished.Store(nil)
	logsCancel() // before stopFollowers: follow starts nothing on a cancelled ctx
	<-logsDone
	stopFollowers(nil)
	if s := logsStore.Swap(nil); s != nil {
		if err := s.Close(); err != nil {
			logger.Error("the logs could not all be written on stop", zap.Error(err))
		}
	}
	logsCancel, logsDone, logsCtx, logsClient = nil, nil, nil, nil
}

func runLogs(ctx context.Context, docker logsDocker) {
	var store *logstore.Store
	for {
		s, err := logstore.Open(ctx, config.AppInfo.LogsPath)
		if err == nil {
			store = s
			break
		}
		logger.Error("the logs directory cannot be created, trying again", zap.Error(err), zap.Duration("in", logsRetry))
		select {
		case <-ctx.Done():
			return
		case <-time.After(logsRetry):
		}
	}
	store.OnError = func(err error) { logger.Error("a container's logs could not be written", zap.Error(err)) }
	logsStore.Store(store)

	followRunning(ctx, docker)
	purgeLogs(store)

	daily := time.NewTicker(24 * time.Hour)
	defer daily.Stop()
	minute := time.NewTicker(logsTick)
	defer minute.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-daily.C:
			purgeLogs(store)
		case <-minute.C:
			followRunning(ctx, docker) // follow starts nothing for a container already followed
			_, mb, _ := logsConfig()
			if used, _, err := store.Usage(); err == nil && used > int64(mb)<<20 {
				purgeLogs(store)
			}
		}
	}
}

func logsConfig() (days, mb int, others bool) {
	logsSettingsMu.Lock()
	defer logsSettingsMu.Unlock()
	return config.AppInfo.LogsRetentionDays, config.AppInfo.LogsMaxSizeMB, config.AppInfo.LogsIncludeOthers
}

func purgeLogs(store *logstore.Store) {
	days, mb, _ := logsConfig()
	if _, err := store.Purge(time.Now(), days, int64(mb)<<20); err != nil {
		logger.Error("the old logs could not all be removed", zap.Error(err))
	}
}

// followRunning follows every running container logTarget accepts.
func followRunning(ctx context.Context, docker logsDocker) {
	list, err := docker.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("the running containers could not be listed for their logs", zap.Error(err))
		}
		return
	}
	_, _, others := logsConfig()
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		if app, service, ok := logTarget(c.Labels, name, others); ok {
			follow(ctx, docker, c.ID, app, service, name, false)
		}
	}
}

// logTarget says under which app and service a container's lines are kept, and whether they are.
func logTarget(labels map[string]string, name string, includeOthers bool) (app, service string, ok bool) {
	name = strings.TrimPrefix(name, "/")
	switch {
	case labels[api.ProjectLabel] != "":
		// a one-off container is somebody's `docker compose run`, not the app
		if labels[api.OneoffLabel] == "True" {
			return "", "", false
		}
		return labels[api.ProjectLabel], labels[api.ServiceLabel], true
	case labels["casaos"] == "casaos":
		return name, "main", true
	case includeOthers:
		return "_other", name, true
	}
	return "", "", false
}

var eventKinds = map[events.Action]string{
	events.ActionCreate:                "created",
	events.ActionStart:                 "started",
	events.ActionStop:                  "stopped",
	events.ActionDie:                   "died",
	events.ActionOOM:                   "oom-killed",
	events.ActionHealthStatusUnhealthy: "unhealthy",
	events.ActionHealthStatusHealthy:   "healthy",
}

func logEventOf(m events.Message, app, service string, held bool) (logstore.Event, bool) {
	kind, ok := eventKinds[m.Action]
	if !ok {
		return logstore.Event{}, false
	}
	by := "docker"
	if held {
		by = "operation"
	}
	e := logstore.Event{At: time.Unix(0, m.TimeNano), App: app, Service: service, Container: shortID(m.Actor.ID), Kind: kind, By: by}
	if m.Action == events.ActionDie {
		e.ExitCode = m.Actor.Attributes["exitCode"]
	}
	return e, true
}

func shortID(id string) string { return id[:min(len(id), 12)] }

// logsOnEvent is the watch's hook: it records the container's event and starts or ends its follower.
func logsOnEvent(ctx context.Context, docker logsDocker, m events.Message) {
	store := Logs()
	if store == nil {
		return
	}
	_, _, others := logsConfig()
	name := m.Actor.Attributes["name"]
	app, service, ok := logTarget(m.Actor.Attributes, name, others)
	if !ok {
		return
	}
	id := shortID(m.Actor.ID)
	if m.Action == events.ActionDie {
		awaitFollower(id, dieDrain) // the follower closes, so flushes, its writer before it ends
	}
	if e, ok := logEventOf(m, app, service, held(app)); ok {
		if err := store.Record(e); err != nil {
			logger.Error("a container event could not be recorded", zap.Error(err))
		}
	}
	if m.Action == events.ActionStart {
		follow(ctx, docker, m.Actor.ID, app, service, name, true)
	}
}

// logsOnPublished records the watch's restart-loop decision.
func logsOnPublished(p published) {
	store := Logs()
	if store == nil || p.eventType.Name != common.EventTypeAppContainerRestarting.Name {
		return
	}
	app, name := p.properties[common.PropertyTypeAppName.Name], p.properties[common.PropertyTypeContainerName.Name]
	// the watch knows the container by name: its id when it is followed, its name otherwise
	e := logstore.Event{At: time.Now().UTC(), App: app, Container: name, Kind: "restarting", By: "docker"}
	followMu.Lock()
	for id, f := range followers {
		if f.app == app && f.name == name {
			e.Container, e.Service = id, f.service
		}
	}
	followMu.Unlock()
	if err := store.Record(e); err != nil {
		logger.Error("a container event could not be recorded", zap.Error(err))
	}
}

// RecordAppEvent records what an operation did to an app; nothing while the store is not open.
func RecordAppEvent(app, kind, fromImage, toImage, detail string) {
	store := Logs()
	if store == nil {
		return
	}
	e := logstore.Event{At: time.Now().UTC(), App: app, Kind: kind, FromImage: fromImage, ToImage: toImage, By: "operation", Detail: detail}
	if err := store.Record(e); err != nil {
		logger.Error("an app event could not be recorded", zap.Error(err), zap.String("app", app), zap.String("kind", kind))
	}
}

// mainImage is the image of the service x-casaos names main, "" when unknown.
func mainImage(a *ComposeApp) string {
	ex, ok := a.Extensions[common.ComposeExtensionNameXCasaOS]
	if !ok {
		return ""
	}
	var info codegen.ComposeAppStoreInfo
	if err := loader.Transform(ex, &info); err != nil || info.Main == nil {
		return ""
	}
	return a.Services[*info.Main].Image
}

type LogsSettings struct {
	RetentionDays int
	MaxSizeMB     int
	IncludeOthers bool
	UsedMB        float64
	OldestAt      *time.Time
	Path          string
}

func GetLogsSettings() LogsSettings {
	days, mb, others := logsConfig()
	s := LogsSettings{RetentionDays: days, MaxSizeMB: mb, IncludeOthers: others, Path: config.AppInfo.LogsPath}
	if store := Logs(); store != nil {
		if used, oldest, err := store.Usage(); err == nil {
			s.UsedMB = float64(used) / (1 << 20)
			if !oldest.IsZero() {
				s.OldestAt = &oldest
			}
		}
	}
	return s
}

// ApplyLogsSettings validates, saves to the config file, purges now, and starts or stops
// following the containers of no app without a restart.
func ApplyLogsSettings(retentionDays, maxSizeMB int, includeOthers bool) error {
	if retentionDays < 1 || retentionDays > 3650 {
		return fmt.Errorf("%w: the retention must be 1 to 3650 days, not %d", ErrLogsSettings, retentionDays)
	}
	if maxSizeMB < 64 || maxSizeMB > 1_048_576 {
		return fmt.Errorf("%w: the size cap must be 64 to 1048576 MB, not %d", ErrLogsSettings, maxSizeMB)
	}

	logsSettingsMu.Lock()
	days, mb, was := config.AppInfo.LogsRetentionDays, config.AppInfo.LogsMaxSizeMB, config.AppInfo.LogsIncludeOthers
	config.AppInfo.LogsRetentionDays, config.AppInfo.LogsMaxSizeMB, config.AppInfo.LogsIncludeOthers = retentionDays, maxSizeMB, includeOthers
	err := config.SaveSetup()
	if err != nil {
		config.AppInfo.LogsRetentionDays, config.AppInfo.LogsMaxSizeMB, config.AppInfo.LogsIncludeOthers = days, mb, was
	}
	logsSettingsMu.Unlock()
	if err != nil {
		return err
	}

	if store := Logs(); store != nil {
		purgeLogs(store)
	}
	switch {
	case includeOthers && !was:
		logsMu.Lock()
		ctx, docker := logsCtx, logsClient
		logsMu.Unlock()
		if docker != nil {
			followRunning(ctx, docker)
		}
	case !includeOthers && was:
		stopFollowers(func(f *follower) bool { return f.app == "_other" })
	}
	return nil
}
