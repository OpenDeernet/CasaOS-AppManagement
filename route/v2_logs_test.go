package route

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/logstore"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/gorilla/websocket"
	"gopkg.in/ini.v1"
	"gotest.tools/v3/assert"
)

// testUserKey is the user-service's key for every test of the package: external caches
// the key it fetched for 10 s, so a second key would be checked against the first.
var testUserKey = sync.OnceValues(func() (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	private, public, err := jwt.GenerateKeyPair()
	if err != nil {
		panic(err)
	}
	return private, public
})

var logsDay = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

type logsRig struct {
	t      *testing.T
	router http.Handler
	store  *logstore.Store
	token  string // a valid user JWT
}

func newLogsRig(t *testing.T) *logsRig {
	t.Helper()
	logger.LogInitConsoleOnly()
	runtime := t.TempDir()
	previous := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	assert.NilError(t, os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("s3cret"), 0o600))

	saved, cfg, path := *config.AppInfo, config.Cfg, config.ConfigFilePath
	config.Cfg, config.ConfigFilePath = ini.Empty(), filepath.Join(t.TempDir(), "app-management.conf")
	t.Cleanup(func() {
		config.CommonInfo.RuntimePath = previous
		*config.AppInfo, config.Cfg, config.ConfigFilePath = saved, cfg, path
	})

	// a user-service that hands out the key, so a real token can be checked
	private, public := testUserKey()
	jwks, err := jwt.GenerateJwksJSON(public)
	assert.NilError(t, err)
	users := httptest.NewServer(jwt.JWKSHandler(jwks))
	t.Cleanup(users.Close)
	assert.NilError(t, os.WriteFile(filepath.Join(runtime, external.UserServiceAddressFilename), []byte(users.URL), 0o600))
	token, err := jwt.GetAccessToken("admin", private, 1)
	assert.NilError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	store, err := logstore.Open(ctx, t.TempDir())
	assert.NilError(t, err)
	service.SetLogs(store)
	t.Cleanup(func() { service.SetLogs(nil); _ = store.Close(); cancel() })
	return &logsRig{t: t, router: InitV2Router(), store: store, token: token}
}

func (r *logsRig) send(method, path, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	request := httptest.NewRequest(method, "/v2/app_management"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:40000"
	request.Header.Set("Authorization", "Internal s3cret")
	recorder := httptest.NewRecorder()
	r.router.ServeHTTP(recorder, request)
	return recorder
}

// get decodes the body's data into out and returns the recorder.
func (r *logsRig) get(path string, status int, out any) *httptest.ResponseRecorder {
	r.t.Helper()
	recorder := r.send(http.MethodGet, path, "")
	assert.Equal(r.t, recorder.Code, status, recorder.Body.String())
	if out != nil {
		var body struct{ Data json.RawMessage }
		assert.NilError(r.t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
		assert.NilError(r.t, json.Unmarshal(body.Data, out), recorder.Body.String())
	}
	return recorder
}

func (r *logsRig) message(recorder *httptest.ResponseRecorder) string {
	r.t.Helper()
	var body struct{ Message string }
	assert.NilError(r.t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	return body.Message
}

// write writes the lines to the app service's container, one millisecond apart from at, and flushes them.
func (r *logsRig) write(app, svc, container string, at time.Time, texts ...string) {
	r.t.Helper()
	w := r.store.Writer(app, svc, container)
	for i, text := range texts {
		assert.NilError(r.t, w.Write(at.Add(time.Duration(i)*time.Millisecond), "o", text))
	}
	assert.NilError(r.t, w.Flush())
}

type logsPage struct {
	Lines  []logstore.Line
	Events []logstore.Event
	Next   *string
}

func texts(lines []logstore.Line) []string {
	out := []string{}
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

func TestTheLogsRoutesNeedTheToken(t *testing.T) {
	r := newLogsRig(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/logs"},
		{http.MethodGet, "/logs/apps"},
		{http.MethodGet, "/logs/events"},
		{http.MethodGet, "/logs/export"},
		{http.MethodGet, "/logs/settings"},
		{http.MethodPut, "/logs/settings"},
	} {
		request := httptest.NewRequest(route.method, "/v2/app_management"+route.path, strings.NewReader(`{"retention_days":10,"max_size_mb":512,"include_others":false}`))
		request.RemoteAddr = "203.0.113.5:40000"
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		r.router.ServeHTTP(recorder, request)
		assert.Equal(t, recorder.Code, http.StatusUnauthorized, "%s %s", route.method, route.path)
	}
}

func TestTheStreamTakesTheTokenAsAQueryParameter(t *testing.T) {
	r := newLogsRig(t)
	for _, c := range []struct {
		path   string
		status int
	}{
		{"/logs/stream?token=bad", http.StatusUnauthorized},
		{"/logs/stream", http.StatusUnauthorized},
		// a good token, but only the stream takes it from the query
		{"/logs?token=" + r.token, http.StatusUnauthorized},
		{"/logs/apps?token=" + r.token, http.StatusUnauthorized},
		{"/logs/export?token=" + r.token, http.StatusUnauthorized},
		{"/logs/settings?token=" + r.token, http.StatusUnauthorized},
		// the token passed: no WebSocket handshake, so the upgrade is refused
		{"/logs/stream?token=" + r.token, http.StatusBadRequest},
	} {
		request := httptest.NewRequest(http.MethodGet, "/v2/app_management"+c.path, nil)
		request.RemoteAddr = "203.0.113.5:40000"
		recorder := httptest.NewRecorder()
		r.router.ServeHTTP(recorder, request)
		assert.Equal(t, recorder.Code, c.status, "%s: %s", c.path, recorder.Body.String())
	}
}

func TestHistoryHasTheContractShape(t *testing.T) {
	r := newLogsRig(t)
	r.write("plex", "plex", "aaaaaaaaaaaa", logsDay, "one", "two", "three")
	assert.NilError(t, r.store.Record(logstore.Event{At: logsDay.Add(time.Second), App: "plex", Service: "plex", Container: "aaaaaaaaaaaa", Kind: "started", By: "docker"}))

	var raw map[string]json.RawMessage
	r.get("/logs?app=plex&limit=2", http.StatusOK, &raw)
	assert.Equal(t, len(raw), 3, "lines, events, next")
	var page logsPage
	r.get("/logs?app=plex&limit=2", http.StatusOK, &page)
	assert.DeepEqual(t, texts(page.Lines), []string{"three", "two"})
	assert.Equal(t, page.Lines[0].App, "plex")
	assert.Equal(t, page.Lines[0].Container, "aaaaaaaaaaaa")
	assert.Equal(t, page.Lines[0].Stream, "o")
	assert.Equal(t, len(page.Events), 1)
	assert.Equal(t, page.Events[0].Kind, "started")
	assert.Assert(t, page.Next != nil && *page.Next != "")

	var older logsPage
	r.get("/logs?app=plex&limit=2&before="+url.QueryEscape(*page.Next), http.StatusOK, &older)
	assert.DeepEqual(t, texts(older.Lines), []string{"one"})
	assert.Assert(t, older.Next != nil, "next is always there")
	assert.Equal(t, *older.Next, "")
}

func TestSinceAndBeforeTogetherIsA400(t *testing.T) {
	r := newLogsRig(t)
	r.write("plex", "plex", "aaaaaaaaaaaa", logsDay, "one", "two")
	var page logsPage
	r.get("/logs?limit=1", http.StatusOK, &page)
	cursor := url.QueryEscape(*page.Next)

	recorder := r.get("/logs?since="+cursor+"&before="+cursor, http.StatusBadRequest, nil)
	assert.Equal(t, r.message(recorder), logstore.ErrBothDirections.Error())
	recorder = r.get("/logs?before=nonsense", http.StatusBadRequest, nil)
	assert.Assert(t, strings.Contains(r.message(recorder), "cursor"), r.message(recorder))
}

func TestABadRegexIsA400WithASentence(t *testing.T) {
	r := newLogsRig(t)
	recorder := r.get("/logs?q=%28&regex=true", http.StatusBadRequest, nil)
	assert.Assert(t, strings.Contains(r.message(recorder), "regex"), r.message(recorder))
	// without regex the same text is searched literally
	r.get("/logs?q=%28", http.StatusOK, nil)
}

func TestExportIsTheDayFileByteForByte(t *testing.T) {
	r := newLogsRig(t)
	r.write("plex", "plex", "aaaaaaaaaaaa", logsDay, "one", "two", "three")
	day, err := os.ReadFile(logstore.DayFile(r.store.Root, "plex", "plex", "aaaaaaaaaaaa", logsDay))
	assert.NilError(t, err)

	recorder := r.get("/logs/export?app=plex", http.StatusOK, nil)
	assert.Equal(t, recorder.Body.String(), string(day))
	assert.Equal(t, recorder.Header().Get("Content-Type"), "text/plain; charset=utf-8")
	assert.Equal(t, recorder.Header().Get("Content-Disposition"), `attachment; filename="recasaos-logs-plex-start-now.log"`)

	r.write("plex", "db", "bbbbbbbbbbbb", logsDay.Add(500*time.Microsecond), "db one")
	recorder = r.get("/logs/export?app=plex&service=plex&service=db&until=2026-11-01T11:00:00Z", http.StatusOK, nil)
	at := func(d time.Duration) string { return logsDay.Add(d).Format(time.RFC3339Nano) }
	assert.Equal(t, recorder.Body.String(), ""+
		"[plex/plex] "+at(0)+" o one\n"+
		"[plex/db] "+at(500*time.Microsecond)+" o db one\n"+
		"[plex/plex] "+at(time.Millisecond)+" o two\n"+
		"[plex/plex] "+at(2*time.Millisecond)+" o three\n")
	assert.Equal(t, recorder.Header().Get("Content-Disposition"), `attachment; filename="recasaos-logs-plex-start-2026-11-01T11-00-00Z.log"`)

	var page logsPage
	r.get("/logs?app=plex&service=plex&limit=3", http.StatusOK, &page) // the cursor of "one"
	recorder = r.get("/logs/export?service=plex&since="+url.QueryEscape(*page.Next), http.StatusOK, nil)
	assert.Equal(t, recorder.Body.String(), at(time.Millisecond)+" o two\n"+at(2*time.Millisecond)+" o three\n")
	assert.Equal(t, recorder.Header().Get("Content-Disposition"), `attachment; filename="recasaos-logs-all-2026-11-01T10-00-00Z-now.log"`)
}

func TestStreamAndHistoryAgree(t *testing.T) {
	r := newLogsRig(t)
	r.write("plex", "plex", "aaaaaaaaaaaa", logsDay, "before")
	var page logsPage
	r.get("/logs?limit=1", http.StatusOK, &page)

	server := httptest.NewServer(r.router)
	t.Cleanup(server.Close)
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/v2/app_management/logs/stream?app=plex&token=" + r.token
	// a browser asks for gzip on the handshake too
	conn, response, err := websocket.DefaultDialer.Dial(address, http.Header{"Accept-Encoding": {"gzip"}})
	assert.NilError(t, err)
	defer conn.Close()
	assert.Equal(t, response.StatusCode, http.StatusSwitchingProtocols)

	r.write("other", "web", "cccccccccccc", logsDay.Add(time.Second), "not plex")
	r.write("plex", "plex", "aaaaaaaaaaaa", logsDay.Add(time.Second), "1", "2", "3", "4", "5")
	var streamed []logstore.Line
	assert.NilError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for len(streamed) < 5 {
		var m logstore.Message
		assert.NilError(t, conn.ReadJSON(&m))
		assert.Equal(t, m.T, "line")
		streamed = append(streamed, *m.Line)
	}

	var history logsPage
	r.get("/logs?app=plex&since="+url.QueryEscape(*page.Next), http.StatusOK, &history)
	for i := range history.Lines { // only history lines say where they are
		assert.Equal(t, streamed[i].Cursor, "")
		assert.Assert(t, history.Lines[i].Cursor != "")
		history.Lines[i].Cursor = ""
	}
	assert.DeepEqual(t, streamed, history.Lines)
}

func TestAHistoryLinesCursorIsASince(t *testing.T) {
	r := newLogsRig(t)
	r.write("plex", "plex", "aaaaaaaaaaaa", logsDay, "one", "two", "three")
	var page logsPage
	r.get("/logs?app=plex", http.StatusOK, &page)
	assert.DeepEqual(t, texts(page.Lines), []string{"three", "two", "one"})
	var after logsPage
	r.get("/logs?app=plex&since="+url.QueryEscape(page.Lines[2].Cursor), http.StatusOK, &after)
	assert.DeepEqual(t, texts(after.Lines), []string{"two", "three"})
}

func TestTheStreamEndsWhenTheClientGoes(t *testing.T) {
	r := newLogsRig(t)
	server := httptest.NewServer(r.router)
	t.Cleanup(server.Close)
	var conns []*websocket.Conn
	for range 3 {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v2/app_management/logs/stream?token="+r.token, nil)
		assert.NilError(t, err)
		conns = append(conns, conn)
	}
	waitFor(t, func() bool { return logsGoroutines() == 9 }, "a handler, a reader and a hub drain per client")
	for _, conn := range conns {
		assert.NilError(t, conn.Close())
	}
	waitFor(t, func() bool { return logsGoroutines() == 0 }, "nothing is left once the clients are gone")
}

// logsGoroutines counts the goroutines of the stream handler and of the hub's queues.
func logsGoroutines() int {
	buf := make([]byte, 4<<20)
	n := 0
	for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, "(*AppManagement).LogsStream") || strings.Contains(g, "(*Hub).drain") {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s (%d goroutines)", what, logsGoroutines())
		}
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	r := newLogsRig(t)
	var settings map[string]any
	r.get("/logs/settings", http.StatusOK, &settings)
	assert.Equal(t, settings["retention_days"], 7.0)
	assert.Equal(t, settings["max_size_mb"], 2048.0)
	assert.Equal(t, settings["include_others"], false)
	assert.Equal(t, settings["path"], config.AppInfo.LogsPath)
	assert.Equal(t, settings["oldest_at"], nil)
	_, ok := settings["used_mb"]
	assert.Assert(t, ok)

	recorder := r.send(http.MethodPut, "/logs/settings", `{"retention_days":0,"max_size_mb":512,"include_others":true}`)
	assert.Equal(t, recorder.Code, http.StatusBadRequest, recorder.Body.String())

	recorder = r.send(http.MethodPut, "/logs/settings", `{"retention_days":10,"max_size_mb":512,"include_others":true}`)
	assert.Equal(t, recorder.Code, http.StatusOK, recorder.Body.String())
	r.get("/logs/settings", http.StatusOK, &settings)
	assert.Equal(t, settings["retention_days"], 10.0)
	assert.Equal(t, settings["max_size_mb"], 512.0)
	assert.Equal(t, settings["include_others"], true)
}

func TestTheAppsListTheirServices(t *testing.T) {
	r := newLogsRig(t)
	r.write("plex", "plex", "aaaaaaaaaaaa", logsDay, "one")
	r.write("_other", "web", "cccccccccccc", logsDay, "one")
	var apps []map[string]json.RawMessage
	r.get("/logs/apps", http.StatusOK, &apps)
	assert.Equal(t, len(apps), 2)
	assert.Equal(t, string(apps[0]["app"]), `"_other"`)
	assert.Equal(t, string(apps[0]["uninstalled"]), "false", "_other is never uninstalled")
	assert.Equal(t, string(apps[1]["app"]), `"plex"`)
	assert.Equal(t, string(apps[1]["title"]), "null")
	assert.Equal(t, string(apps[1]["icon"]), "null")
	var services []logstore.ServiceSummary
	assert.NilError(t, json.Unmarshal(apps[1]["services"], &services))
	assert.Equal(t, len(services), 1)
	assert.Equal(t, services[0].Container, "aaaaaaaaaaaa")
	assert.Equal(t, services[0].Running, false)
}

func TestTheEventsAreFiltered(t *testing.T) {
	r := newLogsRig(t)
	for i, e := range []struct{ app, kind string }{{"plex", "started"}, {"plex", "died"}, {"db", "died"}} {
		assert.NilError(t, r.store.Record(logstore.Event{At: logsDay.Add(time.Duration(i) * time.Second), App: e.app, Kind: e.kind, By: "docker"}))
	}
	var events []logstore.Event
	r.get("/logs/events?app=plex&app=db&kind=died", http.StatusOK, &events)
	assert.Equal(t, len(events), 2)
	r.get("/logs/events?app=plex&until="+url.QueryEscape(logsDay.Add(500*time.Millisecond).Format(time.RFC3339Nano)), http.StatusOK, &events)
	assert.Equal(t, len(events), 1)
	assert.Equal(t, events[0].Kind, "started")
}

func TestWithoutTheStoreTheRoutesAnswer503(t *testing.T) {
	r := newLogsRig(t)
	service.SetLogs(nil)
	for _, path := range []string{"/logs", "/logs/apps", "/logs/events", "/logs/export", "/logs/stream"} {
		recorder := r.get(path, http.StatusServiceUnavailable, nil)
		assert.Equal(t, r.message(recorder), "the logs store is not open", path)
	}
	r.get("/logs/settings", http.StatusOK, nil)
}
