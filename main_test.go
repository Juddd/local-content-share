package main

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
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

func TestIndexTemplateRendersDocument(t *testing.T) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"previewText": func(s string, max int) string { return s },
		"isTruncated": func(s string, max int) bool { return false },
	}).ParseFS(content, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = tmpl.ExecuteTemplate(&output, "index.html", []Entry{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "<!DOCTYPE html>") {
		t.Fatalf("index template rendered %d bytes without a document", output.Len())
	}
}

func TestIndexUsesLocalStructuredCardInsertion(t *testing.T) {
	raw, err := content.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, label := range []string{">文字<", ">文件<", ">链接<", ">新增<", ">记事本<"} {
		if !strings.Contains(source, label) {
			t.Fatalf("missing Chinese label %s", label)
		}
	}
	start := strings.Index(source, "async function insertRenderedCard(item)")
	if start < 0 {
		t.Fatal("card insertion function not found")
	}
	end := strings.Index(source[start:], "async function submitNewCard")
	if end < 0 {
		t.Fatal("card submit function not found")
	}
	insert := source[start : start+end]
	if strings.Contains(insert, "fetch(") {
		t.Fatal("structured card insertion must not fetch the full page")
	}
	if !strings.Contains(insert, "createSnippetCard(item)") {
		t.Fatal("structured snippet card builder is not used")
	}
}

func TestFileViewUsesFaviconPreviewPage(t *testing.T) {
	raw, err := content.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	if strings.Contains(source, `data-file-view href="/view/`) {
		t.Fatal("server-rendered file view still bypasses the preview page")
	}
	if !strings.Contains(source, `data-file-view href="/preview/{{.ID}}"`) ||
		!strings.Contains(source, "`/preview/${item.id}`") {
		t.Fatal("both server-rendered and dynamically inserted file cards must use the preview route")
	}

	preview, err := content.ReadFile("templates/image-preview.html")
	if err != nil {
		t.Fatal(err)
	}
	previewSource := string(preview)
	if !strings.Contains(previewSource, `/icon-192.png?v=app-logo-2`) {
		t.Fatal("image preview does not declare the current app logo favicon")
	}
	if !strings.Contains(previewSource, `src="{{.ImageURL}}"`) {
		t.Fatal("image preview does not load the original view URL")
	}
}

func TestServeFilePreviewWrapsImagesAndRedirectsOtherFiles(t *testing.T) {
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
	if err = os.WriteFile("data/files/picture.png", []byte("not decoded by the wrapper"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile("data/files/notes.txt", []byte("plain text"), 0644); err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("").ParseFS(content, "templates/image-preview.html")
	if err != nil {
		t.Fatal(err)
	}

	imageResponse := httptest.NewRecorder()
	serveFilePreview(tmpl, imageResponse, httptest.NewRequest(http.MethodGet, "/preview/files/picture.png", nil))
	if imageResponse.Code != http.StatusOK {
		t.Fatalf("image preview status %d: %s", imageResponse.Code, imageResponse.Body.String())
	}
	if !strings.Contains(imageResponse.Body.String(), `/icon-192.png?v=app-logo-2`) ||
		!strings.Contains(imageResponse.Body.String(), `src="/view/files/picture.png"`) {
		t.Fatalf("unexpected image preview: %s", imageResponse.Body.String())
	}

	textResponse := httptest.NewRecorder()
	serveFilePreview(tmpl, textResponse, httptest.NewRequest(http.MethodGet, "/preview/files/notes.txt", nil))
	if textResponse.Code != http.StatusFound || textResponse.Header().Get("Location") != "/view/files/notes.txt" {
		t.Fatalf("non-image preview should retain native view behavior, got %d %q", textResponse.Code, textResponse.Header().Get("Location"))
	}
}

func TestPublicDownloadURLRejectsPrivateAddresses(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1/file", "http://localhost/file", "http://192.168.1.10/file"} {
		if _, err := publicDownloadURL(raw); err == nil {
			t.Fatalf("expected private URL to be rejected: %s", raw)
		}
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
	if err = initContentLifecycle("data"); err != nil {
		t.Fatal(err)
	}
	expirationTracker = initExpirationTracker()
	if err = initFileTransfers(); err != nil {
		t.Fatal(err)
	}

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
	if err = initContentLifecycle("data"); err != nil {
		t.Fatal(err)
	}
	expirationTracker = initExpirationTracker()
	if err = initFileTransfers(); err != nil {
		t.Fatal(err)
	}

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
	if err = initContentLifecycle("data"); err != nil {
		t.Fatal(err)
	}
	expirationTracker = initExpirationTracker()
	if err = initFileTransfers(); err != nil {
		t.Fatal(err)
	}
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
	if err := initContentLifecycle("data"); err != nil {
		t.Fatal(err)
	}
	expirationTracker = initExpirationTracker()
	if err := initFileTransfers(); err != nil {
		t.Fatal(err)
	}
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
