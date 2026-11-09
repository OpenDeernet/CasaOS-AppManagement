package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"gotest.tools/v3/assert"
)

// The git routes go through the same router as every v2 route: its authentication and its
// validation of the request against the spec, which a route missing from the spec fails.
func TestTheGitRoutesAreServed(t *testing.T) {
	runtime := t.TempDir()
	previous := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	t.Cleanup(func() { config.CommonInfo.RuntimePath = previous })
	assert.NilError(t, os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("s3cret"), 0o600))

	router := InitV2Router()

	for _, call := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/v2/app_management/git", `{"name":"Not A Name","url":"https://example.invalid/r.git","access":"none"}`, http.StatusBadRequest},
		{http.MethodGet, "/v2/app_management/git/no-such-app", "", http.StatusNotFound},
		{http.MethodPut, "/v2/app_management/git/no-such-app", `{"auto_deploy":true}`, http.StatusNotFound},
		{http.MethodPut, "/v2/app_management/git/no-such-app", `{"webhook_enabled":true,"regenerate_webhook_secret":true}`, http.StatusNotFound},
		{http.MethodPut, "/v2/app_management/git/no-such-app", `{"webhook_enabled":"yes"}`, http.StatusBadRequest},
		{http.MethodPut, "/v2/app_management/git/no-such-app", `{"regenerate_webhook_secret":1}`, http.StatusBadRequest},
		{http.MethodPost, "/v2/app_management/git/no-such-app/check", "", http.StatusNotFound},
		{http.MethodPost, "/v2/app_management/git/no-such-app/deploy", "", http.StatusNotFound},
		{http.MethodPost, "/v2/app_management/git/no-such-app/deploy", `{"commit":"4f5f60c"}`, http.StatusBadRequest},
		{http.MethodPost, "/v2/app_management/git/no-such-app/deploy", `{"tag":"v1.0.0"}`, http.StatusNotFound},
		{http.MethodPost, "/v2/app_management/git/no-such-app/deploy", `{"tag":"v1.0.0","commit":"4f5f60c16eba0123456789abcdef0123456789ab"}`, http.StatusBadRequest},
		{http.MethodGet, "/v2/app_management/git/no-such-app/tags", "", http.StatusNotFound},
		{http.MethodPut, "/v2/app_management/git/no-such-app", `{"follow":"tags","tag_pattern":"v2.*","prereleases":true}`, http.StatusNotFound},
		{http.MethodPut, "/v2/app_management/git/no-such-app", `{"prereleases":"yes"}`, http.StatusBadRequest},
		{http.MethodPost, "/v2/app_management/git", `{"name":"Not A Name","url":"https://example.invalid/r.git","access":"none","follow":1}`, http.StatusBadRequest},
		{http.MethodDelete, "/v2/app_management/git/no-such-app", "", http.StatusNotFound},
	} {
		request := httptest.NewRequest(call.method, call.path, strings.NewReader(call.body))
		request.RemoteAddr = "127.0.0.1:40000"
		request.Header.Set("Authorization", "Internal s3cret")
		if call.body != "" {
			request.Header.Set("Content-Type", "application/json")
		}

		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)

		assert.Equal(t, recorder.Code, call.want, "%s %s: %s", call.method, call.path, recorder.Body.String())
	}
}

// A forge's delivery is the one request served without a token, and only that method on that
// path: every other method there, and every other route, still wants one.
func TestOnlyAGitWebhookIsServedWithoutAToken(t *testing.T) {
	runtime := t.TempDir()
	previous := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	t.Cleanup(func() { config.CommonInfo.RuntimePath = previous })
	assert.NilError(t, os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("s3cret"), 0o600))
	logger.LogInitConsoleOnly()

	router := InitV2Router()

	for _, call := range []struct {
		method, path, contentType, body string
		want                            int
		answer                          string
	}{
		// the forge's route answers, whatever the body: a 404 says it was reached
		{http.MethodPost, "/v2/app_management/git/no-such-app/webhook", "application/json", `{"ref":"refs/heads/main"}`, http.StatusNotFound, `{"message":"not found"}`},
		{http.MethodPost, "/v2/app_management/git/no-such-app/webhook", "application/x-www-form-urlencoded", "payload=%7B%22ref%22%3A%22refs%2Fheads%2Fmain%22%7D", http.StatusNotFound, `{"message":"not found"}`},
		{http.MethodPost, "/v2/app_management/git/no-such-app/webhook", "", "", http.StatusNotFound, `{"message":"not found"}`},
		// nothing is read from a caller before the app and its webhook are known to be there
		{http.MethodPost, "/v2/app_management/git/no-such-app/webhook", "application/json", strings.Repeat(" ", 5<<20+1), http.StatusNotFound, `{"message":"not found"}`},
		// everything else keeps the token
		{http.MethodGet, "/v2/app_management/git/no-such-app/webhook", "", "", http.StatusUnauthorized, ""},
		{http.MethodPut, "/v2/app_management/git/no-such-app/webhook", "", "", http.StatusUnauthorized, ""},
		{http.MethodDelete, "/v2/app_management/git/no-such-app/webhook", "", "", http.StatusUnauthorized, ""},
		{http.MethodPost, "/v2/app_management/git/no-such-app/webhook/more", "", "", http.StatusUnauthorized, ""},
		{http.MethodPost, "/v2/app_management/git/webhook", "", "", http.StatusUnauthorized, ""},
		{http.MethodPost, "/v2/app_management/git/no-such-app%2Fwebhook", "", "", http.StatusUnauthorized, ""},
		{http.MethodPost, "/v2/app_management/git/no-such-app/check", "", "", http.StatusUnauthorized, ""},
		{http.MethodPost, "/v2/app_management/git/no-such-app/deploy", "", "", http.StatusUnauthorized, ""},
		{http.MethodGet, "/v2/app_management/git/no-such-app", "", "", http.StatusUnauthorized, ""},
		{http.MethodPut, "/v2/app_management/git/no-such-app", "application/json", `{"webhook_enabled":true}`, http.StatusUnauthorized, ""},
		{http.MethodPost, "/v2/app_management/git", "application/json", `{"name":"jarvis","url":"https://example.invalid/r.git","access":"none"}`, http.StatusUnauthorized, ""},
		{http.MethodGet, "/v2/app_management/web/appgrid", "", "", http.StatusUnauthorized, ""},
		// it makes this machine ask a URL, so it is never for anyone without a token
		{http.MethodGet, "/v2/app_management/web/frame-check?url=http://127.0.0.1&origin=http://127.0.0.1", "", "", http.StatusUnauthorized, ""},
	} {
		request := httptest.NewRequest(call.method, call.path, strings.NewReader(call.body))
		// from another machine, as a forge calls: the internal secret's exemption does not apply
		request.RemoteAddr = "203.0.113.7:40000"
		if call.contentType != "" {
			request.Header.Set("Content-Type", call.contentType)
		}

		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)

		assert.Equal(t, recorder.Code, call.want, "%s %s: %s", call.method, call.path, recorder.Body.String())
		if call.answer != "" {
			assert.Equal(t, strings.TrimSpace(recorder.Body.String()), call.answer, "%s %s", call.method, call.path)
		}
	}
}
