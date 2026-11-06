package logstore

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

func TestALineRoundTrips(t *testing.T) {
	at := time.Date(2026, 11, 5, 23, 59, 59, 123456789, time.UTC)
	b := FormatLine(at, "e", "boom\r\nnext")
	assert.Equal(t, string(b), "2026-11-05T23:59:59.123456789Z e boom  next\n")
	got, stream, text, ok := ParseLine(bytes.TrimSuffix(b, []byte("\n")))
	assert.Assert(t, ok)
	assert.Equal(t, got, at)
	assert.Equal(t, stream, "e")
	assert.Equal(t, text, "boom  next")
	_, _, _, ok = ParseLine([]byte("no timestamp here"))
	assert.Assert(t, !ok)
}

func TestTruncateKeepsSixteenKiB(t *testing.T) {
	long := strings.Repeat("x", MaxLineBytes+5)
	got := Truncate(long)
	assert.Assert(t, len(got) <= MaxLineBytes+len(truncatedMark))
	assert.Assert(t, strings.HasSuffix(got, truncatedMark))
	assert.Equal(t, Truncate("short"), "short")
}

func TestSafeNameNeverLeavesTheRoot(t *testing.T) {
	for in, want := range map[string]string{"jellyfin": "jellyfin", "/My App": "_My_App", "..": "_", ".": "_", "a/../b": "a_.._b", "": "_", "big-bear.v2_1": "big-bear.v2_1"} {
		assert.Equal(t, SafeName(in), want, in)
	}
}

func TestDayFileIsPerAppServiceContainerAndUTCDay(t *testing.T) {
	at := time.Date(2026, 11, 5, 23, 30, 0, 0, time.FixedZone("CET", 3600)) // 22:30 UTC
	assert.Equal(t, DayFile("/r", "jellyfin", "jellyfin", "0123456789abcdef", at), "/r/jellyfin/jellyfin/2026-11-05.0123456789ab.log")
	assert.Equal(t, EventsFile("/r", at), "/r/_events/2026-11-05.jsonl")
}

func TestGuessLevel(t *testing.T) {
	for _, c := range []struct{ stream, text, want string }{
		{"o", "listening on :80", "info"}, {"o", "WARN disk slow", "warn"}, {"o", "[error] no such file", "error"},
		{"o", "panic: nil", "error"}, {"e", "progress 50%", "warn"}, {"e", "FATAL exit", "error"}, {"o", "err_code=0", "error"},
	} {
		assert.Equal(t, GuessLevel(c.stream, c.text), c.want, c.text)
	}
}
