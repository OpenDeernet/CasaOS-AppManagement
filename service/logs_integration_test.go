package service

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	appdocker "github.com/ReCasaOS/CasaOS-AppManagement/pkg/docker"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/logstore"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	client2 "github.com/docker/docker/client"
	"gotest.tools/v3/assert"
)

// A real container's lines and its died event, through Docker's own log stream and events.
func TestAContainersLinesAndDeathReachTheStore(t *testing.T) {
	if os.Getenv("CASAOS_INTEGRATION") == "" {
		t.Skip("set CASAOS_INTEGRATION=1 to run against a live CasaOS host")
	}
	if !appdocker.IsDaemonRunning() {
		t.Skip("Docker daemon is not running")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli, err := client2.NewClientWithOpts(client2.FromEnv, client2.WithAPIVersionNegotiation())
	assert.NilError(t, err)
	defer cli.Close()

	pull, err := cli.ImagePull(ctx, "busybox:latest", image.PullOptions{})
	assert.NilError(t, err)
	_, _ = io.Copy(io.Discard, pull)
	_ = pull.Close()

	s := openLogs(t)
	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image:  "busybox:latest",
		Cmd:    []string{"sh", "-c", "echo before; sleep 1; echo last; exit 137"},
		Labels: map[string]string{api.ProjectLabel: "itlogs", api.ServiceLabel: "app"},
	}, nil, nil, nil, "")
	assert.NilError(t, err)
	defer func() {
		_ = cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
	}()

	messages, errs := cli.Events(ctx, events.ListOptions{Filters: filters.NewArgs(filters.Arg("container", created.ID))})
	assert.NilError(t, cli.ContainerStart(ctx, created.ID, container.StartOptions{}))
	for died := false; !died; {
		select {
		case m := <-messages:
			logsOnEvent(ctx, cli, m)
			died = m.Action == events.ActionDie
		case err := <-errs:
			t.Fatal(err)
		}
	}

	files, err := logstore.ListFiles(s.Root)
	assert.NilError(t, err)
	var text string
	for _, f := range files {
		if strings.Contains(f.Path, "itlogs") {
			b, err := os.ReadFile(f.Path)
			assert.NilError(t, err)
			text += string(b)
		}
	}
	assert.Assert(t, strings.Contains(text, " o before\n") && strings.Contains(text, " o last\n"), text)

	got, err := s.Events(logstore.EventQuery{Apps: []string{"itlogs"}, Kinds: []string{"died"}})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].ExitCode, "137")
}
