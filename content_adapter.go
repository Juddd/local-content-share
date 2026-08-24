package main

import (
	"encoding/json"
	"net/http"
	"time"

	contentmodel "github.com/tanq16/local-content-share/internal/content"
)

type IdentityRecord = contentmodel.Identity

var contentLifecycle *contentmodel.Manager
var contentEvents = contentmodel.NewEventHub()
var errRevisionConflict = contentmodel.ErrRevisionConflict

func initContentLifecycle(root string) error {
	manager, err := contentmodel.NewManager(root)
	if err == nil {
		contentLifecycle = manager
	}
	return err
}

func stableEntry(entry *Entry) *Entry {
	if entry == nil || contentLifecycle == nil {
		return entry
	}
	fallback := entry.ModifiedAt
	if fallback.IsZero() {
		fallback = time.Now()
	}
	snapshot := contentLifecycle.View(entry.ID, fallback)
	entry.ID = snapshot.ID
	entry.StorageID = snapshot.Storage
	entry.Revision = snapshot.Revision
	entry.CreatedAt = snapshot.CreatedAt
	entry.ModifiedAt = snapshot.ModifiedAt
	entry.Favorite = snapshot.Favorite
	return entry
}

func resolveStorageID(value string) string {
	if contentLifecycle == nil {
		return value
	}
	return contentLifecycle.ResolveStorage(value)
}
func isStableUUID(value string) bool { return contentmodel.IsStableID(value) }

func handleContentUpdates(w http.ResponseWriter, r *http.Request) { contentEvents.ServeHTTP(w, r) }
func notifyContentChange()                                        { contentEvents.Publish(contentmodel.Event{Type: "reconcile"}) }
func notifyContentDelete(id string) {
	contentEvents.Publish(contentmodel.Event{Type: "deleted", ID: id})
}
func notifyContentItem(eventType string, item *Entry) {
	if item == nil {
		notifyContentChange()
		return
	}
	payload, _ := json.Marshal(item)
	contentEvents.Publish(contentmodel.Event{Type: eventType, ID: item.ID, Item: payload})
}
func notifyContentRename(oldID string, item *Entry) {
	if item == nil {
		notifyContentChange()
		return
	}
	payload, _ := json.Marshal(item)
	contentEvents.Publish(contentmodel.Event{Type: "renamed", ID: item.ID, OldID: oldID, Item: payload})
}
