// Package logstore keeps container logs as per-day text files. Standard library only.
package logstore

import (
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Line struct {
	At        time.Time `json:"at"`
	App       string    `json:"app"`
	Service   string    `json:"service"`
	Container string    `json:"container"` // 12 hex chars
	Stream    string    `json:"stream"`    // "o" or "e"
	Text      string    `json:"text"`
	Cursor    string    `json:"cursor,omitempty"` // set by Read only: where the line is, to pass as since or before
}

const MaxLineBytes = 16 << 10
const truncatedMark = " …[tronqué]"

var (
	newlines = strings.NewReplacer("\r", " ", "\n", " ")
	// "err_code" must count as an error, and "_" is a word character, so allow it explicitly after the word.
	errorRe = regexp.MustCompile(`(?i)\b(error|err|fatal|panic)(\b|_)`)
	warnRe  = regexp.MustCompile(`(?i)\bwarn`)
)

// FormatLine returns "<RFC3339Nano> <o|e> <text>\n" with \r and \n in text replaced by spaces.
func FormatLine(at time.Time, stream, text string) []byte {
	return []byte(at.UTC().Format(time.RFC3339Nano) + " " + stream + " " + newlines.Replace(text) + "\n")
}

// ParseLine reads a line produced by FormatLine (without its trailing newline).
func ParseLine(b []byte) (at time.Time, stream, text string, ok bool) {
	ts, rest, found := strings.Cut(string(b), " ")
	if !found || len(rest) < 2 || (rest[0] != 'o' && rest[0] != 'e') || rest[1] != ' ' {
		return time.Time{}, "", "", false
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", "", false
	}
	return at, rest[:1], rest[2:], true
}

func Truncate(text string) string {
	if len(text) <= MaxLineBytes {
		return text
	}
	cut := MaxLineBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) { // do not split a UTF-8 sequence
		cut--
	}
	return text[:cut] + truncatedMark
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func SafeName(s string) string {
	s = unsafeChars.ReplaceAllString(s, "_")
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

func DayFile(root, app, service, container string, day time.Time) string {
	if len(container) > 12 {
		container = container[:12]
	}
	return path.Join(root, SafeName(app), SafeName(service), day.UTC().Format("2006-01-02")+"."+SafeName(container)+".log")
}

func EventsFile(root string, day time.Time) string {
	return path.Join(root, "_events", day.UTC().Format("2006-01-02")+".jsonl")
}

func GuessLevel(stream, text string) string {
	switch {
	case errorRe.MatchString(text):
		return "error"
	case warnRe.MatchString(text), stream == "e":
		return "warn"
	}
	return "info"
}
