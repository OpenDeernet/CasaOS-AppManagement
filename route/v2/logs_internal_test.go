package v2

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/logstore"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"gopkg.in/ini.v1"
	"gotest.tools/v3/assert"
)

// The compose list knows only compose stacks: a v1 app is known installed by its followed container.
// Here and not in route/v2_logs_test.go: the route's list needs Docker.
func TestAnAppIsUninstalledOnlyWhenNeitherListedNorFollowed(t *testing.T) {
	summary := func(app, container string) logstore.AppSummary {
		return logstore.AppSummary{App: app, Services: []logstore.ServiceSummary{{Service: "main", Container: container}}}
	}
	summaries := []logstore.AppSummary{summary("_other", "aaaaaaaaaaaa"), summary("gone", "bbbbbbbbbbbb"), summary("plex", "cccccccccccc"), summary("v1app", "dddddddddddd")}
	installed := map[string]*service.ComposeApp{"plex": {}}
	followed := map[string]bool{"dddddddddddd": true}

	uninstalled := map[string]bool{}
	for _, a := range logsAppsOf(summaries, installed, followed) {
		uninstalled[a.App] = a.Uninstalled
	}
	assert.DeepEqual(t, uninstalled, map[string]bool{"_other": false, "gone": true, "plex": false, "v1app": false})

	for _, a := range logsAppsOf(summaries, nil, nil) {
		assert.Assert(t, !a.Uninstalled, "%s: without the list nothing is uninstalled", a.App)
	}
}

// The router's validator refuses what the service refuses, so the handler is called
// directly to check how it answers each kind of error.
func TestASettingsValueOutOfRangeIsA400AndASaveFailureA500(t *testing.T) {
	logger.LogInitConsoleOnly()
	saved, cfg, path := *config.AppInfo, config.Cfg, config.ConfigFilePath
	config.Cfg, config.ConfigFilePath = ini.Empty(), filepath.Join(t.TempDir(), "app-management.conf")
	t.Cleanup(func() { *config.AppInfo, config.Cfg, config.ConfigFilePath = saved, cfg, path })

	put := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		recorder := httptest.NewRecorder()
		assert.NilError(t, (&AppManagement{}).SetLogsSettings(echo.New().NewContext(request, recorder)))
		return recorder
	}

	recorder := put(`{"retention_days":0,"max_size_mb":512,"include_others":false}`)
	assert.Equal(t, recorder.Code, http.StatusBadRequest)
	assert.Assert(t, strings.Contains(recorder.Body.String(), "the retention must be 1 to 3650 days, not 0"), recorder.Body.String())

	config.ConfigFilePath = t.TempDir() // a directory: the save fails
	recorder = put(`{"retention_days":10,"max_size_mb":512,"include_others":false}`)
	assert.Equal(t, recorder.Code, http.StatusInternalServerError)
	assert.Equal(t, strings.TrimSpace(recorder.Body.String()), `{"message":"the settings could not be saved"}`)
}
