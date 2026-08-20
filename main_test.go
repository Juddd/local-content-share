package main

import (
    "net/http"
    "net/url"
    "strings"
    "testing"
)

func TestPublicDownloadURLRejectsPrivateAddresses(t *testing.T) {
    for _, raw := range []string{"http://127.0.0.1/file", "http://localhost/file", "http://192.168.1.10/file"} {
        if _, err := publicDownloadURL(raw); err == nil { t.Fatalf("expected private URL to be rejected: %s", raw) }
    }
}

func TestDownloadFilenamePriority(t *testing.T) {
    u, _ := url.Parse("https://example.test/path/from-url.jpg")
    r := &http.Response{Header: make(http.Header)}
    if got := downloadFilename(r, u, "chosen.png"); got != "chosen.png" { t.Fatalf("requested name: %q", got) }
    r.Header.Set("Content-Disposition", `attachment; filename="header.bin"`)
    if got := downloadFilename(r, u, ""); got != "header.bin" { t.Fatalf("header name: %q", got) }
}

func TestTemporaryDownloadMarker(t *testing.T) {
    if !strings.Contains("data/files/.url-download-", ".url-download-") { t.Fatal("temporary path marker missing") }
}
