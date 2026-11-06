package logstore

import (
	"fmt"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

func recv(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case m, ok := <-ch:
		assert.Assert(t, ok, "channel closed")
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no message")
		return Message{}
	}
}

func lineMsg(app, text string) Message {
	return Message{T: "line", Line: &Line{App: app, Text: text}}
}

func TestAFilteredSubscriberSeesOnlyItsLines(t *testing.T) {
	h := NewHub()
	m, err := Filter{Apps: []string{"plex"}}.Compile()
	assert.NilError(t, err)
	ch, cancel := h.Subscribe(m)
	defer cancel()

	h.Publish(lineMsg("jellyfin", "no"))
	h.Publish(Message{T: "event", Event: &Event{App: "jellyfin", Kind: "died"}})
	h.Publish(lineMsg("plex", "yes"))
	h.Publish(Message{T: "event", Event: &Event{App: "plex", Kind: "started"}})

	assert.Equal(t, recv(t, ch).Line.Text, "yes")
	assert.Equal(t, recv(t, ch).Event.Kind, "started")
}

func TestASlowSubscriberGetsALagNotLost(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe(nil)
	defer cancel()
	for i := 0; i < QueueSize+5; i++ {
		h.Publish(lineMsg("a", fmt.Sprint(i)))
	}

	h.waitSettled(t)
	first := recv(t, ch)
	assert.Equal(t, first.T, "lag")
	assert.Equal(t, first.Dropped, 5)
	for i := 5; i < QueueSize+5; i++ {
		assert.Equal(t, recv(t, ch).Line.Text, fmt.Sprint(i))
	}
}

func TestPublishNeverBlocksOnASlowSubscriber(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe(nil)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 3*QueueSize; i++ {
			h.Publish(lineMsg("a", "x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked")
	}
}

func TestCancelClosesTheChannel(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe(nil)
	cancel()
	cancel() // idempotent
	h.Publish(lineMsg("a", "x"))
	select {
	case _, ok := <-ch:
		assert.Assert(t, !ok)
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed")
	}
}

// waitSettled waits until every drain goroutine has consumed its wake signal after the last Publish,
// so that what it offers next is the final state and not a stale head.
func (h *Hub) waitSettled(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		h.mu.Lock()
		ok := true
		for s := range h.subs {
			ok = ok && len(s.q)-s.head == QueueSize && len(s.wake) == 0
		}
		h.mu.Unlock()
		if ok {
			return
		}
	}
	t.Fatal("drain never settled")
}
