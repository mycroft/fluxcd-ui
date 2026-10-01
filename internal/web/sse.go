package web

import (
	"fmt"
	"net/http"
	"time"
)

// handleEvents streams change notifications as server-sent events. Each event
// is named after a kind ID (or store.TopicChanged), which htmx elements listen
// to with hx-trigger="sse:<name>" before re-fetching their fragment.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (ingress-nginx)

	events, unsubscribe := s.broker.Subscribe()
	defer unsubscribe()

	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case topic, ok := <-events:
			if !ok {
				return // broker closed: server shutting down
			}
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", topic, topic)
		case <-heartbeat.C:
			_, err = fmt.Fprint(w, ": ping\n\n")
		}
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}
