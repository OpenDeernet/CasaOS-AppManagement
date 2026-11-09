package rclone

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/backupprogress"
)

// Nothing here should wait two seconds to find out a job finished.
func fastPolling(t *testing.T) {
	t.Helper()

	was := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = was })
}

func TestCopyingAFolderStartsAJobAndWaitsForIt(t *testing.T) {
	fastPolling(t)

	var polls int32
	client, seen := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/copy":
			_, _ = w.Write([]byte(`{"jobid": 7}`))
		case "/job/status":
			// two frames of "still going" before it finishes, so the wait is real
			if atomic.AddInt32(&polls, 1) < 3 {
				_, _ = w.Write([]byte(`{"id":7,"finished":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":7,"finished":true,"success":true}`))
		}
	})

	if err := client.CopyDirectory(context.Background(), "/DATA/Media", "offsite", "nextcloud/run/binds/1"); err != nil {
		t.Fatal(err)
	}

	start := (*seen)[0].PostForm
	if start.Get("srcFs") != "/DATA/Media" {
		t.Fatalf("source: %q", start.Get("srcFs"))
	}
	if start.Get("dstFs") != "casaos-backup-offsite:nextcloud/run/binds/1" {
		t.Fatalf("destination: %q", start.Get("dstFs"))
	}
	// nothing holds an HTTP request open for the length of a transfer
	if start.Get("_async") != "true" {
		t.Fatal("the copy must be a job")
	}
	if atomic.LoadInt32(&polls) < 3 {
		t.Fatalf("it should have waited: %d polls", polls)
	}
}

// rclone addresses a file as a folder plus a name, on both sides. Handing a file
// to the directory call copies its whole parent.
func TestCopyingASingleFileAddressesItAsFolderAndName(t *testing.T) {
	fastPolling(t)

	client, seen := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/operations/copyfile" {
			_, _ = w.Write([]byte(`{"jobid": 9}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":9,"finished":true,"success":true}`))
	})

	err := client.CopyFile(context.Background(),
		"/var/lib/casaos/apps/nextcloud/docker-compose.yml", "offsite", "nextcloud/run/compose/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}

	form := (*seen)[0].PostForm
	for field, want := range map[string]string{
		"srcFs":     "/var/lib/casaos/apps/nextcloud",
		"srcRemote": "docker-compose.yml",
		"dstFs":     "casaos-backup-offsite:nextcloud/run/compose",
		"dstRemote": "docker-compose.yml",
	} {
		if got := form.Get(field); got != want {
			t.Errorf("%s: want %q, got %q", field, want, got)
		}
	}
}

func TestAJobThatFailsSaysWhy(t *testing.T) {
	fastPolling(t)

	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sync/copy" {
			_, _ = w.Write([]byte(`{"jobid": 3}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":3,"finished":true,"success":false,"error":"AccessDenied: no write permission"}`))
	})

	err := client.CopyDirectory(context.Background(), "/DATA", "offsite", "x")
	if err == nil {
		t.Fatal("want an error")
	}
	if err.Error() != "rclone: AccessDenied: no write permission" {
		t.Fatalf("the daemon's own words: %q", err)
	}
}

// A job that reports failure and says nothing must still be an error, not a
// silent success.
func TestAJobThatFailsSilently(t *testing.T) {
	fastPolling(t)

	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sync/copy" {
			_, _ = w.Write([]byte(`{"jobid": 4}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":4,"finished":true,"success":false}`))
	})

	if err := client.CopyDirectory(context.Background(), "/DATA", "offsite", "x"); err == nil {
		t.Fatal("a job that did not succeed did not succeed")
	}
}

// Walking away from a running transfer leaves it writing to the destination long
// after whoever asked for it has gone, and the next run then races it.
func TestCancellingStopsTheJobRatherThanWalkingAway(t *testing.T) {
	fastPolling(t)

	var stopped int32
	ctx, cancel := context.WithCancel(context.Background())

	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/copy":
			_, _ = w.Write([]byte(`{"jobid": 11}`))
		case "/job/status":
			cancel() // it is running, and the caller gives up
			_, _ = w.Write([]byte(`{"id":11,"finished":false}`))
		case "/job/stop":
			atomic.AddInt32(&stopped, 1)
			_, _ = w.Write([]byte(`{}`))
		}
	})

	err := client.CopyDirectory(ctx, "/DATA/Media", "offsite", "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the cancellation, got %v", err)
	}
	if atomic.LoadInt32(&stopped) != 1 {
		t.Fatal("the job must be stopped, not abandoned")
	}
}

// A daemon that accepts the call and starts nothing would otherwise be polled for
// job zero for ever.
func TestACallThatStartsNoJob(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	if err := client.CopyDirectory(context.Background(), "/DATA", "offsite", "x"); err == nil {
		t.Fatal("no job number is not a copy in progress")
	}
}

func TestListingTheRunsAnAppHasAtADestination(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"list":[
			{"Name":"2026-09-10T03-00-00Z","IsDir":true},
			{"Name":"2026-09-11T03-00-00Z","IsDir":true},
			{"Name":"stray.txt","IsDir":false}
		]}`))
	})

	stamps, err := client.Runs("offsite", "nextcloud")
	if err != nil {
		t.Fatal(err)
	}
	if len(stamps) != 2 {
		t.Fatalf("only the run folders: %v", stamps)
	}
	if (*seen)[0].PostForm.Get("remote") != "nextcloud" {
		t.Fatalf("one app's folder: %q", (*seen)[0].PostForm.Get("remote"))
	}
}

// The only call here that removes anything.
func TestPurgingOneRun(t *testing.T) {
	fastPolling(t)

	client, seen := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/operations/purge" {
			_, _ = w.Write([]byte(`{"jobid": 21}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":21,"finished":true,"success":true}`))
	})

	if err := client.Purge(context.Background(), "offsite", "nextcloud/2026-09-08T03-00-00Z"); err != nil {
		t.Fatal(err)
	}

	form := (*seen)[0].PostForm
	if form.Get("fs") != "casaos-backup-offsite:" || form.Get("remote") != "nextcloud/2026-09-08T03-00-00Z" {
		t.Fatalf("one run, not the app: fs=%q remote=%q", form.Get("fs"), form.Get("remote"))
	}
}

// A caller that gives up must hear that it gave up, not that the daemon is gone:
// the two lead to different decisions about a job that may still be running.
func TestCallContextReportsTheCancellationNotTheDaemon(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := client.callContext(ctx, "/rc/noop", nil, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("got %v, want the cancellation", err)
	}
}

// Some endpoints (operations/check, operations/size) put their answer in the job's
// output, while the job itself succeeds whatever that answer is.
func TestJobStatusCarriesTheEndpointOutput(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":4,"finished":true,"success":true,"output":{"success":false}}`))
	})

	status, err := client.JobStatus(4)
	if err != nil || string(status.Output) != `{"success":false}` {
		t.Fatalf("output lost: %q %v", status.Output, err)
	}
}

func TestAJobWithAReporterReportsItsStatsWhileItRuns(t *testing.T) {
	fastPolling(t)

	var polls int32
	var groups []string
	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/copy":
			_, _ = w.Write([]byte(`{"jobid": 7}`))
		case "/job/status":
			if atomic.AddInt32(&polls, 1) < 3 {
				_, _ = w.Write([]byte(`{"id":7,"finished":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":7,"finished":true,"success":true}`))
		case "/core/stats":
			groups = append(groups, r.PostForm.Get("group"))
			_, _ = w.Write([]byte(`{"bytes":1182844747,"totalBytes":1926565931,"transfers":9812,"totalTransfers":25488,` +
				`"speed":219371.14,"eta":3390,"transferring":[{"name":"cache/a.jpg"}]}`))
		}
	})

	var got []backupprogress.Stats
	ctx := backupprogress.With(context.Background(), func(stats backupprogress.Stats) { got = append(got, stats) })
	if err := client.CopyDirectory(ctx, "/DATA/AppData/overseerr/config", "GDrive", "happy_rik/run/binds/1"); err != nil {
		t.Fatal(err)
	}

	if len(got) == 0 {
		t.Fatal("no stats reported")
	}
	for _, group := range groups {
		if group != "job/7" {
			t.Fatalf("stats asked for group %q", group)
		}
	}
	stats := got[0]
	if stats.Bytes != 1182844747 || stats.TotalBytes != 1926565931 || stats.Files != 9812 || stats.TotalFiles != 25488 ||
		stats.Speed != 219371 || stats.ETA == nil || *stats.ETA != 3390 || stats.Current != "cache/a.jpg" {
		t.Fatalf("stats: %+v", stats)
	}
}

func TestStatsThatFailDoNotStopTheCopy(t *testing.T) {
	fastPolling(t)

	var polls, asked int32
	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/copy":
			_, _ = w.Write([]byte(`{"jobid": 8}`))
		case "/job/status":
			if atomic.AddInt32(&polls, 1) < 4 {
				_, _ = w.Write([]byte(`{"id":8,"finished":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":8,"finished":true,"success":true}`))
		case "/core/stats":
			switch atomic.AddInt32(&asked, 1) {
			case 1:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
			case 2:
				_, _ = w.Write([]byte(`{}`))
			default:
				_, _ = w.Write([]byte(`{"eta":null}`))
			}
		}
	})

	var got []backupprogress.Stats
	ctx := backupprogress.With(context.Background(), func(stats backupprogress.Stats) { got = append(got, stats) })
	if err := client.CopyDirectory(ctx, "/DATA/Media", "offsite", "x"); err != nil {
		t.Fatalf("the copy must not fail on its stats: %v", err)
	}
	if atomic.LoadInt32(&asked) < 3 {
		t.Fatalf("stats asked %d times", asked)
	}
	for _, stats := range got {
		if stats.ETA != nil {
			t.Fatalf("a null eta is unknown: %+v", stats)
		}
	}
}

func TestNoStatsAreAskedWithoutAReporter(t *testing.T) {
	fastPolling(t)

	var polls, asked int32
	client, _ := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/copy":
			_, _ = w.Write([]byte(`{"jobid": 9}`))
		case "/job/status":
			if atomic.AddInt32(&polls, 1) < 3 {
				_, _ = w.Write([]byte(`{"id":9,"finished":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":9,"finished":true,"success":true}`))
		case "/core/stats":
			atomic.AddInt32(&asked, 1)
			_, _ = w.Write([]byte(`{}`))
		}
	})

	if err := client.CopyDirectory(context.Background(), "/DATA/Media", "offsite", "x"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&asked) != 0 {
		t.Fatalf("stats asked %d times with nobody following", asked)
	}
}
