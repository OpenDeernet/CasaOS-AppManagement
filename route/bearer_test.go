package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	"gotest.tools/v3/assert"
)

// The v2 API takes the user's access token bare (as the dashboard sends it) and as
// `Bearer <token>`; another scheme, or the scheme alone, is no token.
func TestTheAccessTokenIsTakenBareOrAsBearer(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)

	privateKey, publicKey := testUserKey()
	jwks, err := jwt.GenerateJwksJSON(publicKey)
	assert.NilError(t, err)
	userService := httptest.NewServer(jwt.JWKSHandler(jwks))
	defer userService.Close()
	assert.NilError(t, os.WriteFile(filepath.Join(config.CommonInfo.RuntimePath, external.UserServiceAddressFilename), []byte(userService.URL), 0o600))
	token, err := jwt.GetAccessToken("admin", privateKey, 1)
	assert.NilError(t, err)

	for authorization, want := range map[string]int{
		token:             http.StatusOK,
		"Bearer " + token: http.StatusOK,
		"bearer " + token: http.StatusOK,
		"Basic " + token:  http.StatusUnauthorized,
		"Bearer":          http.StatusUnauthorized,
	} {
		request := httptest.NewRequest(http.MethodGet, "/v2/app_management/stats", nil)
		request.RemoteAddr = "203.0.113.5:40000"
		request.Header.Set("Authorization", authorization)
		recorder := httptest.NewRecorder()
		r.router.ServeHTTP(recorder, request)
		assert.Equal(t, recorder.Code, want, "Authorization: %.12s…", authorization)
	}
}
