package logstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is the single entry point of the package: it owns the writers, the hub and the root directory.
type Store struct {
	Root string
	// OnError, if set before use, receives every writer's flush error (nil = ignored).
	OnError func(error)

	ctx     context.Context
	now     func() time.Time // the clock of the forwards hold-back; a field for the tests
	hub     *Hub
	mu      sync.Mutex
	writers map[string]*Writer
}

// Open creates root if needed, but never its grandparent (/DATA/AppData for /DATA/AppData/casaos/logs):
// while the data disk is missing nothing is written to the system disk, and the caller retries.
// An existing but unmounted mount point cannot be told apart from the disk itself.
// A symlinked root is resolved, so the purge walks the files it points to. ctx bounds the writers' flush loops.
func Open(ctx context.Context, root string) (*Store, error) {
	if _, err := os.Stat(filepath.Dir(filepath.Dir(root))); err != nil {
		return nil, fmt.Errorf("logstore: %s: %w", root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return &Store{Root: root, ctx: ctx, now: time.Now, hub: NewHub(), writers: map[string]*Writer{}}, nil
}

func containerKey(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Writer returns the container's writer, creating and starting it on first use.
func (s *Store) Writer(app, service, container string) *Writer {
	key := containerKey(container)
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.writers[key]; ok {
		return w
	}
	w := NewWriter(s.Root, app, service, key, func(l Line) { s.hub.Publish(Message{T: "line", Line: &l}) })
	w.OnError = func(err error) {
		if s.OnError != nil {
			s.OnError(err)
		}
	}
	s.writers[key] = w
	go w.Run(s.ctx)
	return w
}

// CloseWriter flushes, closes and forgets the container's writer; unknown ids are a no-op.
func (s *Store) CloseWriter(container string) error {
	key := containerKey(container)
	s.mu.Lock()
	w, ok := s.writers[key]
	delete(s.writers, key)
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return w.Close()
}

// Record appends the event to the journal, then publishes it; nothing is published on error.
func (s *Store) Record(e Event) error {
	e.At = e.At.UTC() // one spelling for the journal and the stream, or the UI lists the event twice
	if err := AppendEvent(s.Root, e); err != nil {
		return err
	}
	s.hub.Publish(Message{T: "event", Event: &e})
	return nil
}

// Read flushes every writer first: any line's cursor may start a forwards poll, which must not
// pass a line still in a buffer.
func (s *Store) Read(ctx context.Context, q Query) (Page, error) {
	for _, w := range s.allWriters() {
		_ = w.Flush() // errors go to OnError
	}
	return read(ctx, s.Root, q, s.now())
}

func (s *Store) allWriters() []*Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := make([]*Writer, 0, len(s.writers))
	for _, w := range s.writers {
		ws = append(ws, w)
	}
	return ws
}

func (s *Store) Subscribe(m *Matcher) (<-chan Message, func()) { return s.hub.Subscribe(m) }
func (s *Store) Apps() ([]AppSummary, error)                   { return ListApps(s.Root) }
func (s *Store) Events(q EventQuery) ([]Event, error)          { return ReadEvents(s.Root, q) }

// Purge makes every writer drop its open file afterwards: a removed file still held open would keep
// taking lines no reader sees, on space never freed; the next flush creates the file again.
// The files are also dropped before, so that a system refusing to remove an open file removes it.
func (s *Store) Purge(now time.Time, retentionDays int, maxBytes int64) (int64, error) {
	s.dropFiles()
	n, err := Purge(s.Root, now, retentionDays, maxBytes)
	s.dropFiles()
	return n, err
}

func (s *Store) dropFiles() {
	for _, w := range s.allWriters() {
		w.mu.Lock()
		w.closeFile()
		w.mu.Unlock()
	}
}

func (s *Store) Usage() (int64, time.Time, error) { return Usage(s.Root) }

// Close flushes and closes every writer and returns the first error.
func (s *Store) Close() error {
	s.mu.Lock()
	ws := s.writers
	s.writers = map[string]*Writer{}
	s.mu.Unlock()
	var first error
	for _, w := range ws {
		if err := w.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
