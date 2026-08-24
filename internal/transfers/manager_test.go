package transfers

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
	"sync"
	"testing"
	"time"
)

func multipartUpload(t *testing.T, filename string, payload []byte, expiry string) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if expiry != "" {
		if err := writer.WriteField("expiry", expiry); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("file-upload", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return writer.FormDataContentType(), body.Bytes()
}

func TestSaveMultipartPublishesOnceAndReplaysIdempotentResult(t *testing.T) {
	root := t.TempDir()
	var commits []File
	var preferredIDs []string
	manager, err := NewManager(root, 1<<20, 1<<20, func(storage, expiry, preferredID string) error {
		commits = append(commits, File{Storage: storage, Expiry: expiry})
		preferredIDs = append(preferredIDs, preferredID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}
	contentType, body := multipartUpload(t, "image", png, "1 day")
	first, err := manager.SaveMultipart("same-operation", contentType, bytes.NewReader(body), "", "0195e6c7-1234-4123-8123-123456789abc")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.SaveMultipart("same-operation", "not multipart", strings.NewReader("ignored"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("idempotent results differ: %#v %#v", first, second)
	}
	if first[0].Filename != "image.png" || first[0].Expiry != "1 day" {
		t.Fatalf("unexpected published file: %#v", first[0])
	}
	if len(commits) != 1 || commits[0].Storage != "files/image.png" || commits[0].Expiry != "1 day" {
		t.Fatalf("metadata committed more than once or incorrectly: %#v", commits)
	}
	if len(preferredIDs) != 1 || preferredIDs[0] != "0195e6c7-1234-4123-8123-123456789abc" {
		t.Fatalf("preferred identity was not committed: %#v", preferredIDs)
	}
	data, err := os.ReadFile(filepath.Join(root, "files", "image.png"))
	if err != nil || !bytes.Equal(data, png) {
		t.Fatalf("published bytes differ: %v %v", data, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(root, "transfer-transactions", "*")); len(leftovers) != 0 {
		t.Fatalf("completed transfer left transaction files: %v", leftovers)
	}
	restarted, err := NewManager(root, 1<<20, 1<<20, func(storage, expiry, preferredID string) error {
		t.Fatal("durable idempotency replay committed metadata again")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, err := restarted.SaveMultipart("same-operation", "not multipart", strings.NewReader("ignored"), "", "")
	if err != nil || len(afterRestart) != 1 || afterRestart[0] != first[0] {
		t.Fatalf("idempotency result did not survive restart: %#v %v", afterRestart, err)
	}
}

func TestSaveMultipartRejectsLimitAndCleansUncommittedPayload(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root, 4, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	contentType, body := multipartUpload(t, "large.bin", []byte("12345"), "")
	if _, err = manager.SaveMultipart("", contentType, bytes.NewReader(body), "", ""); err == nil || !strings.Contains(err.Error(), "file exceeds 4 bytes") {
		t.Fatalf("unexpected size-limit result: %v", err)
	}
	if files, _ := os.ReadDir(filepath.Join(root, "files")); len(files) != 0 {
		t.Fatalf("oversized file became visible: %v", files)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(root, "transfer-transactions", "*")); len(leftovers) != 0 {
		t.Fatalf("uncommitted payload was not cleaned: %v", leftovers)
	}
}

func TestDownloadReportsPreparedAndProgressThenPublishes(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root, 1<<20, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "12")
		_, _ = io.WriteString(w, "task payload")
	}))
	defer server.Close()
	source, _ := url.Parse(server.URL + "/remote.bin")
	var updates []DownloadUpdate
	file, err := manager.Download(context.Background(), source, "saved.bin", "Never", server.Client().Transport, func(update DownloadUpdate) {
		updates = append(updates, update)
	})
	if err != nil {
		t.Fatal(err)
	}
	if file.Filename != "saved.bin" || file.Size != 12 {
		t.Fatalf("unexpected file: %#v", file)
	}
	if len(updates) < 2 || updates[0].Filename != "saved.bin" || updates[0].Received != 0 || updates[0].Total != 12 {
		t.Fatalf("missing prepared update: %#v", updates)
	}
	last := updates[len(updates)-1]
	if last.Received != 12 || last.Total != 12 {
		t.Fatalf("missing final progress: %#v", updates)
	}
}

func TestDownloadCancellationCleansPayload(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root, 1<<20, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer server.Close()
	source, _ := url.Parse(server.URL + "/slow.bin")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, downloadErr := manager.Download(ctx, source, "cancelled.bin", "Never", server.Client().Transport, nil)
		done <- downloadErr
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	if err = <-done; err == nil {
		t.Fatal("cancelled download unexpectedly succeeded")
	}
	if files, _ := os.ReadDir(filepath.Join(root, "files")); len(files) != 0 {
		t.Fatalf("cancelled file became visible: %v", files)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(root, "transfer-transactions", "*")); len(leftovers) != 0 {
		t.Fatalf("cancelled download left payloads: %v", leftovers)
	}
}

func TestRecoverCompletesJournalExactlyOnce(t *testing.T) {
	root := t.TempDir()
	var mutex sync.Mutex
	commits := 0
	manager, err := NewManager(root, 1<<20, 1<<20, func(storage, expiry, preferredID string) error {
		mutex.Lock()
		defer mutex.Unlock()
		commits++
		if storage != "files/recovered.bin" || expiry != "Never" {
			t.Fatalf("unexpected commit: %q %q", storage, expiry)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(root, "transfer-transactions", ".payload-recovery.tmp")
	final := filepath.Join(root, "files", "recovered.bin")
	if err = os.WriteFile(temp, []byte("recover me"), 0600); err != nil {
		t.Fatal(err)
	}
	tx := transaction{ID: "recovery", Temp: temp, Final: final, Storage: "files/recovered.bin", Filename: "recovered.bin", Expiry: "Never", Size: 10}
	payload, _ := json.Marshal(tx)
	journal := filepath.Join(root, "transfer-transactions", "recovery.json")
	if err = os.WriteFile(journal, payload, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Storage != tx.Storage {
		t.Fatalf("unexpected recovery result: %#v", recovered)
	}
	if data, readErr := os.ReadFile(final); readErr != nil || string(data) != "recover me" {
		t.Fatalf("recovered bytes missing: %q %v", data, readErr)
	}
	if _, statErr := os.Stat(journal); !os.IsNotExist(statErr) {
		t.Fatalf("journal remains: %v", statErr)
	}
	again, err := manager.Recover()
	if err != nil || len(again) != 0 || commits != 1 {
		t.Fatalf("recovery was not idempotent: %#v commits=%d err=%v", again, commits, err)
	}
}

func TestCommitFailureLeavesRecoverableInvisibleTransaction(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root, 1<<20, 1<<20, func(storage, expiry, preferredID string) error {
		return io.ErrUnexpectedEOF
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Publish(strings.NewReader("durable payload"), "pending.bin", "Never", ""); err == nil {
		t.Fatal("metadata failure unexpectedly succeeded")
	}
	if _, err = os.Stat(filepath.Join(root, "files", "pending.bin")); !os.IsNotExist(err) {
		t.Fatalf("failed transaction became visible: %v", err)
	}
	journals, _ := filepath.Glob(filepath.Join(root, "transfer-transactions", "*.json"))
	payloads, _ := filepath.Glob(filepath.Join(root, "transfer-transactions", ".payload-*.tmp"))
	if len(journals) != 1 || len(payloads) != 1 {
		t.Fatalf("recoverable transaction was not preserved: journals=%v payloads=%v", journals, payloads)
	}
	recovery, err := NewManager(root, 1<<20, 1<<20, func(storage, expiry, preferredID string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	files, err := recovery.Recover()
	if err != nil || len(files) != 1 {
		t.Fatalf("recovery failed: %#v %v", files, err)
	}
	if data, readErr := os.ReadFile(filepath.Join(root, "files", "pending.bin")); readErr != nil || string(data) != "durable payload" {
		t.Fatalf("recovered payload differs: %q %v", data, readErr)
	}
}

func TestPublicURLRejectsPrivateNetworks(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1/file", "http://192.168.1.2/file", "http://[::1]/file"} {
		if _, err := PublicURL(raw); err == nil {
			t.Fatalf("private URL accepted: %s", raw)
		}
	}
}

func TestDownloadFilenamePriority(t *testing.T) {
	source, _ := url.Parse("https://example.test/from-url.txt")
	response := &http.Response{Header: make(http.Header)}
	response.Header.Set("Content-Disposition", `attachment; filename="from-header.txt"`)
	if got := DownloadFilename(response, source, "requested.txt"); got != "requested.txt" {
		t.Fatalf("requested filename lost: %q", got)
	}
	if got := DownloadFilename(response, source, ""); got != "from-header.txt" {
		t.Fatalf("header filename lost: %q", got)
	}
}
