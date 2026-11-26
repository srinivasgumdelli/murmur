package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/memory"
)

type memorySubscription struct {
	scopes map[string]bool
	events chan memory.Event
}

func (n *Notifier) PublishMemory(event memory.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for sub := range n.memorySubscribers {
		if len(sub.scopes) > 0 && !sub.scopes[event.Scope] && !sub.scopes[event.OriginScope] {
			continue
		}
		select {
		case sub.events <- event:
		default:
			// A slow consumer can reconnect and refresh its scopes over HTTP.
		}
	}
}

func (n *Notifier) subscribeMemory(scopes []string) (*memorySubscription, func()) {
	sub := &memorySubscription{scopes: make(map[string]bool), events: make(chan memory.Event, 32)}
	for _, scope := range scopes {
		sub.scopes[scope] = true
	}
	n.mu.Lock()
	n.memorySubscribers[sub] = struct{}{}
	n.mu.Unlock()
	return sub, func() {
		n.mu.Lock()
		delete(n.memorySubscribers, sub)
		n.mu.Unlock()
	}
}

func MemoryStream(n *Notifier, authMode string, messages http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("types") != "memory" {
			messages(w, r)
			return
		}
		if authMode != "required" {
			memoryError(w, memory.ErrAuthRequired)
			return
		}
		p := principalFromContext(r.Context())
		if p.agent == "" && !p.admin {
			memoryError(w, memory.ErrUnauthorized)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		sub, unsubscribe := n.subscribeMemory(splitQuery(r.URL.Query()["scope"]))
		defer unsubscribe()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher.Flush()

		heartbeat := time.NewTicker(30 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-sub.events:
				data, _ := json.Marshal(event)
				if _, err := fmt.Fprintf(w, "event: memory\ndata: %s\n\n", data); err != nil {
					return
				}
				flusher.Flush()
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}
