package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const heartbeat = 15 * time.Second

// events streams state changes as Server-Sent Events.
//
// A new client (or one that fell too far behind to replay) first receives a
// "snapshot" event holding the full state, then a typed event per change.
// A reconnecting client sends Last-Event-ID and receives just what it missed.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	last, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	replay, live, cancel, gap := s.State.Hub.Subscribe(last)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if last == 0 || gap {
		replay = nil
		payload, _ := json.Marshal(s.snapshot())
		_, _ = fmt.Fprintf(w, "id: %d\nevent: snapshot\ndata: %s\n\n", s.State.Hub.LastID(), payload)
	}
	for _, ev := range replay {
		writeEvent(w, ev.ID, ev.Type, ev.Data)
	}
	if rc.Flush() != nil {
		return
	}

	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-live:
			if !ok {
				return // fell behind; the client reconnects and replays
			}
			writeEvent(w, ev.ID, ev.Type, ev.Data)
		case <-tick.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
		}
		if rc.Flush() != nil {
			return
		}
	}
}

func writeEvent(w http.ResponseWriter, id uint64, typ string, data []byte) {
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, typ, data)
}
