package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/tanq16/local-content-share/internal/transfers"
)

var fileTransfers *transfers.Manager

func initFileTransfers() error {
	manager, err := transfers.NewManager("data", maxFileSize, maxURLDownloadSize, func(storage, expiry, preferredID string) error {
		if _, err := contentLifecycle.Add(storage, preferredID); err != nil {
			return err
		}
		if expiry != "" && expiry != "Never" {
			expirationTracker.SetExpiration(storage, expiry)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fileTransfers = manager
	recovered, err := manager.Recover()
	if err != nil {
		return err
	}
	for _, file := range recovered {
		if entry, entryErr := fileEntry(file.Storage); entryErr == nil {
			notifyContentItem("created", entry)
		}
	}
	return nil
}

func publicDownloadURL(raw string) (*url.URL, error) { return transfers.PublicURL(raw) }
func publicOnlyTransport() *http.Transport           { return transfers.PublicTransport() }
func generateUniqueFilename(baseDir, baseName string) string {
	return transfers.UniqueFilename(baseDir, baseName)
}

func streamUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	files, err := fileTransfers.SaveMultipart(strings.TrimSpace(r.Header.Get("Idempotency-Key")), r.Header.Get("Content-Type"), r.Body, "", "")
	if err != nil {
		log.Printf("Stream upload failed from %s: %v", r.RemoteAddr, err)
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "exceeds 4 GB") {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	entries := make([]Entry, 0, len(files))
	for _, file := range files {
		entry, entryErr := fileEntry(file.Storage)
		if entryErr != nil {
			http.Error(w, entryErr.Error(), 500)
			return
		}
		entries = append(entries, *entry)
		notifyContentItem("created", entry)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "items": entries})
}

func runURLDownloadTask(ctx context.Context, id string, source *url.URL, requestedName, expiry string) {
	runURLDownloadTaskWithClient(ctx, id, source, requestedName, expiry, publicOnlyTransport())
}
func runURLDownloadTaskWithClient(ctx context.Context, id string, source *url.URL, requestedName, expiry string, transport http.RoundTripper) {
	downloadTasks.update(id, func(task *DownloadTask) { task.Status = "downloading" })
	file, err := fileTransfers.Download(ctx, source, requestedName, expiry, transport, func(update transfers.DownloadUpdate) {
		downloadTasks.update(id, func(task *DownloadTask) {
			task.Filename = update.Filename
			task.Received = update.Received
			task.Total = update.Total
		})
	})
	if err != nil {
		failURLDownloadTask(id, err)
		return
	}
	entry, err := fileEntry(file.Storage)
	if err != nil {
		failURLDownloadTask(id, err)
		return
	}
	downloadTasks.update(id, func(task *DownloadTask) {
		task.Status = "completed"
		task.Filename = file.Filename
		task.Received = file.Size
		if task.Total < 0 {
			task.Total = file.Size
		}
		task.Item = entry
		task.cancel = nil
	})
	scheduleDownloadTaskCleanup(id)
	notifyContentItem("created", entry)
}
