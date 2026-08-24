package content

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const eventHistoryLimit = 512

type Event struct {
	Sequence uint64          `json:"sequence"`
	Type     string          `json:"type"`
	ID       string          `json:"id,omitempty"`
	OldID    string          `json:"oldId,omitempty"`
	Item     json.RawMessage `json:"item,omitempty"`
}

// EventHub owns event sequencing, replay and slow-client reconciliation.
type EventHub struct {
	mu       sync.Mutex
	clients  map[chan string]struct{}
	sequence uint64
	history  []Event
}

func NewEventHub() *EventHub { return &EventHub{clients: map[chan string]struct{}{}} }

func (h *EventHub) Sequence() uint64 { h.mu.Lock(); defer h.mu.Unlock(); return h.sequence }

func (h *EventHub) Publish(event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sequence++
	event.Sequence = h.sequence
	h.history = append(h.history, event)
	if len(h.history) > eventHistoryLimit {
		h.history = append([]Event(nil), h.history[len(h.history)-eventHistoryLimit:]...)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	for client := range h.clients {
		select {
		case client <- string(payload):
		default:
			select {
			case <-client:
			default:
			}
			reconcile, _ := json.Marshal(Event{Sequence: event.Sequence, Type: "reconcile"})
			select {
			case client <- string(reconcile):
			default:
			}
		}
	}
}

func (h *EventHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	messages := make(chan string, 64)
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	h.mu.Lock()
	h.clients[messages] = struct{}{}
	current := h.sequence
	replay := append([]Event(nil), h.history...)
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.clients, messages); h.mu.Unlock() }()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	connected, _ := json.Marshal(Event{Sequence: current, Type: "connected"})
	fmt.Fprintf(w, "data: %s\n\n", connected)
	if since > 0 && since < current {
		oldest := current + 1
		if len(replay) > 0 {
			oldest = replay[0].Sequence
		}
		if since+1 < oldest {
			payload, _ := json.Marshal(Event{Sequence: current, Type: "reconcile"})
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", current, payload)
		} else {
			for _, event := range replay {
				if event.Sequence > since {
					payload, _ := json.Marshal(event)
					fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, payload)
				}
			}
		}
	}
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case message := <-messages:
			var event Event
			_ = json.Unmarshal([]byte(message), &event)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, message)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}
