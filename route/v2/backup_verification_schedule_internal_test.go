package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
)

func verificationScheduleCall(t *testing.T, method, body string) (int, codegen.BackupVerificationScheduleOK) {
	t.Helper()
	request := httptest.NewRequest(method, "/", strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(request, recorder)
	a := &AppManagement{}
	var err error
	if method == http.MethodGet {
		err = a.BackupVerificationSchedule(ctx)
	} else {
		err = a.SetBackupVerificationSchedule(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	var out codegen.BackupVerificationScheduleOK
	_ = json.Unmarshal(recorder.Body.Bytes(), &out)
	return recorder.Code, out
}

func TestTheVerificationScheduleRoutes(t *testing.T) {
	was := service.VerificationSchedulePath
	service.VerificationSchedulePath = filepath.Join(t.TempDir(), "verification-schedule.json")
	t.Cleanup(func() { service.VerificationSchedulePath = was })

	code, out := verificationScheduleCall(t, http.MethodGet, "")
	if code != http.StatusOK || out.Data == nil || out.Data.Enabled || out.Data.EveryDays != 7 || out.Data.At != "03:00" || out.Data.LastRun != nil {
		t.Fatalf("the default, off: %d %+v", code, out.Data)
	}

	for _, body := range []string{
		`{"enabled":true,"every_days":0,"at":"03:00"}`,
		`{"enabled":true,"every_days":31,"at":"03:00"}`,
		`{"enabled":true,"every_days":7,"at":"24:00"}`,
		`{"enabled":true,"every_days":7}`,
		`{"enabled":true,"every_days":7,"at":"03:00","evry_days":3}`,
		`not json`,
		`{"enabled":true,"every_days":7,"at":"03:00"}xyz`,
		`{"enabled":true,"every_days":7,"at":"03:00"}{}`,
	} {
		if code, _ := verificationScheduleCall(t, http.MethodPut, body); code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want 400", body, code)
		}
	}

	code, out = verificationScheduleCall(t, http.MethodPut, `{"enabled":false,"every_days":3,"at":"04:30","last_run":"2020-01-01T00:00:00Z"}`)
	if code != http.StatusOK || out.Data == nil || out.Data.EveryDays != 3 || out.Data.At != "04:30" || out.Data.LastRun != nil {
		t.Fatalf("saved, the client's last_run ignored: %d %+v", code, out.Data)
	}

	code, out = verificationScheduleCall(t, http.MethodPut, `{"enabled":true,"every_days":3,"at":"04:30"}`)
	if code != http.StatusOK || out.Data == nil || !out.Data.Enabled || out.Data.LastRun == nil {
		t.Fatalf("switched on, the slot passed is spent and returned: %d %+v", code, out.Data)
	}

	if code, got := verificationScheduleCall(t, http.MethodGet, ""); code != http.StatusOK || got.Data == nil || !got.Data.LastRun.Equal(*out.Data.LastRun) {
		t.Fatalf("GET returns what PUT stored: %d %+v", code, got.Data)
	}
}
