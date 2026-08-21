package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const eventHistoryLimit = 512

var (
	clients       = make(map[chan string]bool)
	clientMux     sync.Mutex
	eventSequence uint64
	eventHistory  []ContentEvent
)

type ContentEvent struct {
	Sequence uint64 `json:"sequence"`
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	OldID    string `json:"oldId,omitempty"`
	Item     *Entry `json:"item,omitempty"`
}

func handleContentUpdates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	messageChan := make(chan string, 64)
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	clientMux.Lock()
	clients[messageChan] = true
	currentSequence := eventSequence
	replay := append([]ContentEvent(nil), eventHistory...)
	clientMux.Unlock()
	defer func() { clientMux.Lock(); delete(clients, messageChan); clientMux.Unlock(); close(messageChan) }()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	connected, _ := json.Marshal(ContentEvent{Sequence: currentSequence, Type: "connected"})
	fmt.Fprintf(w, "data: %s\n\n", connected)
	if since > 0 && since < currentSequence {
		oldest := currentSequence + 1
		if len(replay) > 0 {
			oldest = replay[0].Sequence
		}
		if since+1 < oldest {
			payload, _ := json.Marshal(ContentEvent{Sequence: currentSequence, Type: "reconcile"})
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", currentSequence, payload)
		} else {
			for _, event := range replay {
				if event.Sequence > since {
					payload, _ := json.Marshal(event)
					fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, payload)
				}
			}
		}
	}
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-messageChan:
			var event ContentEvent
			_ = json.Unmarshal([]byte(msg), &event)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, msg)
			w.(http.Flusher).Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func notifyContentChange()          { notifyStructuredEvent(ContentEvent{Type: "reconcile"}) }
func notifyContentDelete(id string) { notifyStructuredEvent(ContentEvent{Type: "deleted", ID: id}) }
func notifyContentItem(eventType string, item *Entry) {
	if item == nil {
		notifyContentChange()
		return
	}
	notifyStructuredEvent(ContentEvent{Type: eventType, ID: item.ID, Item: item})
}
func notifyContentRename(oldID string, item *Entry) {
	if item == nil {
		notifyContentChange()
		return
	}
	notifyStructuredEvent(ContentEvent{Type: "renamed", ID: item.ID, OldID: oldID, Item: item})
}
func notifyStructuredEvent(event ContentEvent) {
	clientMux.Lock()
	defer clientMux.Unlock()
	eventSequence++
	event.Sequence = eventSequence
	eventHistory = append(eventHistory, event)
	if len(eventHistory) > eventHistoryLimit {
		eventHistory = append([]ContentEvent(nil), eventHistory[len(eventHistory)-eventHistoryLimit:]...)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	for client := range clients {
		select {
		case client <- string(payload):
		default:
			select {
			case <-client:
			default:
			}
			reconcile, _ := json.Marshal(ContentEvent{Sequence: event.Sequence, Type: "reconcile"})
			select {
			case client <- string(reconcile):
			default:
			}
		}
	}
}
