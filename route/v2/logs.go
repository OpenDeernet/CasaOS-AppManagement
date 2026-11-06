package v2

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/logstore"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/utils"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

const (
	logsWriteWait = 10 * time.Second
	logsPing      = 30 * time.Second
	// a client that answers no ping for this long is gone
	logsPongWait = logsPing + logsWriteWait
	logsStamp    = "2006-01-02T15-04-05Z"
)

var logsUpgrader = websocket.Upgrader{
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
	CheckOrigin:      func(*http.Request) bool { return true },
	HandshakeTimeout: 5 * time.Second,
}

func logsMessage(ctx echo.Context, status int, message string) error {
	return ctx.JSON(status, codegen.BaseResponse{Message: &message})
}

func logsClosed(ctx echo.Context) error {
	return logsMessage(ctx, http.StatusServiceUnavailable, "the logs store is not open")
}

// logsFailure answers the client's own mistakes with their cause, anything else with a 500.
func logsFailure(ctx echo.Context, err error) error {
	if errors.Is(err, logstore.ErrBadFilter) || errors.Is(err, logstore.ErrBothDirections) {
		return logsMessage(ctx, http.StatusBadRequest, err.Error())
	}
	logger.Error("reading the logs failed", zap.Error(err))
	return logsMessage(ctx, http.StatusInternalServerError, "the logs could not be read")
}

// logsQueryOf reads the filter from the query itself: the generated params give each
// route its own enum types for the same values.
func logsQueryOf(ctx echo.Context, since *string, until *time.Time) (logstore.Query, error) {
	values := ctx.QueryParams()
	regex, _ := strconv.ParseBool(values.Get("regex")) // the validator has checked it
	q := logstore.Query{Filter: logstore.Filter{
		Apps: values["app"], Services: values["service"], Stream: values.Get("stream"),
		Level: values.Get("level"), Q: values.Get("q"), Regex: regex,
	}}
	if since != nil {
		c, err := logstore.ParseCursor(*since)
		if err != nil {
			return q, err
		}
		q.After = &c
	}
	if until != nil {
		q.Until = *until
	}
	return q, nil
}

type logsApp struct {
	logstore.AppSummary
	Title       map[string]string `json:"title"`
	Icon        *string           `json:"icon"`
	Uninstalled bool              `json:"uninstalled"`
}

// LogsApps lists the apps with logs on disk, with their title and icon when installed.
func (a *AppManagement) LogsApps(ctx echo.Context) error {
	store := service.Logs()
	if store == nil {
		return logsClosed(ctx)
	}
	summaries, err := store.Apps()
	if err != nil {
		return logsFailure(ctx, err)
	}
	// without the list nothing is called uninstalled: it is not known
	var installed map[string]*service.ComposeApp
	if service.MyService != nil {
		if installed, err = service.MyService.Compose().List(ctx.Request().Context()); err != nil {
			logger.Error("listing the apps for their logs failed", zap.Error(err))
			installed = nil
		}
	}
	return ctx.JSON(http.StatusOK, echo.Map{"data": logsAppsOf(summaries, installed, service.FollowedContainers())})
}

// logsAppsOf marks the running services, and the apps neither installed nor running as uninstalled;
// installed nil means the list is not known.
func logsAppsOf(summaries []logstore.AppSummary, installed map[string]*service.ComposeApp, followed map[string]bool) []logsApp {
	out := make([]logsApp, 0, len(summaries))
	for _, s := range summaries {
		app := logsApp{AppSummary: s}
		running := false
		for i := range app.Services {
			app.Services[i].Running = followed[app.Services[i].Container]
			running = running || app.Services[i].Running
		}
		if composeApp, ok := installed[s.App]; ok {
			if info, err := composeApp.StoreInfo(false); err == nil && info != nil {
				app.Title, app.Icon = info.Title, utils.Ptr(info.Icon)
			}
		} else {
			// the compose list knows no v1 app: one with a followed container is installed
			app.Uninstalled = installed != nil && s.App != "_other" && !running
		}
		out = append(out, app)
	}
	return out
}

// LogsQuery returns one page of history and the events of its window.
func (a *AppManagement) LogsQuery(ctx echo.Context, params codegen.LogsQueryParams) error {
	store := service.Logs()
	if store == nil {
		return logsClosed(ctx)
	}
	q, err := logsQueryOf(ctx, params.Since, params.Until)
	if err != nil {
		return logsFailure(ctx, err)
	}
	if params.Before != nil {
		c, err := logstore.ParseCursor(*params.Before)
		if err != nil {
			return logsFailure(ctx, err)
		}
		q.Before = &c
	}
	if params.Limit != nil {
		q.Limit = *params.Limit
	}
	page, err := store.Read(ctx.Request().Context(), q)
	if err != nil {
		return logsFailure(ctx, err)
	}
	// next always present, empty at the end: the contract's shape, not Page's omitempty
	return ctx.JSON(http.StatusOK, echo.Map{"data": echo.Map{"lines": page.Lines, "events": page.Events, "next": page.Next}})
}

// LogsEvents returns the matching events, newest first.
func (a *AppManagement) LogsEvents(ctx echo.Context, params codegen.LogsEventsParams) error {
	store := service.Logs()
	if store == nil {
		return logsClosed(ctx)
	}
	q := logstore.EventQuery{}
	if params.App != nil {
		q.Apps = *params.App
	}
	if params.Kind != nil {
		q.Kinds = *params.Kind
	}
	if params.Since != nil {
		q.Since = *params.Since
	}
	if params.Until != nil {
		q.Until = *params.Until
	}
	if params.Limit != nil {
		q.Limit = *params.Limit
	}
	events, err := store.Events(q)
	if err != nil {
		return logsFailure(ctx, err)
	}
	return ctx.JSON(http.StatusOK, echo.Map{"data": events})
}

// LogsExport streams the matching lines oldest first, as kept on disk, page by page.
func (a *AppManagement) LogsExport(ctx echo.Context, params codegen.LogsExportParams) error {
	store := service.Logs()
	if store == nil {
		return logsClosed(ctx)
	}
	q, err := logsQueryOf(ctx, params.Since, params.Until)
	if err != nil {
		return logsFailure(ctx, err)
	}
	if _, err := q.Compile(); err != nil { // refused before the 200 is sent
		return logsFailure(ctx, err)
	}
	from, to, name := "start", "now", "all"
	if q.After != nil {
		from = q.After.At.Format(logsStamp)
	} else {
		q.After = &logstore.Cursor{} // sorts before every line: a forwards walk from the first
	}
	if params.Until != nil {
		to = params.Until.UTC().Format(logsStamp)
	}
	if len(q.Apps) == 1 {
		name = logstore.SafeName(q.Apps[0])
	}
	several, err := logsSeveralServices(store, q.Filter)
	if err != nil {
		return logsFailure(ctx, err)
	}
	q.Limit = 1000

	res := ctx.Response()
	res.Header().Set(echo.HeaderContentType, "text/plain; charset=utf-8")
	res.Header().Set(echo.HeaderContentDisposition, fmt.Sprintf(`attachment; filename="recasaos-logs-%s-%s-%s.log"`, name, from, to))
	res.WriteHeader(http.StatusOK)
	w := bufio.NewWriter(res)
	for {
		page, err := store.Read(ctx.Request().Context(), q)
		if err != nil { // the status is sent: the body just ends
			logger.Error("exporting the logs stopped", zap.Error(err))
			return nil
		}
		for _, l := range page.Lines {
			if several {
				_, _ = w.WriteString("[" + l.App + "/" + l.Service + "] ")
			}
			_, _ = w.Write(logstore.FormatLine(l.At, l.Stream, l.Text))
		}
		if err := w.Flush(); err != nil {
			return nil
		}
		res.Flush()
		if page.Next == "" {
			return nil
		}
		next, err := logstore.ParseCursor(page.Next)
		if err != nil {
			return nil
		}
		q.After = &next
	}
}

// logsSeveralServices reports whether more than one service on disk matches the filter's
// names: their lines then carry where they come from.
func logsSeveralServices(store *logstore.Store, f logstore.Filter) (bool, error) {
	apps, err := store.Apps()
	if err != nil {
		return false, err
	}
	m, err := logstore.Filter{Apps: f.Apps, Services: f.Services}.Compile()
	if err != nil {
		return false, err
	}
	n := 0
	for _, app := range apps {
		for _, s := range app.Services {
			if m.Match(logstore.Line{App: app.App, Service: s.Service}) {
				n++
			}
		}
	}
	return n > 1, nil
}

// LogsStream sends the new lines and events as they come, one JSON message each, until the client goes.
func (a *AppManagement) LogsStream(ctx echo.Context, _ codegen.LogsStreamParams) error {
	store := service.Logs()
	if store == nil {
		return logsClosed(ctx)
	}
	q, _ := logsQueryOf(ctx, nil, nil)
	m, err := q.Compile()
	if err != nil {
		return logsFailure(ctx, err)
	}
	// subscribed before the handshake ends: a client misses nothing written once it is connected
	messages, unsubscribe := store.Subscribe(m)
	defer unsubscribe()
	conn, err := logsUpgrader.Upgrade(ctx.Response().Writer, ctx.Request(), nil)
	if err != nil {
		return nil // Upgrade has answered the client
	}
	defer conn.Close()

	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_ = conn.SetReadDeadline(time.Now().Add(logsPongWait))
		conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(logsPongWait)) })
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	ping := time.NewTicker(logsPing)
	defer ping.Stop()
	for {
		select {
		case <-gone:
			return nil
		case msg, ok := <-messages:
			if !ok {
				return nil
			}
			_ = conn.SetWriteDeadline(time.Now().Add(logsWriteWait))
			if err := conn.WriteJSON(msg); err != nil {
				return nil
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(logsWriteWait)); err != nil {
				return nil
			}
		}
	}
}

// GetLogsSettings returns the retention, the size cap, the use and where the logs are kept.
func (a *AppManagement) GetLogsSettings(ctx echo.Context) error {
	return logsSettings(ctx)
}

// SetLogsSettings saves the retention and size cap.
func (a *AppManagement) SetLogsSettings(ctx echo.Context) error {
	var in codegen.LogsSettingsInput
	if err := ctx.Bind(&in); err != nil {
		return logsMessage(ctx, http.StatusBadRequest, err.Error())
	}
	if err := service.ApplyLogsSettings(in.RetentionDays, in.MaxSizeMb, in.IncludeOthers); err != nil {
		if errors.Is(err, service.ErrLogsSettings) {
			return logsMessage(ctx, http.StatusBadRequest, err.Error())
		}
		logger.Error("saving the logs settings failed", zap.Error(err))
		return logsMessage(ctx, http.StatusInternalServerError, "the settings could not be saved")
	}
	return logsSettings(ctx)
}

func logsSettings(ctx echo.Context) error {
	s := service.GetLogsSettings()
	return ctx.JSON(http.StatusOK, echo.Map{"data": codegen.LogsSettings{
		RetentionDays: s.RetentionDays, MaxSizeMb: s.MaxSizeMB, IncludeOthers: s.IncludeOthers,
		UsedMb: s.UsedMB, OldestAt: s.OldestAt, Path: s.Path,
	}})
}
