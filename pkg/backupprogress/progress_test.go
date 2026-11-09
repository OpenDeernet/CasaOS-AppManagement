package backupprogress

import (
	"context"
	"encoding/json"
	"testing"
)

func TestReportReachesTheCallbackOfTheContext(t *testing.T) {
	var got []Stats
	ctx := With(context.Background(), func(stats Stats) { got = append(got, stats) })

	eta := int64(3390)
	sent := Stats{Bytes: 5, TotalBytes: 10, Files: 1, TotalFiles: 2, Speed: 3, ETA: &eta, Current: "a.jpg"}
	Report(ctx, sent)

	if !Wanted(ctx) {
		t.Fatal("a context with a callback wants stats")
	}
	if len(got) != 1 || got[0].Bytes != 5 || got[0].TotalFiles != 2 || *got[0].ETA != 3390 || got[0].Current != "a.jpg" {
		t.Fatalf("got %+v", got)
	}
}

func TestReportWithoutACallbackDoesNothing(t *testing.T) {
	Report(context.Background(), Stats{Bytes: 1})

	if Wanted(context.Background()) {
		t.Fatal("a bare context wants nothing")
	}
}

func TestAZeroTotalIsLeftOutOfTheJSON(t *testing.T) {
	encoded, err := json.Marshal(Stats{Bytes: 5})
	if err != nil {
		t.Fatal(err)
	}

	if string(encoded) != `{"bytes":5,"files":0}` {
		t.Fatalf("got %s", encoded)
	}
}
