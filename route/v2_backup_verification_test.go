package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/external"
)

func TestVerificationRoutesRequireAuthenticationAndValidateRequests(t *testing.T) {
	runtime := t.TempDir()
	previousRuntime, previousLog := config.CommonInfo.RuntimePath, service.BackupVerificationLogPath
	config.CommonInfo.RuntimePath = runtime
	service.BackupVerificationLogPath = filepath.Join(t.TempDir(), "verifications.json")
	t.Cleanup(func() {
		config.CommonInfo.RuntimePath = previousRuntime
		service.BackupVerificationLogPath = previousLog
	})
	if err := os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("verification-test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	router := InitV2Router()
	for _, test := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v2/app_management/backup/verifications", ""},
		{http.MethodPost, "/v2/app_management/backup/verifications", `{"destination":"offsite","app":"demo","stamp":"2026-10-04T12-00-00Z"}`},
		{http.MethodPost, "/v2/app_management/backup/verifications/check-1/cancel", ""},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.RemoteAddr = "127.0.0.1:40000"
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s: %d %s", test.method, test.path, recorder.Code, recorder.Body.String())
		}
	}
	if _, err := os.Stat(service.BackupVerificationLogPath); !os.IsNotExist(err) {
		t.Fatalf("unauthenticated requests must not create a job: %v", err)
	}
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/v2/app_management/backup/verifications", "", http.StatusOK},
		{http.MethodPost, "/v2/app_management/backup/verifications", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/v2/app_management/backup/verifications", `{"destination":"offsite","app":"../escape","stamp":"now"}`, http.StatusBadRequest},
		{http.MethodPost, "/v2/app_management/backup/verifications/missing/cancel", "", http.StatusNotFound},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.RemoteAddr = "127.0.0.1:40000"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Internal verification-test-only")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Fatalf("authenticated %s %s: %d %s", test.method, test.path, recorder.Code, recorder.Body.String())
		}
	}
}
