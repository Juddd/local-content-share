package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestStreamUploadLimitIsFourGiB(t *testing.T) {
	if maxFileSize != 4294967296 {
		t.Fatalf("unexpected limit: %d", maxFileSize)
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
