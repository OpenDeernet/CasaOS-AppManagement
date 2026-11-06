package logstore

import "sync"

type Message struct {
	T       string `json:"t"` // "line" | "event" | "lag"
	Line    *Line  `json:"line,omitempty"`
	Event   *Event `json:"event,omitempty"`
	Dropped int    `json:"dropped,omitempty"`
}

// QueueSize is the per-subscriber backlog; beyond it the oldest message is dropped.
const QueueSize = 10_000

// Hub fans messages out to subscribers. Publish never blocks: each subscriber has its own bounded queue.
type Hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	match *Matcher
	out   chan Message
	wake  chan struct{} // 1 slot: "state changed"
	done  chan struct{}

	// All fields below are guarded by Hub.mu.
	q       []Message // q[head:] is the queue
	head    int
	popped  uint64 // number of messages ever removed from the head; identifies the head message
	dropped int    // drops not yet reported by a lag message
	closed  bool
}

func NewHub() *Hub { return &Hub{subs: map[*subscriber]struct{}{}} }

func (h *Hub) Publish(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		switch {
		case m.T == "line" && m.Line != nil:
			if s.match != nil && !s.match.Match(*m.Line) {
				continue
			}
		case m.T == "event" && m.Event != nil:
			if s.match != nil && !s.match.MatchEvent(*m.Event) {
				continue
			}
		}
		if len(s.q)-s.head == QueueSize {
			s.q[s.head] = Message{}
			s.head++
			s.popped++
			s.dropped++
		}
		if s.head >= QueueSize { // compact so the backing array stays bounded
			s.q = append(s.q[:0], s.q[s.head:]...)
			s.head = 0
		}
		s.q = append(s.q, m)
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) Subscribe(m *Matcher) (<-chan Message, func()) {
	s := &subscriber{match: m, out: make(chan Message), wake: make(chan struct{}, 1), done: make(chan struct{})}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	go h.drain(s)
	var once sync.Once
	return s.out, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			s.closed = true
			h.mu.Unlock()
			close(s.done)
		})
	}
}

// drain is the only goroutine that blocks on s.out. It offers the head of the queue (or a pending lag) and, whenever
// Publish signals a change, re-picks it, so a reader never gets a message that a later drop has made stale.
func (h *Hub) drain(s *subscriber) {
	defer close(s.out)
	for {
		h.mu.Lock()
		if s.closed {
			h.mu.Unlock()
			return
		}
		var item Message
		var id uint64
		lag := s.dropped > 0
		switch {
		case lag:
			item = Message{T: "lag", Dropped: s.dropped}
		case len(s.q) > s.head:
			item, id = s.q[s.head], s.popped
		default:
			h.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.done:
				return
			}
		}
		h.mu.Unlock()

		select { // a pending change wins over a ready reader
		case <-s.wake:
			continue
		default:
		}
		select {
		case s.out <- item:
		case <-s.wake:
			continue
		case <-s.done:
			return
		}

		h.mu.Lock()
		if lag {
			s.dropped -= item.Dropped
		} else if s.popped != id {
			s.dropped-- // the message just sent had been counted as dropped
		} else if len(s.q) > s.head {
			s.q[s.head] = Message{}
			s.head++
			s.popped++
			if s.head == len(s.q) { // empty: reuse the array from the start
				s.q, s.head = s.q[:0], 0
			}
		}
		h.mu.Unlock()
	}
}
