package service_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/docker/compose/v5/pkg/api"
)

func TestComposeUpAgainstFakeDocker(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("Caller")
	}
	amRoot := filepath.Join(filepath.Dir(thisFile), "..")
	casaos := filepath.Join(amRoot, "..", "CasaOS")
	fakeDir := filepath.Join(casaos, "dev", "fakedocker")
	if _, err := os.Stat(fakeDir); err != nil {
		t.Skip("sibling CasaOS/dev/fakedocker missing")
	}

	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	data := filepath.Join(dir, "data")
	cmd := exec.Command("go", "run", "./dev/fakedocker", "-socket", sock, "-data", data)
	cmd.Dir = casaos
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", sock)
		if err == nil {
			_ = c.Close()
			break
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatal("fakedocker exited")
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_API_VERSION", "")

	project := "fakedemo"
	composePath := filepath.Join(dir, "docker-compose.yml")
	yaml := []byte("name: " + project + "\nservices:\n  web:\n    image: alpine:latest\n    command: [\"sleep\", \"infinity\"]\n")
	if err := os.WriteFile(composePath, yaml, 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := service.LoadComposeAppFromConfigFile(project, composePath)
	if err != nil {
		t.Fatal(err)
	}
	svc, dockerClient, err := service.ApiService()
	if err != nil {
		t.Fatal(err)
	}
	defer dockerClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := svc.Up(ctx, (*codegen.ComposeApp)(app), api.UpOptions{
		Create: api.CreateOptions{},
		Start:  api.StartOptions{Wait: true},
	}); err != nil {
		t.Fatal(err)
	}
	stacks, err := svc.List(ctx, api.ListOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range stacks {
		if s.ID == project || s.Name == project {
			found = true
			if s.Status != "running" && s.Status != "running(1)" {
				t.Fatalf("status %q", s.Status)
			}
		}
	}
	if !found {
		t.Fatalf("project %q not listed: %+v", project, stacks)
	}
	if _, err := os.Stat(filepath.Join(data, project, "docker-compose.yml")); err != nil {
		t.Fatalf("yaml: %v", err)
	}
}
