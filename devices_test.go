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

func TestClassifyDeviceIP(t *testing.T) {
	t.Setenv("LCS_TRUSTED_PROXY_CIDRS", "192.168.32.0/20")
	cases := map[string]string{
		"192.168.3.20":         "lan",
		"10.20.30.40":          "lan",
		"127.0.0.1":            "lan",
		"192.168.32.1":         "unknown",
		"::1":                  "lan",
		"8.8.8.8":              "wan",
		"2001:4860:4860::8888": "wan",
		"invalid":              "",
	}
	for value, want := range cases {
		if got := classifyDeviceIP(value); got != want {
			t.Errorf("classifyDeviceIP(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestRequestIPOnlyTrustsForwardingHeadersFromConfiguredProxy(t *testing.T) {
	t.Setenv("LCS_TRUSTED_PROXY_CIDRS", "192.168.32.0/20")

	request := httptest.NewRequest(http.MethodPost, "/api/v1/device/heartbeat", nil)
	request.RemoteAddr = "192.168.32.1:4567"
	request.Header.Set("X-Forwarded-For", "203.0.113.17")
	if got := requestIP(request); got != "203.0.113.17" {
		t.Fatalf("requestIP() = %q, want trusted forwarded client address", got)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/device/heartbeat", nil)
	request.RemoteAddr = "192.168.32.1:4567"
	request.Header.Set("X-Forwarded-For", "192.168.3.20")
	if got := requestIP(request); got != "192.168.3.20" {
		t.Fatalf("requestIP() = %q, want trusted forwarded LAN address", got)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/device/heartbeat", nil)
	request.RemoteAddr = "8.8.8.8:4567"
	request.Header.Set("X-Forwarded-For", "192.168.3.20")
	if got := requestIP(request); got != "8.8.8.8" {
		t.Fatalf("requestIP() trusted an untrusted forwarding header: %q", got)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/device/heartbeat", nil)
	request.RemoteAddr = "192.168.32.1:4567"
	if got := requestIP(request); got != "" {
		t.Fatalf("requestIP() = %q, want empty when trusted proxy omits client address", got)
	}
}

func TestDeviceRetentionCleanup(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "devices.json")
	store := newDeviceStore(path)
	store.now = func() time.Time { return now }

	oldID, _ := randomDeviceID()
	recentID, _ := randomDeviceID()
	activeID, _ := randomDeviceID()
	store.devices[oldID] = &BrowserDevice{ID: oldID, CreatedAt: now.Add(-40 * 24 * time.Hour), LastSeen: now.Add(-31 * 24 * time.Hour)}
	store.devices[recentID] = &BrowserDevice{ID: recentID, CreatedAt: now.Add(-40 * 24 * time.Hour), LastActivity: now.Add(-29 * 24 * time.Hour)}
	store.devices[activeID] = &BrowserDevice{ID: activeID, CreatedAt: now.Add(-40 * 24 * time.Hour), LastSeen: now.Add(-31 * 24 * time.Hour)}
	store.sessions[activeID+"\x00session-0000000000000001"] = &browserSession{ID: "session-0000000000000001", DeviceID: activeID, LastSeen: now}
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}

	views := store.list()
	if len(views) != 2 || store.devices[oldID] != nil || store.devices[recentID] == nil || store.devices[activeID] == nil {
		t.Fatalf("unexpected cleanup result: views=%#v devices=%#v", views, store.devices)
	}
	reloaded := newDeviceStore(path)
	if reloaded.devices[oldID] != nil || reloaded.devices[recentID] == nil || reloaded.devices[activeID] == nil {
		t.Fatalf("cleanup was not persisted: %#v", reloaded.devices)
	}
}
