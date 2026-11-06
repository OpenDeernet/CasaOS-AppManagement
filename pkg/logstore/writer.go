package logstore

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const flushThreshold = 64 << 10

// Writer appends one container's lines to its per-day files through a buffer.
type Writer struct {
	root, app, service, container string // container is already cut to 12 chars
	onLine                        func(Line)

	// OnError, if set before use, receives every flush error (nil = ignored). The failed buffer is dropped.
	OnError func(error)

	mu     sync.Mutex
	buf    bytes.Buffer
	file   *os.File
	day    string // UTC day of buf and file, "2006-01-02"
	closed bool
}

// onLine runs synchronously inside Write and must stay cheap; nil is allowed.
func NewWriter(root, app, service, container string, onLine func(Line)) *Writer {
	if len(container) > 12 {
		container = container[:12]
	}
	return &Writer{root: root, app: app, service: service, container: container, onLine: onLine}
}

// Write always buffers the line and always calls onLine. A flush error (day change or 64 KiB) is
// returned and sent to OnError; it concerns earlier lines only.
func (w *Writer) Write(at time.Time, stream, text string) error {
	text = Truncate(text)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("logstore: write on closed writer")
	}
	var err error
	day := at.UTC().Format("2006-01-02")
	if day != w.day {
		err = w.flushLocked()
		w.closeFile()
		w.day = day
	}
	w.buf.Write(FormatLine(at, stream, text))
	if w.buf.Len() >= flushThreshold {
		err = cmp.Or(err, w.flushLocked())
	}
	w.mu.Unlock()
	if w.onLine != nil { // outside the mutex: a slow or re-entrant callback must not block the writer
		w.onLine(Line{At: at, App: w.app, Service: w.service, Container: w.container, Stream: stream, Text: newlines.Replace(text)})
	}
	w.report(err)
	return err
}

func (w *Writer) report(err error) {
	if err != nil && w.OnError != nil {
		w.OnError(err)
	}
}

// Flush does not fsync: the 1 s crash window is accepted by the spec.
func (w *Writer) Flush() error {
	w.mu.Lock()
	err := w.flushLocked()
	w.mu.Unlock()
	w.report(err)
	return err
}

func (w *Writer) flushLocked() error {
	if w.buf.Len() == 0 {
		return nil
	}
	day, _ := time.Parse("2006-01-02", w.day)
	name := DayFile(w.root, w.app, w.service, w.container, day)
	if w.file == nil {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			w.buf.Reset()
			return fmt.Errorf("logstore: %s: %w", name, err)
		}
		f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			w.buf.Reset()
			return fmt.Errorf("logstore: %s: %w", name, err)
		}
		w.file = f
	}
	_, err := w.file.Write(w.buf.Bytes())
	w.buf.Reset() // dropped even on error: retrying a failing disk would grow the buffer without bound
	if err != nil {
		return fmt.Errorf("logstore: %s: %w", name, err)
	}
	return nil
}

func (w *Writer) closeFile() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
}

// Run does not close the writer when ctx ends; the caller still calls Close.
func (w *Writer) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = w.Flush()
		case <-ctx.Done():
			_ = w.Flush()
			return
		}
	}
}

func (w *Writer) Close() error {
	w.mu.Lock()
	err := w.flushLocked()
	w.closeFile()
	w.closed = true
	w.mu.Unlock()
	w.report(err)
	return err
}

// LastTimestamp returns the time of the last parsable line of the container's newest day file.
func LastTimestamp(root, app, service, container string) (time.Time, bool) {
	if len(container) > 12 {
		container = container[:12]
	}
	files, err := filepath.Glob(path.Join(root, SafeName(app), SafeName(service), "*."+SafeName(container)+".log"))
	if err != nil || len(files) == 0 {
		return time.Time{}, false
	}
	sort.Strings(files)
	at := lastLineAt(files[len(files)-1])
	return at, !at.IsZero()
}

// lastLineAt returns the time of the file's last parsable line within its last 64 KiB, zero if none.
func lastLineAt(name string) time.Time {
	f, err := os.Open(name)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return time.Time{}
	}
	start := max(st.Size()-flushThreshold, 0)
	tail := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}
	}
	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if i == 0 && start > 0 {
			break // first chunk line is cut mid-line
		}
		if at, _, _, ok := ParseLine(lines[i]); ok {
			return at
		}
	}
	return time.Time{}
}
