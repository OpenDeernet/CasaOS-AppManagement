package logstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

type Event struct {
	At        time.Time `json:"at"`
	App       string    `json:"app"`
	Service   string    `json:"service"`
	Container string    `json:"container"`
	Kind      string    `json:"kind"`
	ExitCode  string    `json:"exit_code,omitempty"`
	FromImage string    `json:"from_image,omitempty"`
	ToImage   string    `json:"to_image,omitempty"`
	By        string    `json:"by"` // "operation" | "docker"
	Detail    string    `json:"detail,omitempty"`
}

var Kinds = []string{"created", "started", "stopped", "died", "oom-killed", "restarting", "unhealthy", "healthy", "installed", "uninstalled", "updated", "image-pulled"}

type EventQuery struct {
	Apps  []string
	Kinds []string
	Since time.Time // zero = no bound
	Until time.Time // zero = no bound
	Limit int       // ≤ 1000, default 500
}

const (
	defaultEventLimit = 500
	maxEventLimit     = 1000
)

// AppendEvent writes the event as one line in a single Write call, so concurrent appenders do not interleave.
func AppendEvent(root string, e Event) error {
	e.At = e.At.UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	name := EventsFile(root, e.At)
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return fmt.Errorf("logstore: %s: %w", name, err)
	}
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("logstore: %s: %w", name, err)
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("logstore: %s: %w", name, err)
	}
	return nil
}

// ReadEvents returns events newest first. Day files outside the Since/Until days are never opened.
func ReadEvents(root string, q EventQuery) ([]Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultEventLimit
	}
	limit = min(limit, maxEventLimit)

	files, err := filepath.Glob(path.Join(root, "_events", "????-??-??.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files))) // names are UTC days: newest first

	sinceDay, untilDay := "", ""
	if !q.Since.IsZero() {
		sinceDay = q.Since.UTC().Format("2006-01-02")
	}
	if !q.Until.IsZero() {
		untilDay = q.Until.UTC().Format("2006-01-02")
	}

	out := []Event{}
	for _, name := range files {
		day := filepath.Base(name)[:len("2006-01-02")]
		if (sinceDay != "" && day < sinceDay) || (untilDay != "" && day > untilDay) {
			continue
		}
		data, err := os.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue // purged since the Glob
		}
		if err != nil {
			return nil, err
		}
		lines := bytes.Split(data, []byte("\n"))
		for i := len(lines) - 1; i >= 0; i-- {
			var e Event
			if json.Unmarshal(lines[i], &e) != nil { // corrupt or empty line
				continue
			}
			if (len(q.Apps) > 0 && !slices.Contains(q.Apps, e.App)) ||
				(len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind)) ||
				(!q.Since.IsZero() && e.At.Before(q.Since)) ||
				(!q.Until.IsZero() && e.At.After(q.Until)) {
				continue
			}
			out = append(out, e)
			if len(out) == limit {
				return out, nil
			}
		}
	}
	return out, nil
}
