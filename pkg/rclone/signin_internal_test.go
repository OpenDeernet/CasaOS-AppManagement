package rclone

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the rclone these tests sign in with: the test binary is
// started in place of `rclone authorize` and behaves like it, so no real drive,
// provider or rclone install is involved.
func TestMain(m *testing.M) {
	switch os.Getenv("FAKE_RCLONE") {
	case "":
		os.Exit(m.Run())
	case "dead":
		// the shape of a real failure: the cause, rclone's usage text, then the cause again
		fmt.Fprintln(os.Stderr, "Error: Failed to start auth webserver: address already in use\nUsage:\n  rclone authorize [flags]\n\nFlags:\n  -h, --help   help for authorize")
		fmt.Fprintln(os.Stderr, "2026/11/04 Fatal error: Failed to start auth webserver: address already in use")
		os.Exit(1)
	default:
		fakeAuthorize()
	}
}

func fakeAuthorize() {
	done := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://accounts.example/auth?state="+r.URL.Query().Get("state"), http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "abc123" || r.URL.Query().Get("code") == "" {
			http.Error(w, "bad state", http.StatusBadRequest)

			return
		}
		_, _ = w.Write([]byte("Success!"))
		fmt.Printf("Paste the following into your remote machine --->\n{\"access_token\":\"%s\"}\n<---End paste\n", r.URL.Query().Get("code"))
		close(done)
	})

	go func() { _ = http.ListenAndServe(signInAddr, mux) }()
	time.Sleep(100 * time.Millisecond)
	fmt.Fprintln(os.Stderr, "NOTICE: If your browser doesn't open automatically go to the following link: http://"+signInAddr+"/auth?state=abc123")
	<-done
	time.Sleep(50 * time.Millisecond)
	os.Exit(0)
}

func fakeRclone(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("FAKE_RCLONE", mode)
	previous := rcloneBinary
	rcloneBinary = os.Args[0]
	t.Cleanup(func() {
		currentMu.Lock()
		stopCurrent()
		currentMu.Unlock()
		rcloneBinary = previous
	})
}

func TestSignInGivesTheProvidersAddressAndThenTheToken(t *testing.T) {
	fakeRclone(t, "ok")

	address, err := SignIn("drive")
	if err != nil {
		t.Fatal(err)
	}
	// the link rclone prints points at the box; the person needs the provider's
	if !strings.HasPrefix(address, "https://accounts.example/auth") || !strings.Contains(address, "state=abc123") {
		t.Fatalf("want the provider's address, got %s", address)
	}

	// what the person pastes is the page that failed to load, whole
	token, parameters, err := CompleteSignIn("http://127.0.0.1:53682/?state=abc123&code=4%2FXYZ&scope=drive")
	if err != nil {
		t.Fatal(err)
	}
	if token != `{"access_token":"4/XYZ"}` {
		t.Fatalf("token: %s", token)
	}
	if parameters != nil {
		t.Fatalf("drive needs nothing besides the token, got %v", parameters)
	}
}

func TestSignInToOneDriveAlsoGivesItsDrive(t *testing.T) {
	fakeRclone(t, "ok")
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// the fake rclone's access token is the code that was pasted
		if r.URL.Path != "/me/drive" || r.Header.Get("Authorization") != "Bearer M.C507" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(`{"id":"b!8f3a","driveType":"business","owner":{}}`))
	}))
	defer graph.Close()
	previous := graphURL
	graphURL = graph.URL
	defer func() { graphURL = previous }()

	if _, err := SignIn("onedrive"); err != nil {
		t.Fatal(err)
	}
	// OneDrive redirects to localhost, not 127.0.0.1: only the query matters
	token, parameters, err := CompleteSignIn("http://localhost:53682/?code=M.C507&state=abc123")
	if err != nil {
		t.Fatal(err)
	}
	if token != `{"access_token":"M.C507"}` || parameters["drive_id"] != "b!8f3a" || parameters["drive_type"] != "business" || len(parameters) != 2 {
		t.Fatalf("token %s, parameters %v", token, parameters)
	}

	// a token Microsoft refuses is reported, not saved without its drive
	if _, err := SignIn("onedrive"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CompleteSignIn("code=other&state=abc123"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want Microsoft's refusal, got %v", err)
	}
}

func TestSignInAcceptsOnlyTheQueryToo(t *testing.T) {
	fakeRclone(t, "ok")

	if _, err := SignIn("dropbox"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CompleteSignIn("state=abc123&code=c"); err != nil {
		t.Fatal(err)
	}
}

func TestSignInRefusesWhatIsNotFromThisSignIn(t *testing.T) {
	fakeRclone(t, "ok")
	if _, err := SignIn("drive"); err != nil {
		t.Fatal(err)
	}

	for pasted, want := range map[string]string{
		"http://127.0.0.1:53682/?state=other&code=c":               "another sign-in",
		"http://127.0.0.1:53682/?state=abc123":                     "no code",
		"http://127.0.0.1:53682/?state=abc123&error=access_denied": "refused",
		"": "no code",
	} {
		if _, _, err := CompleteSignIn(pasted); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want an error about %q, got %v", pasted, want, err)
		}
	}
}

// A tunnel to the box lets the browser reach rclone itself: rclone takes the code,
// prints the token and exits, and nothing listens when the address is pasted.
func TestSignInGivesTheTokenRcloneAlreadyPrinted(t *testing.T) {
	s := &signIn{backend: "dropbox", state: "abc123", cancel: func() {}, exited: make(chan struct{}), token: make(chan string, 1)}
	close(s.exited)
	s.token <- `{"access_token":"already"}`
	currentMu.Lock()
	current = s
	currentMu.Unlock()
	t.Cleanup(func() {
		currentMu.Lock()
		current = nil
		currentMu.Unlock()
	})

	if _, _, err := CompleteSignIn("http://127.0.0.1:53682/?state=other&code=c"); err == nil || !strings.Contains(err.Error(), "another sign-in") {
		t.Fatalf("an address from another sign-in is still refused, got %v", err)
	}

	token, _, err := CompleteSignIn("http://127.0.0.1:53682/?state=abc123&code=c")
	if err != nil || token != `{"access_token":"already"}` {
		t.Fatalf("want the token rclone printed, got %q, %v", token, err)
	}
}

func TestSignInIsOnlyOfferedForCloudDrives(t *testing.T) {
	if _, err := SignIn("sftp"); err == nil {
		t.Fatal("sftp has no sign-in")
	}
}

func TestCompletingWithoutStartingSaysSo(t *testing.T) {
	currentMu.Lock()
	stopCurrent()
	currentMu.Unlock()

	if _, _, err := CompleteSignIn("state=a&code=b"); err != ErrNoSignIn {
		t.Fatalf("want ErrNoSignIn, got %v", err)
	}
}

func TestSignInReportsAnRcloneThatStopsAtOnce(t *testing.T) {
	fakeRclone(t, "dead")

	_, err := SignIn("drive")
	if err == nil || !strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "Usage:") {
		t.Fatalf("want the cause rclone gave, without its usage text: %v", err)
	}
}
