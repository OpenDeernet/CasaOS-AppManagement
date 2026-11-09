package rclone

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

// The daemon answers over a unix socket in production. Everything below drives the
// same code against an ordinary test server: what is worth pinning is the shape of
// the calls and how answers are read, not which kind of socket carries them.
func serverFor(t *testing.T, handler http.HandlerFunc) (*Client, *[]*http.Request) {
	t.Helper()

	seen := &[]*http.Request{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// a client whose deadline passes mid-request leaves a cut body: that is the client
		// giving up, which several tests provoke on purpose, not a malformed call
		if err := r.ParseForm(); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("bad form: %v", err)
		}
		*seen = append(*seen, r)
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	return &Client{http: resty.New().SetBaseURL(server.URL)}, seen
}

func TestDestinationsAreTheRemotesThisProjectPutThere(t *testing.T) {
	// a real box holds cloud drives in the same config, which are not backup
	// destinations and must not be offered as if they were
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string][]string{"remotes": {
			"GoogleDrive-gary",
			"casaos-backup-nas",
			"Dropbox",
			"casaos-backup-offsite",
		}})
	})

	names, err := client.Destinations()
	if err != nil {
		t.Fatal(err)
	}

	if len(names) != 2 || names[0] != "nas" || names[1] != "offsite" {
		t.Fatalf("want the two backup destinations, without the prefix: %v", names)
	}
	if (*seen)[0].URL.Path != "/config/listremotes" {
		t.Fatalf("wrong endpoint: %s", (*seen)[0].URL.Path)
	}
}

func TestCreatingADestination(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	err := client.CreateDestination("offsite", "s3", "", map[string]string{
		"provider":          "Wasabi",
		"access_key_id":     "AKIA...",
		"secret_access_key": "shhh",
	})
	if err != nil {
		t.Fatal(err)
	}

	form := (*seen)[0].PostForm
	if form.Get("name") != "casaos-backup-offsite" {
		t.Fatalf("the prefix is added here, not by the caller: %q", form.Get("name"))
	}
	if form.Get("type") != "s3" {
		t.Fatalf("want backend s3, got %q", form.Get("type"))
	}

	// the backend's own options are passed through: rclone knows what each backend
	// needs, and a list of required fields kept on this side would go stale
	var parameters map[string]string
	if err := json.Unmarshal([]byte(form.Get("parameters")), &parameters); err != nil {
		t.Fatalf("parameters are not JSON: %v", err)
	}
	if parameters["provider"] != "Wasabi" || parameters["secret_access_key"] != "shhh" {
		t.Fatalf("parameters did not survive: %v", parameters)
	}
}

// A token pasted from rclone authorize is kept as it is: asked to refresh it,
// rclone would start a sign-in and wait for a browser the box does not have.
func TestADestinationGivenATokenIsNotSignedInAgain(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	given := map[string]string{"token": `{"access_token":"x"}`, "drive_type": "personal"}

	for _, parameters := range []map[string]string{
		given,
		{"token": `{"access_token":"x"}`, "config_refresh_token": "true"},
		{"host": "nas.local"},
	} {
		backend := "onedrive"
		if parameters["token"] == "" {
			backend = "sftp"
		}
		if err := client.CreateDestination("offsite", backend, "", parameters); err != nil {
			t.Fatal(err)
		}
	}

	sent := func(i int) map[string]string {
		var parameters map[string]string
		if err := json.Unmarshal([]byte((*seen)[i].PostForm.Get("parameters")), &parameters); err != nil {
			t.Fatal(err)
		}
		return parameters
	}
	if got := sent(0); got["config_refresh_token"] != "false" || got["token"] == "" || got["drive_type"] != "personal" {
		t.Fatalf("a token must be kept as given, with the other options: %v", got)
	}
	if _, touched := given["config_refresh_token"]; touched {
		t.Fatal("the caller's map was changed")
	}
	if got := sent(1); got["config_refresh_token"] != "true" {
		t.Fatalf("an explicit choice is the owner's: %v", got)
	}
	if _, set := sent(2)["config_refresh_token"]; set {
		t.Fatal("a backend without a token is left alone")
	}
}

// rclone's OneDrive configuration asks about the kind of connection and the drive
// after the token, on the daemon's terminal where nobody answers: the call hangs
// until it times out (seen on a real box). Left unanswered, rclone keeps what it was
// given and returns the question at once.
func TestOneDriveIsCreatedWithoutWaitingForRcloneToAskQuestions(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	parameters := map[string]string{"token": `{"access_token":"x"}`, "drive_id": "d", "drive_type": "personal"}

	for backend, want := range map[string]string{"onedrive": `{"nonInteractive": true}`, "drive": "", "sftp": ""} {
		*seen = nil
		if err := client.CreateDestination("offsite", backend, "", parameters); err != nil {
			t.Fatal(err)
		}
		if got := (*seen)[0].PostForm.Get("opt"); got != want {
			t.Errorf("%s: want opt %q, got %q", backend, want, got)
		}
	}
}

func TestADriveWithoutATokenIsRefusedBeforeRcloneWaitsForABrowser(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })

	for _, backend := range []string{"onedrive", "drive", "dropbox"} {
		err := client.CreateDestination("offsite", backend, "Backup", map[string]string{"drive_id": "x"})

		var bad BadSignIn
		if !errors.As(err, &bad) || !strings.Contains(err.Error(), "sign in first") {
			t.Errorf("%s: want a sign-in mistake, got %v", backend, err)
		}
	}
	if len(*seen) != 0 {
		t.Fatal("none of them should have reached the daemon")
	}
}

func TestADestinationNeedsANameAndABackend(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })

	if err := client.CreateDestination("", "s3", "", nil); err == nil {
		t.Error("a destination with no name is not a destination")
	}
	if err := client.CreateDestination("offsite", "", "", nil); err == nil {
		t.Error("a destination with no backend is not a destination")
	}
	if len(*seen) != 0 {
		t.Fatal("neither should have reached the daemon")
	}
}

// The daemon says what went wrong in the body; the status code alone says nothing
// anyone can act on.
func TestRcloneSaysWhyItRefused(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"didn't find backend called \"s4\""}`))
	})

	err := client.CreateDestination("offsite", "s4", "", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if got := err.Error(); got != `rclone: didn't find backend called "s4"` {
		t.Fatalf("the daemon's own words: %q", got)
	}
}

func TestAnErrorWithNothingToSayStillSaysSomething(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`not json at all`))
	})

	if _, err := client.Destinations(); err == nil {
		t.Fatal("a status with no message is still an error, not an empty list")
	}
}

// rclone not running is a different problem from a destination being wrong, and
// the two deserve different advice -- so they are different errors.
func TestADaemonThatIsNotThere(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	client := &Client{http: resty.New().SetBaseURL(url)}

	_, err := client.Destinations()
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("want ErrDaemonUnreachable, got %v", err)
	}
}

// A destination is checked when somebody configures it, not at 3am by a scheduled
// job nobody is watching.
func TestCheckingADestination(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"used":1024,"free":2048,"total":3072}`))
	})

	space, err := client.CheckDestination("offsite")
	if err != nil {
		t.Fatal(err)
	}

	if (*seen)[0].PostForm.Get("fs") != "casaos-backup-offsite:" {
		t.Fatalf("an fs is a remote followed by a colon: %q", (*seen)[0].PostForm.Get("fs"))
	}
	if space.Used == nil || *space.Used != 1024 || space.Free == nil || *space.Free != 2048 {
		t.Fatalf("space did not survive: %+v", space)
	}
}

// Object stores mostly have no answer to "how much room is left", and inventing a
// zero would read as a full destination.
func TestADestinationThatCannotSayHowMuchRoomItHas(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	space, err := client.CheckDestination("offsite")
	if err != nil {
		t.Fatal(err)
	}
	if space.Free != nil || space.Used != nil || space.Total != nil {
		t.Fatalf("nothing said is not zero: %+v", space)
	}
}

// rclone's S3 backend has no About: asked for the space, the daemon answers an error
// rather than an empty answer. That is not a destination that cannot be reached, and
// listing the root says whether it can.
func TestADestinationWithoutAboutIsCheckedByListingIt(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/operations/about" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"S3 root doesn't support about","status":500}`))

			return
		}
		_, _ = w.Write([]byte(`{"list":[]}`))
	})

	space, err := client.CheckDestination("bucket")
	if err != nil {
		t.Fatalf("a destination that can be listed is reachable: %v", err)
	}
	if space.Free != nil || space.Used != nil || space.Total != nil {
		t.Fatalf("no space is told when the backend has none: %+v", space)
	}
	if len(*seen) != 2 || (*seen)[1].URL.Path != "/operations/list" || (*seen)[1].PostForm.Get("fs") != "casaos-backup-bucket:" {
		t.Fatalf("the root of the destination is what gets listed: %v", *seen)
	}
}

func TestADestinationWithoutAboutThatCannotBeListedIsNotReachable(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if r.URL.Path == "/operations/about" {
			_, _ = w.Write([]byte(`{"error":"S3 root doesn't support about","status":500}`))

			return
		}
		_, _ = w.Write([]byte(`{"error":"InvalidAccessKeyId","status":500}`))
	})

	if _, err := client.CheckDestination("bucket"); err == nil {
		t.Fatal("a wrong key must still be reported")
	}
}

func TestAnErrorOtherThanNoAboutIsNotHiddenByListing(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"couldn't connect","status":500}`))
	})

	if _, err := client.CheckDestination("nas"); err == nil {
		t.Fatal("an unreachable destination must be reported")
	}
	if len(*seen) != 1 {
		t.Fatalf("nothing else is asked once the first answer is a real error: %d calls", len(*seen))
	}
}

// An object store with no root puts one bucket per app, and at a provider where
// bucket names are shared by everyone that name is taken: a root says which bucket.
// rclone has no bucket option, so the backend goes under a raw name and the
// destination is an alias for a place inside it.
func TestADestinationWithARootIsAnAliasOfAPlaceInTheBackend(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	if err := client.CreateDestination("offsite", "s3", " my-bucket ", map[string]string{"provider": "AWS"}); err != nil {
		t.Fatal(err)
	}

	if len(*seen) != 2 {
		t.Fatalf("want the backend and the alias, got %d calls", len(*seen))
	}
	raw, alias := (*seen)[0].PostForm, (*seen)[1].PostForm
	if raw.Get("name") != "casaos-backup-offsite-raw" || raw.Get("type") != "s3" || !strings.Contains(raw.Get("parameters"), "AWS") {
		t.Fatalf("the backend goes under the raw name with its own options: %v", raw)
	}
	if alias.Get("name") != "casaos-backup-offsite" || alias.Get("type") != "alias" {
		t.Fatalf("the destination is the alias: %v", alias)
	}
	var parameters map[string]string
	if err := json.Unmarshal([]byte(alias.Get("parameters")), &parameters); err != nil {
		t.Fatal(err)
	}
	if parameters["remote"] != "casaos-backup-offsite-raw:my-bucket" {
		t.Fatalf("the alias points into the bucket, as typed but for spaces around it: %v", parameters)
	}
}

func TestADestinationWithoutARootIsTheBackendItself(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	if err := client.CreateDestination("offsite", "s3", "  ", nil); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0].PostForm.Get("name") != "casaos-backup-offsite" {
		t.Fatalf("a destination made without a root is one remote, as before: %d calls", len(*seen))
	}
}

func TestAnEncryptedDestinationEncryptsInsideItsRoot(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	if err := client.CreateEncryptedDestination("offsite", "s3", "my-bucket/box", nil, "secret"); err != nil {
		t.Fatal(err)
	}

	crypt := (*seen)[1].PostForm
	if crypt.Get("type") != "crypt" || !strings.Contains(crypt.Get("parameters"), "casaos-backup-offsite-raw:my-bucket/box") {
		t.Fatalf("the ciphertext goes inside the root: %v", crypt)
	}
}

func TestADestinationCannotBeNamedLikeTheRemoteUnderneathAnother(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	if err := client.CreateDestination("offsite-raw", "s3", "", nil); err == nil {
		t.Fatal("it would be listed by nothing")
	}
	if err := client.CreateDestination("offsite-raw", "s3", "bucket", nil); err == nil {
		t.Fatal("it would be listed by nothing")
	}
	if len(*seen) != 0 {
		t.Fatalf("nothing is asked of rclone: %d calls", len(*seen))
	}
}

// Half a destination is worse than none: the backend alone, under a raw name, is
// listed by nothing and reachable by anything.
func TestAFailedAliasTakesTheBackendBackOut(t *testing.T) {
	var deleted []string
	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/config/create" && r.PostForm.Get("type") == "alias":
			http.Error(w, `{"error":"no"}`, http.StatusInternalServerError)
		case r.URL.Path == "/config/delete":
			deleted = append(deleted, r.PostForm.Get("name"))
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})

	if err := client.CreateDestination("offsite", "s3", "bucket", nil); err == nil {
		t.Fatal("rclone refused the alias")
	}
	if !slices.Contains(deleted, "casaos-backup-offsite-raw") {
		t.Fatalf("the raw remote must go with it: %v", deleted)
	}
}

// The root of an object store is typed by hand, and a bucket that is not there is the
// mistake to catch when the destination is saved, not at 3am.
func TestAMissingBucketIsSaidInTheCheck(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if r.URL.Path == "/operations/about" {
			_, _ = w.Write([]byte(`{"error":"S3 bucket mine doesn't support about"}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"error in ListJSON: directory not found"}`))
	})

	_, err := client.CheckDestination("offsite")
	if err == nil || !strings.Contains(err.Error(), "bucket or folder of this destination does not exist") {
		t.Fatalf("want the missing bucket said in words: %v", err)
	}
}
