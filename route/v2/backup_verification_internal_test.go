package v2

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
)

func TestEveryVerificationFieldReachesTheAPI(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, message := range []string{"", "the available backup could not be compared"} {
		record := service.BackupVerificationRecord{
			ID: "check-1", Destination: "offsite", App: "demo", Stamp: "2026-10-04T11-59-00Z",
			Status: "failed", StartedAt: now, FinishedAt: &now,
			CheckedOperations: 3, SkippedCount: 2, Files: 8, Bytes: 12345, Error: message,
		}
		asMap := func(value any) map[string]any {
			t.Helper()
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
		if got, want := asMap(backupVerificationOut(record)), asMap(record); !reflect.DeepEqual(got, want) {
			t.Fatalf("verification fields lost or invented: got %v, want %v", got, want)
		}
	}
}

func TestVerificationAPIErrorsPreserveStatusWithoutTransportDetails(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"invalid", fmt.Errorf("destination required: %w", service.ErrBackupVerificationInvalid), http.StatusBadRequest},
		{"restic", service.ErrBackupVerificationRestic, http.StatusBadRequest},
		{"busy", service.ErrBackupVerificationBusy, http.StatusConflict},
		{"needs recovery", service.ErrBackupVerificationNeedsRecovery, http.StatusConflict},
		{"finished", service.ErrBackupVerificationNotRunning, http.StatusConflict},
		{"missing", service.ErrBackupVerificationNotFound, http.StatusNotFound},
		{"transport", errors.New("https://example.invalid?access_token=do-not-return"), http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), recorder)
			if err := backupVerificationError(ctx, test.err); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.status || strings.Contains(recorder.Body.String(), "do-not-return") {
				t.Fatalf("unsafe or incorrect response: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestVerificationRejectsInvalidRequestsBeforeStartingAJob(t *testing.T) {
	for _, body := range []string{"not JSON", `{}`, `{"destination":"offsite","app":"../demo","stamp":"2026-10-04T12-00-00Z"}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/backup/verifications", strings.NewReader(body))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		ctx := echo.New().NewContext(request, recorder)
		if err := NewAppManagement().StartBackupVerification(ctx); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid request must not start a worker: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}
