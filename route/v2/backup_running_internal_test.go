package v2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/backupprogress"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
	"gotest.tools/v3/assert"
)

// stallingCopier says it started and holds its copy until the backup is stopped.
type stallingCopier struct {
	started chan struct{}
	report  bool
}

func (s *stallingCopier) copy(ctx context.Context) error {
	if s.report {
		backupprogress.Report(ctx, backupprogress.Stats{Bytes: 5, Files: 1})
	}
	s.started <- struct{}{}
	<-ctx.Done()

	return ctx.Err()
}

func (s *stallingCopier) CopyDirectory(ctx context.Context, _, _, _ string) error { return s.copy(ctx) }
func (s *stallingCopier) CopyFile(ctx context.Context, _, _, _ string) error      { return s.copy(ctx) }

type idleDocker struct{}

func (idleDocker) StopContainer(string) error  { return nil }
func (idleDocker) StartContainer(string) error { return nil }
func (idleDocker) VolumeMountpoints(context.Context, []string) map[string]string {
	return map[string]string{}
}

// runningDemo starts a backup of an app that is one compose file, held at its first copy.
func runningDemo(t *testing.T, report bool) (<-chan error, string) {
	t.Helper()

	dir := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("name: demo\n"), 0o600))
	app := &service.ComposeApp{Name: "demo", WorkingDir: filepath.ToSlash(dir), ComposeFiles: []string{filepath.ToSlash(dir) + "/docker-compose.yml"}}

	copier := &stallingCopier{started: make(chan struct{}, 1), report: report}
	done := make(chan error, 1)
	go func() {
		_, err := service.RunBackup(context.Background(), app, idleDocker{}, copier, service.BackupOptions{Destination: "GDrive", Stamp: "2026-11-08T16-55-18Z"})
		done <- err
	}()
	<-copier.started

	for _, live := range service.LiveBackups() {
		if live.App == "demo" {
			return done, live.ID
		}
	}
	t.Fatal("the backup is not listed")

	return nil, ""
}

func running(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, "/v2/app_management/backup/running", nil)
	recorder := httptest.NewRecorder()
	assert.NilError(t, (&AppManagement{}).BackupRunning(echo.New().NewContext(request, recorder)))

	return recorder
}

func stop(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/v2/app_management/backup/running/"+id+"/stop", nil)
	recorder := httptest.NewRecorder()
	assert.NilError(t, (&AppManagement{}).StopRunningBackup(echo.New().NewContext(request, recorder), id))

	return recorder
}

func TestTheRunningRouteSaysNothingRunsWithEmptyLists(t *testing.T) {
	answer := running(t)

	assert.Equal(t, answer.Code, http.StatusOK)
	var body map[string]json.RawMessage
	assert.NilError(t, json.Unmarshal(answer.Body.Bytes(), &body))
	assert.Equal(t, string(body["backups"]), "[]")
	assert.Assert(t, body["passes"] != nil, "passes is always there: %s", answer.Body.String())
}

func TestTheRunningRouteListsABackupAndItsPass(t *testing.T) {
	done, id := runningDemo(t, true)
	t.Cleanup(func() { _ = service.StopLiveBackup(id); <-done })

	text := running(t).Body.String()
	for _, want := range []string{`"id":"` + id + `"`, `"app":"demo"`, `"origin":"on_demand"`, `"operation":1`, `"bytes":5`, `"files":1`} {
		assert.Assert(t, strings.Contains(text, want), "%s not in %s", want, text)
	}
	assert.Assert(t, !strings.Contains(text, "total_bytes"), "an unknown total is left out: %s", text)
	assert.Assert(t, !strings.Contains(text, "null"), "nothing unknown is null: %s", text)

	var body struct {
		Backups []struct {
			StartedAt string `json:"started_at"`
		} `json:"backups"`
	}
	assert.NilError(t, json.Unmarshal([]byte(text), &body))
	assert.Assert(t, strings.Contains(body.Backups[0].StartedAt, "T"), "RFC 3339: %s", body.Backups[0].StartedAt)
}

func TestStoppingThroughTheRouteStopsTheBackup(t *testing.T) {
	done, id := runningDemo(t, false)

	assert.Equal(t, stop(t, id).Code, http.StatusAccepted)
	err := <-done
	assert.Assert(t, errors.Is(err, service.ErrStoppedFromInterface), "stopped: %v", err)
	assert.Assert(t, !strings.Contains(running(t).Body.String(), `"id":"`+id+`"`))
}

func TestStoppingAnUnknownBackupIs404(t *testing.T) {
	assert.Equal(t, stop(t, "nope").Code, http.StatusNotFound)
}
