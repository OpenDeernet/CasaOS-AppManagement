package v2

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
	"gotest.tools/v3/assert"
)

func force(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/v2/app_management/backup/schedules/run", strings.NewReader(body))
	if body != "" {
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	recorder := httptest.NewRecorder()
	assert.NilError(t, (&AppManagement{}).ForceBackupSchedules(echo.New().NewContext(request, recorder)))

	return recorder
}

func schedulesIn(t *testing.T, schedules []service.BackupSchedule) {
	t.Helper()

	dir := t.TempDir()
	wasSchedules, wasLog := service.BackupSchedulePath, service.BackupLogPath
	service.BackupSchedulePath = filepath.Join(dir, "schedules.json")
	service.BackupLogPath = filepath.Join(dir, "runs.json")
	t.Cleanup(func() { service.BackupSchedulePath, service.BackupLogPath = wasSchedules, wasLog })

	assert.NilError(t, service.SaveBackupSchedules(schedules))
}

func TestForcingSchedulesAnswersWhatHappened(t *testing.T) {
	schedulesIn(t, []service.BackupSchedule{{
		App: "nextcloud", Destination: "offsite", Every: service.ScheduleDaily, At: "03:00", Enabled: false,
	}})

	for _, bad := range []string{`{"app":""}`, `{"destination":""}`, `[not json`} {
		assert.Equal(t, force(t, bad).Code, http.StatusBadRequest, bad)
	}

	assert.Equal(t, force(t, `{"app":"immich"}`).Code, http.StatusNotFound)

	// nothing enabled: a whole-box force finds nothing to run
	assert.Equal(t, force(t, "").Code, http.StatusNotFound)
	assert.Equal(t, force(t, `{}`).Code, http.StatusNotFound)

	// a filter that finds only a switched-off schedule: 409, with the reason
	answer := force(t, `{"app":"nextcloud"}`)
	assert.Equal(t, answer.Code, http.StatusConflict)
	assert.Equal(t, strings.TrimSpace(answer.Body.String()),
		`{"data":{"refused":[{"app":"nextcloud","destination":"offsite","reason":"this schedule is switched off"}],"started":[]},`+
			`"message":"nothing started: nextcloud to offsite: this schedule is switched off"}`)
}

func TestTheOperationsSayWhenAnUpgradeIsOpen(t *testing.T) {
	was := osUpgradeOpen
	t.Cleanup(func() { osUpgradeOpen = was })

	for _, open := range []bool{false, true} {
		osUpgradeOpen = func() (bool, error) { return open, nil }

		recorder := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), recorder)
		assert.NilError(t, (&AppManagement{}).AppOperations(ctx))
		assert.Assert(t, strings.Contains(recorder.Body.String(), map[bool]string{
			false: `"os_upgrade_open":false`, true: `"os_upgrade_open":true`,
		}[open]), recorder.Body.String())
	}
}
