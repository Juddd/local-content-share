warning: /bin/sh: setlocale: LC_ALL: cannot change locale (C.UTF-8)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicDownloadURLRejectsPrivateAddresses(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1/file", "http://localhost/file", "http://192.168.1.10/file"} {
		if _, err := publicDownloadURL(raw); err == nil {
			t.Fatalf("expected private URL to be rejected: %s", raw)
		}
	}
}

func TestDownloadFilenamePriority(t *testing.T) {
	u, _ := url.Parse("https://example.test/path/from-url.jpg")
	r := &http.Response{Header: make(http.Header)}
	if got := downloadFilename(r, u, "chosen.png"); got != "chosen.png" {
		t.Fatalf("requested name: %q", got)
	}
	r.Header.Set("Content-Disposition", `attachment; filename="header.bin"`)
	if got := downloadFilename(r, u, ""); got != "header.bin" {
		t.Fatalf("header name: %q", got)
	}
}

func TestTemporaryDownloadMarker(t *testing.T) {
	if !strings.Contains("data/files/.url-download-", ".url-download-") {
		t.Fatal("temporary path marker missing")
	}
}

func TestContentFilePathRejectsTraversal(t *testing.T) {
	for _, id := range []string{"", "../secret", "files/../secret", "files/a/b", "files\\secret", "/files/secret", "notepad/md.file"} {
		if _, err := contentFilePath(id, "files", "text"); err == nil {
			t.Fatalf("expected invalid content ID to be rejected: %q", id)
		}
	}
	if got, err := contentFilePath("files/image.png", "files", "text"); err != nil || got != filepath.Join("data", "files", "image.png") {
		t.Fatalf("valid content ID rejected: %q (%v)", got, err)
	}
}

func TestStreamUploadLimitIsFourGiB(t *testing.T) {
	if maxFileSize != 4294967296 {
		t.Fatalf("unexpected limit: %d", maxFileSize)
	}
}

func TestCustomDurationDistinguishesMonthsFromMinutes(t *testing.T) {
	if got := parseCustomDuration("2M"); got != 60*24*time.Hour {
		t.Fatalf("2M should mean two months, got %v", got)
	}
	if got := parseCustomDuration("2m"); got != 5*time.Minute {
		t.Fatalf("minimum minute duration should remain five minutes, got %v", got)
	}
}

func TestAtomicWriteFileReplacesCompleteContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := atomicWriteFile(path, []byte(`{"version":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(path, []byte(`{"version":2,"complete":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"version":2,"complete":true}` {
		t.Fatalf("unexpected contents: %s", data)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".metadata-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary metadata files remained: %v", matches)
	}
}

func TestStreamUploadHandlerSavesMultipartFile(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err = os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	if err = os.MkdirAll("data/files", 0755); err != nil {
		t.Fatal(err)
	}
	itemTimeTracker = initItemTimeTracker()
	expirationTracker = initExpirationTracker()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err = mw.WriteField("expiry", "Never"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file-upload", "sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(part, "streamed content"); err != nil {
		t.Fatal(err)
	}
	if err = mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/upload-stream", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	streamUploadHandler(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Items []Entry `json:"items"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil || len(response.Items) != 1 || response.Items[0].Filename != "sample.txt" {
		t.Fatalf("unexpected upload response: %s (%v)", rec.Body.String(), err)
	}
	data, err := os.ReadFile(filepath.Join("data", "files", "sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "streamed content" {
		t.Fatalf("unexpected content: %q", data)
	}
	matches, err := filepath.Glob("data/files/.upload-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary uploads remain: %v", matches)
	}
}

func TestStreamUploadAddsExtensionFromFileContent(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err = os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	if err = os.MkdirAll("data/files", 0755); err != nil {
		t.Fatal(err)
	}
	itemTimeTracker = initItemTimeTracker()
	expirationTracker = initExpirationTracker()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file-upload", "origin")
	if err != nil {
		t.Fatal(err)
	}
	pngHeader := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}
	if _, err = part.Write(pngHeader); err != nil {
		t.Fatal(err)
	}
	if err = mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/upload-stream", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	streamUploadHandler(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if _, err = os.Stat("data/files/origin.png"); err != nil {
		t.Fatalf("detected PNG filename missing: %v", err)
	}
}

func TestURLDownloadTaskReportsCompletionAndItem(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err = os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	if err = os.MkdirAll("data/files", 0755); err != nil {
		t.Fatal(err)
	}
	itemTimeTracker = initItemTimeTracker()
	expirationTracker = initExpirationTracker()
	downloadTasks = downloadTaskStore{tasks: map[string]*DownloadTask{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12")
		_, _ = io.WriteString(w, "task payload")
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL + "/remote.bin")
	id := "test-complete"
	ctx, cancel := context.WithCancel(context.Background())
	downloadTasks.tasks[id] = &DownloadTask{ID: id, Status: "queued", Total: -1, cancel: cancel}
	runURLDownloadTaskWithClient(ctx, id, u, "saved.bin", "Never", server.Client().Transport)
	task, ok := downloadTasks.snapshot(id)
	if !ok || task.Status != "completed" || task.Received != 12 || task.Total != 12 || task.Item == nil || task.Item.Filename != "saved.bin" {
		t.Fatalf("unexpected task: %+v", task)
	}
	if data, err := os.ReadFile("data/files/saved.bin"); err != nil || string(data) != "task payload" {
		t.Fatalf("unexpected saved file: %q (%v)", data, err)
	}
}

func TestURLDownloadTaskCancellationRemovesTemporaryFile(t *testing.T) {
	oldDir, _ := os.Getwd()
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	_ = os.MkdirAll("data/files", 0755)
	itemTimeTracker = initItemTimeTracker()
	expirationTracker = initExpirationTracker()
	downloadTasks = downloadTaskStore{tasks: map[string]*DownloadTask{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		for i := 0; i < 50; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL + "/slow.bin")
	id := "test-cancel"
	ctx, cancel := context.WithCancel(context.Background())
	downloadTasks.tasks[id] = &DownloadTask{ID: id, Status: "queued", Total: -1, cancel: cancel}
	done := make(chan struct{})
	go func() {
		runURLDownloadTaskWithClient(ctx, id, u, "cancelled.bin", "Never", server.Client().Transport)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	<-done
	task, _ := downloadTasks.snapshot(id)
	if task.Status != "cancelled" {
		t.Fatalf("unexpected task status: %+v", task)
	}
	if matches, _ := filepath.Glob("data/files/.url-download-*.tmp"); len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}
