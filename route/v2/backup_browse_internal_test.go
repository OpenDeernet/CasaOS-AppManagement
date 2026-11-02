package v2

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
)

func TestLookingInsideTheBackupOfTheBoxIsRefused(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), recorder)

	if err := (&AppManagement{}).BrowseBackup(ctx, "nas", "casaos-system", "2026-09-13T02-29-56Z", codegen.BrowseBackupParams{}); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("answered %d", recorder.Code)
	}
}

func TestAPathThatClimbsOutOfTheBackupIsABadRequestAndReachesNoDestination(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), recorder)

	climbing := "volumes/../../etc"
	if err := (&AppManagement{}).BrowseBackup(ctx, "nas", "blog", "2026-09-13T02-29-56Z", codegen.BrowseBackupParams{Path: &climbing}); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestARestoreOfAPathThatClimbsOutIsRefusedBeforeItStarts(t *testing.T) {
	body := `{"destination":"nas","app":"blog","stamp":"2026-09-13T02-29-56Z","paths":["volumes/data/../../../etc/passwd"]}`
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recorder := httptest.NewRecorder()

	if err := (&AppManagement{}).RestoreBackup(echo.New().NewContext(request, recorder)); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestARestoreOfSomePathsOfTheBoxIsRefused(t *testing.T) {
	body := `{"destination":"nas","app":"casaos-system","stamp":"2026-09-13T02-29-56Z","paths":["x"]}`
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recorder := httptest.NewRecorder()

	if err := (&AppManagement{}).RestoreBackup(echo.New().NewContext(request, recorder)); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("answered %d: %s", recorder.Code, recorder.Body.String())
	}
}
