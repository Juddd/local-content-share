package content

import (
	"encoding/json"
	"testing"
)

func TestEventHubKeepsContinuousBoundedHistory(t *testing.T) {
	hub := NewEventHub()
	for sequence := 1; sequence <= eventHistoryLimit+3; sequence++ {
		hub.Publish(Event{Type: "updated", ID: "item"})
	}
	if hub.Sequence() != eventHistoryLimit+3 {
		t.Fatalf("unexpected sequence: %d", hub.Sequence())
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.history) != eventHistoryLimit {
		t.Fatalf("unexpected history length: %d", len(hub.history))
	}
	if hub.history[0].Sequence != 4 || hub.history[len(hub.history)-1].Sequence != eventHistoryLimit+3 {
		t.Fatalf("history is not continuous: first=%d last=%d", hub.history[0].Sequence, hub.history[len(hub.history)-1].Sequence)
	}
}

func TestSlowEventClientReceivesReconcileAtLatestSequence(t *testing.T) {
	hub := NewEventHub()
	client := make(chan string, 1)
	client <- "stale"
	hub.clients[client] = struct{}{}
	hub.Publish(Event{Type: "created", ID: "item"})
	var event Event
	if err := json.Unmarshal([]byte(<-client), &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "reconcile" || event.Sequence != 1 {
		t.Fatalf("unexpected slow-client event: %#v", event)
	}
}
