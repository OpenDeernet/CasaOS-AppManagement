package rclone

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

// Opt-in against the release's real daemon. All paths and the only remote belong
// to this test; no machine config, credentials or installed applications are used.
func TestVerificationWithRealRclone(t *testing.T) {
	if os.Getenv("CASAOS_RCLONE_INTEGRATION") != "1" {
		t.Skip("set CASAOS_RCLONE_INTEGRATION=1 to test a local rclone v1.75.1 daemon")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the production daemon uses a Unix socket")
	}
	binary := os.Getenv("RCLONE_BINARY")
	if binary == "" {
		binary = "rclone"
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote")
	for _, directory := range []string{"app/run/directory/empty", "app/run/empty"} {
		if err := os.MkdirAll(filepath.Join(remote, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"app/run/directory/data.txt": "backup bytes",
		"app/run/config[one]*.json":  "{}",
		"app/run/unrelated.txt":      "must not be part of the single-file comparison",
		"app/run/manifest.json":      "{}",
	} {
		if err := os.WriteFile(filepath.Join(remote, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(root, "rclone.conf")
	if err := os.WriteFile(config, []byte("[casaos-backup-test]\ntype = alias\nremote = "+remote+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "rclone.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, binary, "rcd", "--rc-addr", "unix://"+socket, "--rc-no-auth", "--config", config, "--log-level", "ERROR")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &Client{http: resty.New().SetTransport(transport).SetBaseURL("http://localhost").SetTimeout(3 * time.Second)}
	for {
		if err := client.callContext(ctx, "/rc/noop", nil, nil); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the isolated rclone daemon did not become ready")
		case <-time.After(20 * time.Millisecond):
		}
	}
	fastPolling(t)
	stage := filepath.Join(root, "staging")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if free, err := client.StagingFreeSpace(ctx, stage); err != nil || free <= 0 {
		t.Fatalf("free space: %d %v", free, err)
	}
	if data, err := client.FetchLimited(ctx, "test", "app/run/manifest.json", 1024); err != nil || string(data) != "{}" {
		t.Fatalf("bounded metadata: %q %v", data, err)
	}
	if _, err := client.FetchLimited(ctx, "test", "app/run/manifest.json", 1); err == nil {
		t.Fatal("oversized metadata was accepted")
	}
	if size, err := client.InspectBackupPath(ctx, "test", "app/run/directory", true); err != nil || size != (VerificationSize{Bytes: 12, Files: 1}) {
		t.Fatalf("directory size: %+v %v", size, err)
	}
	if size, err := client.InspectBackupPath(ctx, "test", "app/run/empty", true); err != nil || size != (VerificationSize{}) {
		t.Fatalf("empty directory: %+v %v", size, err)
	}
	if _, err := client.InspectBackupPath(ctx, "test", "app/run/missing", true); err == nil {
		t.Fatal("missing directory was mistaken for an empty one")
	}
	if _, err := client.InspectBackupPath(ctx, "test", "app/run/directory/data.txt", true); err == nil {
		t.Fatal("file was mistaken for directory")
	}
	dirStage := filepath.Join(stage, "directory")
	if err := client.VerifyBackupPath(ctx, "test", "app/run/directory", dirStage, true, 12); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dirStage, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty child directory was not copied: %v", err)
	}
	fileStage := filepath.Join(stage, "single")
	if err := os.Mkdir(fileStage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyBackupPath(ctx, "test", "app/run/config[one]*.json", filepath.Join(fileStage, "config[one]*.json"), false, 2); err != nil {
		t.Fatalf("literal filename verification: %v", err)
	}
	if err := client.VerifyBackupPath(ctx, "test", "app/run/empty", filepath.Join(stage, "empty"), true, 0); err != nil {
		t.Fatalf("zero-byte directory: %v", err)
	}
	if err := client.VerifyBackupPath(ctx, "test", "app/run/directory", filepath.Join(stage, "limited"), true, 1); err == nil {
		t.Fatal("hard transfer limit was ignored")
	}
	// A same-length corruption is invisible to size-only checks, but the real
	// download comparison returns output.success=false even though its job succeeds.
	if err := os.WriteFile(filepath.Join(dirStage, "data.txt"), []byte("broken bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := client.verificationJob(ctx, "/operations/check", map[string]string{
		"srcFs": Destination("test", "app/run/directory"), "dstFs": verificationLocal(dirStage), "download": "true", "oneWay": "false",
	}, 1024)
	if err != nil {
		t.Fatalf("check envelope should succeed: %v", err)
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(out, &result); err != nil || result.Success {
		t.Fatalf("content corruption must fail endpoint result: %s %v", out, err)
	}
}
