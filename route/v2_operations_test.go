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
	"gotest.tools/v3/assert"
)

// The core asks this route, with the per-boot secret, before it updates the box by itself:
// an empty list, not null, when nothing runs; each held app with what holds it otherwise;
// and nothing at all to a caller without a token.
func TestTheOperationsRouteListsWhatHoldsTheApps(t *testing.T) {
	runtime := t.TempDir()
	previous := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	t.Cleanup(func() { config.CommonInfo.RuntimePath = previous })
	assert.NilError(t, os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("s3cret"), 0o600))

	router := InitV2Router()
	ask := func(authorization string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v2/app_management/operations", nil)
		request.RemoteAddr = "127.0.0.1:40000"
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}

		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)

		return recorder
	}

	answer := ask("Internal s3cret")
	assert.Equal(t, answer.Code, http.StatusOK, answer.Body.String())
	assert.Equal(t, strings.TrimSpace(answer.Body.String()), `{"operations":[],"os_upgrade_open":false}`)

	end, err := service.Begin("jarvis", "deploy")
	assert.NilError(t, err)
	answer = ask("Internal s3cret")
	assert.Equal(t, strings.TrimSpace(answer.Body.String()), `{"operations":[{"app":"jarvis","kind":"deploy"}],"os_upgrade_open":false}`)

	end()
	answer = ask("Internal s3cret")
	assert.Equal(t, strings.TrimSpace(answer.Body.String()), `{"operations":[],"os_upgrade_open":false}`)

	assert.Equal(t, ask("").Code, http.StatusUnauthorized)
}
