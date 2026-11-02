package rclone

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestListingOneLevelInsideACopy(t *testing.T) {
	client, seen := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"list":[
			{"Name":"notes.txt","Size":12,"ModTime":"2026-10-01T09:00:00Z","IsDir":false},
			{"Name":"docs","Size":-1,"ModTime":"2026-10-02T09:00:00Z","IsDir":true}
		]}`))
	})

	files, err := client.List(context.Background(), "offsite", "nextcloud/run/volumes/data", "docs/old")
	if err != nil {
		t.Fatal(err)
	}
	if got := (*seen)[0].PostForm.Get("remote"); got != "nextcloud/run/volumes/data/docs/old" {
		t.Fatalf("listed %q", got)
	}
	if len(files) != 2 || files[0].Size != 12 || files[0].Directory || !files[1].Directory || files[1].Size != 0 {
		t.Fatalf("%+v", files)
	}
}

func TestTakingAFileBackCopiesThatFile(t *testing.T) {
	fastPolling(t)

	client, seen := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/operations/stat":
			_, _ = w.Write([]byte(`{"item":{"IsDir":false}}`))
		case "/operations/copyfile":
			_, _ = w.Write([]byte(`{"jobid": 3}`))
		case "/job/status":
			_, _ = w.Write([]byte(`{"id":3,"finished":true,"success":true}`))
		}
	})

	if err := client.CopyBack(context.Background(), "offsite", "app/run/volumes/data", "docs/a.txt", "/host/data/docs/a.txt"); err != nil {
		t.Fatal(err)
	}

	copied := (*seen)[1]
	if copied.URL.Path != "/operations/copyfile" || copied.PostForm.Get("srcRemote") != "a.txt" || copied.PostForm.Get("dstFs") != "/host/data/docs" {
		t.Fatalf("%s %v", copied.URL.Path, copied.PostForm)
	}
}

// sync/sync would delete what is in the folder now; this is a few things taken back.
func TestTakingAFolderBackCopiesItAndDeletesNothing(t *testing.T) {
	fastPolling(t)

	client, seen := serverFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/operations/stat":
			_, _ = w.Write([]byte(`{"item":{"IsDir":true}}`))
		case "/sync/copy":
			_, _ = w.Write([]byte(`{"jobid": 4}`))
		case "/job/status":
			_, _ = w.Write([]byte(`{"id":4,"finished":true,"success":true}`))
		}
	})

	if err := client.CopyBack(context.Background(), "offsite", "app/run/volumes/data", "docs", "/host/data/docs"); err != nil {
		t.Fatal(err)
	}

	for _, request := range *seen {
		if request.URL.Path == "/sync/sync" {
			t.Fatal("a folder taken back is copied, never synced")
		}
	}
	start := (*seen)[1].PostForm
	if start.Get("srcFs") != "casaos-backup-offsite:app/run/volumes/data/docs" || start.Get("dstFs") != "/host/data/docs" {
		t.Fatalf("%v", start)
	}
}

func TestTakingBackWhatTheDestinationDoesNotHold(t *testing.T) {
	client, _ := serverFor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"item":null}`))
	})

	err := client.CopyBack(context.Background(), "offsite", "app/run/volumes/data", "gone", "/host/gone")

	var missing *NotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("err %v", err)
	}
}
