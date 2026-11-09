package rclone

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

// Each endpoint runs as an asynchronous job, even when its useful output is a
// size or a comparison result. The outer job succeeds independently of that output.
func verificationServer(t *testing.T, result func(string, url.Values) any) (*Client, *[]*http.Request) {
	t.Helper()
	fastPolling(t)
	var output any
	return serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/job/status" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "finished": true, "success": true, "output": output})
			return
		}
		output = result(r.URL.Path, r.PostForm)
		_, _ = w.Write([]byte(`{"jobid":1}`))
	})
}

func TestVerificationInspectsExistenceTypeAndKnownSize(t *testing.T) {
	for _, tc := range []struct {
		name       string
		directory  bool
		stat, size any
		want       VerificationSize
		fails      bool
	}{
		{"file", false, map[string]any{"item": map[string]any{"Size": 3, "IsDir": false}}, nil, VerificationSize{Bytes: 3, Files: 1}, false},
		{"missing", false, map[string]any{"item": nil}, nil, VerificationSize{}, true},
		{"wrong type", false, map[string]any{"item": map[string]any{"Size": 0, "IsDir": true}}, nil, VerificationSize{}, true},
		{"unknown file size", false, map[string]any{"item": map[string]any{"Size": -1, "IsDir": false}}, nil, VerificationSize{}, true},
		{"omitted file size", false, map[string]any{"item": map[string]any{"IsDir": false}}, nil, VerificationSize{}, true},
		{"empty directory", true, map[string]any{"item": map[string]any{"Size": -1, "IsDir": true}}, map[string]any{"bytes": 0, "count": 0, "sizeless": 0}, VerificationSize{}, false},
		{"directory", true, map[string]any{"item": map[string]any{"IsDir": true}}, map[string]any{"bytes": 5, "count": 2, "sizeless": 0}, VerificationSize{Bytes: 5, Files: 2}, false},
		{"sizeless files", true, map[string]any{"item": map[string]any{"IsDir": true}}, map[string]any{"bytes": 5, "count": 2, "sizeless": 1}, VerificationSize{}, true},
		{"incomplete size", true, map[string]any{"item": map[string]any{"IsDir": true}}, map[string]any{"bytes": 5, "count": 2}, VerificationSize{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := verificationServer(t, func(endpoint string, _ url.Values) any {
				if endpoint == "/operations/stat" {
					return tc.stat
				}
				return tc.size
			})
			got, err := client.InspectBackupPath(context.Background(), "test", "app/run/part", tc.directory)
			if (err != nil) != tc.fails || (!tc.fails && got != tc.want) {
				t.Fatalf("got %+v, %v; want %+v, fails=%v", got, err, tc.want, tc.fails)
			}
		})
	}
}

func TestVerificationChecksEndpointResultAndUsesBoundedExactFileFilter(t *testing.T) {
	for _, result := range []any{map[string]any{"success": true}, map[string]any{"success": false, "status": "private backend diagnostic"}, map[string]any{}} {
		t.Run(stringJSON(result), func(t *testing.T) {
			staging := t.TempDir()
			name := "config[one]*.json"
			client, seen := verificationServer(t, func(endpoint string, params url.Values) any {
				if endpoint != "/operations/check" {
					return map[string]any{}
				}
				var filter struct{ FilesFromRaw []string }
				if err := json.Unmarshal([]byte(params.Get("_filter")), &filter); err != nil || len(filter.FilesFromRaw) != 1 {
					t.Fatalf("exact filter missing: %+v %v", filter, err)
				}
				contents, err := os.ReadFile(filter.FilesFromRaw[0])
				if err != nil || string(contents) != name+"\n" {
					t.Fatalf("literal filename list: %q %v", contents, err)
				}
				return result
			})
			err := client.VerifyBackupPath(context.Background(), "test", "app/run/"+name, filepath.Join(staging, name), false, 42)
			ok := result.(map[string]any)["success"] == true
			if (err == nil) != ok {
				t.Fatalf("check output must decide success: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private backend") {
				t.Fatal("backend diagnostics must not leak")
			}
			var started []*http.Request
			for _, request := range *seen {
				if request.URL.Path != "/job/status" {
					started = append(started, request)
				}
			}
			if len(started) != 2 || started[0].URL.Path != "/operations/copyfile" || started[1].URL.Path != "/operations/check" {
				t.Fatalf("unexpected calls: %v", started)
			}
			for i, request := range started {
				var config map[string]any
				if err := json.Unmarshal([]byte(request.PostForm.Get("_config")), &config); err != nil {
					t.Fatal(err)
				}
				if config["MaxTransfer"] != float64(42*(i+1)+1) || config["CutoffMode"] != "HARD" || config["Links"] != false || config["SizeOnly"] != false {
					t.Fatalf("limits: %+v", config)
				}
				if !strings.HasPrefix(request.PostForm.Get("dstFs"), ":local,copy_links=false,links=false,no_clone=true:") {
					t.Fatal("staging must disable links and server-side clones")
				}
			}
			if started[1].PostForm.Get("download") != "true" || started[1].PostForm.Get("oneWay") != "false" {
				t.Fatal("full bidirectional read required")
			}
			entries, err := os.ReadDir(staging)
			if err != nil || len(entries) != 0 {
				t.Fatalf("filter must be removed after finished job: %v %v", entries, err)
			}
		})
	}
}

func stringJSON(value any) string { b, _ := json.Marshal(value); return string(b) }

func TestVerificationStopsOnStatusFailureAndWaitsForCompletion(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "status failure", true: "cancellation"}[cancelled], func(t *testing.T) {
			fastPolling(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stopped, polls := false, 0
			client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/sync/copy":
					_, _ = w.Write([]byte(`{"jobid":3}`))
				case "/job/stop":
					stopped = true
					_, _ = w.Write([]byte(`{}`))
				case "/job/status":
					polls++
					if !stopped {
						if cancelled {
							cancel()
							_, _ = w.Write([]byte(`{"finished":false}`))
						} else {
							w.WriteHeader(500)
							_, _ = w.Write([]byte(`{"error":"private diagnostic"}`))
						}
						return
					}
					_, _ = w.Write([]byte(`{"finished":true,"success":false}`))
				}
			})
			_, err := client.verificationJob(ctx, "/sync/copy", map[string]string{}, 10)
			if err == nil || !stopped || polls < 2 || errors.Is(err, ErrVerificationJobUncertain) {
				t.Fatalf("stop must be confirmed: stopped=%v polls=%d err=%v", stopped, polls, err)
			}
			if cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("preserve cancellation: %v", err)
			}
			if strings.Contains(err.Error(), "private diagnostic") {
				t.Fatal("diagnostics must remain private")
			}
		})
	}
}

func TestVerificationRetainsStagingWhenJobWillNotStop(t *testing.T) {
	fastPolling(t)
	old := verificationStopTimeout
	verificationStopTimeout = 10 * time.Millisecond
	t.Cleanup(func() { verificationStopTimeout = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/copy":
			_, _ = w.Write([]byte(`{"jobid":3}`))
		case "/job/status":
			cancel()
			_, _ = w.Write([]byte(`{"finished":false}`))
		case "/job/stop":
			_, _ = w.Write([]byte(`{}`))
		}
	})
	_, err := client.verificationJob(ctx, "/sync/copy", map[string]string{}, 10)
	if !errors.Is(err, ErrVerificationJobUncertain) || !errors.Is(err, context.Canceled) {
		t.Fatalf("caller must retain its staging: %v", err)
	}
}

func TestVerificationFetchKeepsItsTemporaryDirectoryForAnUncertainJob(t *testing.T) {
	fastPolling(t)
	copyStarted := false
	var staging string
	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/operations/stat":
			_, _ = w.Write([]byte(`{"jobid":1}`))
		case "/operations/copyfile":
			copyStarted = true
			staging = strings.TrimPrefix(r.PostForm.Get("dstFs"), ":local,copy_links=false,links=false,no_clone=true:")
			_, _ = w.Write([]byte(`{"jobid":2}`))
		case "/job/status":
			if !copyStarted {
				_, _ = w.Write([]byte(`{"finished":true,"success":true,"output":{"item":{"Size":2,"IsDir":false}}}`))
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		case "/job/stop":
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	_, err := client.FetchLimited(context.Background(), "test", "app/run/manifest.json", 1024)
	if staging != "" {
		t.Cleanup(func() { _ = os.RemoveAll(staging) })
	}
	if !errors.Is(err, ErrVerificationJobUncertain) || staging == "" {
		t.Fatalf("uncertain job must reach the caller: staging=%q err=%v", staging, err)
	}
	if info, err := os.Stat(staging); err != nil || !info.IsDir() {
		t.Fatalf("the daemon may still write, so its staging must remain: %v", err)
	}
}

func TestVerificationRequiresReportedStagingSpace(t *testing.T) {
	for _, tc := range []struct {
		output any
		valid  bool
	}{
		{map[string]any{"free": 1234}, true}, {map[string]any{}, false}, {map[string]any{"free": -1}, false},
	} {
		t.Run(stringJSON(tc.output), func(t *testing.T) {
			client, _ := verificationServer(t, func(string, url.Values) any { return tc.output })
			free, err := client.StagingFreeSpace(context.Background(), t.TempDir())
			if (err == nil) != tc.valid || (tc.valid && free != 1234) {
				t.Fatalf("free space: %d %v", free, err)
			}
		})
	}
}

func TestVerificationFetchIsBoundedAndRejectsSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reported int64
		content  string
		link     bool
		fails    bool
	}{
		{"valid", 2, "{}", false, false}, {"too large before copy", 3, "", false, true},
		{"grew during copy", 2, "too big", false, true}, {"symlink", 2, "{}", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var copiedPath string
			client, _ := verificationServer(t, func(endpoint string, params url.Values) any {
				if endpoint == "/operations/stat" {
					return map[string]any{"item": map[string]any{"Size": tc.reported, "IsDir": false}}
				}
				copiedPath = filepath.Join(strings.TrimPrefix(params.Get("dstFs"), ":local,copy_links=false,links=false,no_clone=true:"), params.Get("dstRemote"))
				if tc.link {
					target := filepath.Join(t.TempDir(), "outside")
					if err := os.WriteFile(target, []byte(tc.content), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, copiedPath); err != nil {
						t.Skipf("symlink unsupported: %v", err)
					}
				} else if err := os.WriteFile(copiedPath, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
				return map[string]any{}
			})
			data, err := client.FetchLimited(context.Background(), "test", "app/run/manifest.json", 2)
			if (err != nil) != tc.fails || (!tc.fails && string(data) != "{}") {
				t.Fatalf("bounded fetch: %q %v", data, err)
			}
			if copiedPath != "" {
				if _, err := os.Stat(filepath.Dir(copiedPath)); !os.IsNotExist(err) {
					t.Fatalf("finished fetch must clean staging: %v", err)
				}
			}
		})
	}
}

// rclone refuses a copy once the bytes so far reach MaxTransfer (>=, fs/operations
// copy.go checkLimits), so a limit of exactly the size refuses an empty file, and
// an empty file copied last in a directory. One byte over is still caught: the
// local size is compared with the measured one afterwards.
func TestVerificationLimitLetsEmptyFilesThrough(t *testing.T) {
	var config map[string]any
	if err := json.Unmarshal([]byte(verificationConfig(context.Background(), 0)), &config); err != nil {
		t.Fatal(err)
	}
	if config["MaxTransfer"] != float64(1) {
		t.Fatalf("an empty object would be refused: %+v", config["MaxTransfer"])
	}
}

// Only an answer lost after the request went out can hide a running job. A daemon
// that is not there, or that answered with an error, started nothing: calling that
// uncertain kept an empty workspace and refused every verification for two hours.
func TestVerificationThatNeverStartedIsNotUncertain(t *testing.T) {
	fastPolling(t)
	errorAnswer, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"directory not found"}`))
	})
	lostAnswer, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	})
	for _, tc := range []struct {
		name      string
		client    *Client
		uncertain bool
	}{
		{"refused", &Client{http: resty.New().SetBaseURL("http://127.0.0.1:1")}, false},
		{"error answer", errorAnswer, false},
		{"lost answer", lostAnswer, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.InspectBackupPath(context.Background(), "test", "app/run/manifest.json", false)
			if err == nil || errors.Is(err, ErrVerificationJobUncertain) != tc.uncertain {
				t.Fatalf("uncertain=%v wanted, got %v", tc.uncertain, err)
			}
		})
	}
}
