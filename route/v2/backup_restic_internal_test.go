package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/restic"
)

func resticState(t *testing.T, name string) (enabled, kept bool) {
	t.Helper()

	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), recorder)
	if err := (&AppManagement{}).BackupDestinationRestic(ctx, name); err != nil {
		t.Fatal(err)
	}

	var answer struct {
		Data struct {
			Enabled bool `json:"enabled"`
			Kept    bool `json:"kept"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("%v: %s", err, recorder.Body.String())
	}

	return answer.Data.Enabled, answer.Data.Kept
}

func switchBack(t *testing.T, name string) int {
	t.Helper()

	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodDelete, "/", nil), recorder)
	if err := (&AppManagement{}).DisableBackupDestinationRestic(ctx, name); err != nil {
		t.Fatal(err)
	}

	return recorder.Code
}

func TestASwitchedBackDestinationIsNotEnabledButItsRepositoryIsStillKept(t *testing.T) {
	was := restic.KeyDir
	restic.KeyDir = t.TempDir()
	t.Cleanup(func() { restic.KeyDir = was })

	if err := os.WriteFile(filepath.Join(restic.KeyDir, "nas.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	if enabled, kept := resticState(t, "nas"); !enabled || !kept {
		t.Fatalf("before: enabled %v, kept %v", enabled, kept)
	}

	if code := switchBack(t, "nas"); code != http.StatusOK {
		t.Fatalf("switching back answered %d", code)
	}

	// this is what the screen needs to offer "Resume" and a check, not "Keep with restic"
	if enabled, kept := resticState(t, "nas"); enabled || !kept {
		t.Fatalf("after: enabled %v, kept %v", enabled, kept)
	}
}

func TestSwitchingBackADestinationWithNoRepositoryIsABadRequest(t *testing.T) {
	was := restic.KeyDir
	restic.KeyDir = t.TempDir()
	t.Cleanup(func() { restic.KeyDir = was })

	if code := switchBack(t, "nas"); code != http.StatusBadRequest {
		t.Fatalf("answered %d", code)
	}
	if enabled, kept := resticState(t, "nas"); enabled || kept {
		t.Fatalf("enabled %v, kept %v", enabled, kept)
	}
}
