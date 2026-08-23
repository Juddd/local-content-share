warning: /bin/sh: setlocale: LC_ALL: cannot change locale (C.UTF-8)
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func deviceTestCookie(t *testing.T, response *http.Response) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == deviceCookieName {
			return cookie
		}
	}
	t.Fatal("device cookie was not set")
	return nil
}

func postDeviceJSON(t *testing.T, client *http.Client, url string, cookie *http.Cookie, value any) *http.Response {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestDeviceCookieSharedSessionsAndPersistentLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	store := newDeviceStore(path)
	fixedNow := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return fixedNow }
	mux := http.NewServeMux()
	registerDeviceHandlers(mux, store)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("private page")) })
	server := httptest.NewServer(store.gateMiddleware(store.identityMiddleware(mux)))
	defer server.Close()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/", nil)
	request.Header.Set("Accept", "text/html")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	cookie := deviceTestCookie(t, response)
	_ = response.Body.Close()
	if !deviceIDPattern.MatchString(cookie.Value) || !cookie.HttpOnly || cookie.Path != "/" {
		t.Fatalf("unexpected device cookie: %#v", cookie)
	}

	for _, sessionID := range []string{"session-0000000000000001", "session-0000000000000002"} {
		response = postDeviceJSON(t, server.Client(), server.URL+"/api/v1/device/heartbeat", cookie, heartbeatRequest{SessionID: sessionID, Platform: "Windows", Browser: "Chrome", Visible: true, Active: true})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("heartbeat returned %d", response.StatusCode)
		}
		_ = response.Body.Close()
	}
	devices := store.list()
	if len(devices) != 1 || devices[0].Tabs != 2 || devices[0].State != "online" {
		t.Fatalf("unexpected device view: %#v", devices)
	}

	response = postDeviceJSON(t, server.Client(), server.URL+"/api/v1/devices/"+cookie.Value+"/rename", nil, map[string]string{"name": "书房电脑"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("rename returned %d", response.StatusCode)
	}
	_ = response.Body.Close()
	response = postDeviceJSON(t, server.Client(), server.URL+"/api/v1/devices/"+cookie.Value+"/lock", nil, map[string]any{})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("lock returned %d", response.StatusCode)
	}
	_ = response.Body.Close()

	lockedRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/", nil)
	lockedRequest.Header.Set("Accept", "text/html")
	lockedRequest.AddCookie(cookie)
	response, err = server.Client().Do(lockedRequest)
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	body := string(bodyBytes)
	if response.StatusCode != http.StatusNotFound || !strings.Contains(body, "404") || strings.Contains(body, "private page") {
		t.Fatalf("locked page was not gated: status=%d body=%q", response.StatusCode, body)
	}

	statusRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/device/status", nil)
	statusRequest.AddCookie(cookie)
	response, err = server.Client().Do(statusRequest)
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]bool
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !status["locked"] {
		t.Fatalf("locked status endpoint failed: %#v", status)
	}

	reloaded := newDeviceStore(path)
	if !reloaded.isLocked(cookie.Value) || reloaded.list()[0].DisplayName != "书房电脑" {
		t.Fatalf("lock or device name did not survive reload: %#v", reloaded.list())
	}
}

func TestDeviceUnlockRestoresPage(t *testing.T) {
	store := newDeviceStore(filepath.Join(t.TempDir(), "devices.json"))
	id, err := randomDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.heartbeat(id, heartbeatRequest{SessionID: "session-0000000000000001", Platform: "macOS", Browser: "Safari", Visible: false}, "192.168.3.31"); err != nil {
		t.Fatal(err)
	}
	if err := store.setLocked(id, true); err != nil {
		t.Fatal(err)
	}
	if err := store.setLocked(id, false); err != nil {
		t.Fatal(err)
	}
	if store.isLocked(id) {
		t.Fatal("device remained locked")
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"locked": true`) {
		t.Fatalf("unlocked state was not persisted: %s", data)
	}
}

func TestDeviceInputValidation(t *testing.T) {
	if _, err := cleanDeviceLabel(strings.Repeat("a", 81), 80); err == nil {
		t.Fatal("overlong name was accepted")
	}
	if _, err := cleanDeviceLabel("bad\nname", 80); err == nil {
		t.Fatal("control character was accepted")
	}
	if devicePathAllowedWhileLocked("/api/v1/devices") {
		t.Fatal("device administration API must not bypass a locked browser cookie")
	}
}
