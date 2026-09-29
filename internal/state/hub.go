package state

import (
	"encoding/json"
	"sync"
)

// Event is one change to the supervisor's state, as sent over SSE.
type Event struct {
	ID   uint64          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

const ringSize = 512

// Hub fans events out to subscribers and keeps the most recent ones so a
// client that reconnects with Last-Event-ID can catch up instead of resyncing.
type Hub struct {
	mu   sync.Mutex
	next uint64
	ring []Event
	subs map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{next: 1, subs: map[chan Event]struct{}{}} }

func (h *Hub) publish(typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := Event{ID: h.next, Type: typ, Data: raw}
	h.next++
	h.ring = append(h.ring, ev)
	if len(h.ring) > ringSize {
		h.ring = h.ring[len(h.ring)-ringSize:]
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// Too slow to keep up. Closing tells the handler to end the
			// stream; the client reconnects and replays from Last-Event-ID.
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// Subscribe returns the events after lastID that are still buffered, then a
// channel of live ones. The channel is closed if the subscriber falls behind.
// gap reports that lastID is older than the buffer, so the caller should
// re-read a full snapshot rather than trust the replay.
func (h *Hub) Subscribe(lastID uint64) (replay []Event, live <-chan Event, cancel func(), gap bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lastID > 0 {
		if len(h.ring) > 0 && lastID+1 < h.ring[0].ID {
			gap = true
		}
		for _, ev := range h.ring {
			if ev.ID > lastID {
				replay = append(replay, ev)
			}
		}
	}
	ch := make(chan Event, 128)
	h.subs[ch] = struct{}{}
	return replay, ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}, gap
}

// LastID is the ID of the most recent event, or 0 if none.
func (h *Hub) LastID() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.next - 1
}
