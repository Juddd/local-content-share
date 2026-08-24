package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	deviceCookieName          = "lcs_device"
	deviceCookieAge           = 10 * 365 * 24 * 60 * 60
	deviceOnlineTTL           = 40 * time.Second
	deviceRetention           = 30 * 24 * time.Hour
	deviceSeenPersistInterval = 5 * time.Minute
)

var (
	deviceIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
)

type BrowserDevice struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Platform      string    `json:"platform,omitempty"`
	Browser       string    `json:"browser,omitempty"`
	Locked        bool      `json:"locked"`
	LockedAt      time.Time `json:"lockedAt,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	LastSeen      time.Time `json:"lastSeen,omitempty"`
	LastActivity  time.Time `json:"lastActivity,omitempty"`
	LastIP        string    `json:"ip,omitempty"`
	NetworkHint   string    `json:"networkHint,omitempty"`
	persistedSeen time.Time
}

type browserSession struct {
	ID           string
	DeviceID     string
	Visible      bool
	LastSeen     time.Time
	LastActivity time.Time
}

type deviceCommand struct {
	Type string `json:"type"`
}

type deviceView struct {
	ID           string    `json:"id"`
	Name         string    `json:"name,omitempty"`
	DisplayName  string    `json:"displayName"`
	Platform     string    `json:"platform,omitempty"`
	Browser      string    `json:"browser,omitempty"`
	Locked       bool      `json:"locked"`
	LockedAt     time.Time `json:"lockedAt,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	LastSeen     time.Time `json:"lastSeen,omitempty"`
	LastActivity time.Time `json:"lastActivity,omitempty"`
	IP           string    `json:"ip,omitempty"`
	Network      string    `json:"network,omitempty"`
	State        string    `json:"state"`
	Tabs         int       `json:"tabs"`
}

type deviceStore struct {
	mu        sync.RWMutex
	path      string
	devices   map[string]*BrowserDevice
	sessions  map[string]*browserSession
	listeners map[string]map[chan deviceCommand]struct{}
	now       func() time.Time
}

func newDeviceStore(path string) *deviceStore {
	s := &deviceStore{
		path:      path,
		devices:   map[string]*BrowserDevice{},
		sessions:  map[string]*browserSession{},
		listeners: map[string]map[chan deviceCommand]struct{}{},
		now:       time.Now,
	}
	if data, err := os.ReadFile(path); err == nil {
		var stored struct {
			Devices []*BrowserDevice `json:"devices"`
		}
		if json.Unmarshal(data, &stored) == nil {
			for _, device := range stored.Devices {
				if device != nil && deviceIDPattern.MatchString(device.ID) {
					copy := *device
					copy.persistedSeen = copy.LastSeen
					s.devices[copy.ID] = &copy
				}
			}
		}
	}
	return s
}

func randomDeviceID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *deviceStore) deviceIDFromRequest(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(deviceCookieName)
	if err != nil || !deviceIDPattern.MatchString(cookie.Value) {
		return "", false
	}
	return cookie.Value, true
}

func (s *deviceStore) ensureDeviceID(w http.ResponseWriter, r *http.Request) (string, error) {
	if id, ok := s.deviceIDFromRequest(r); ok {
		return id, nil
	}
	id, err := randomDeviceID()
	if err != nil {
		return "", err
	}
	secure := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   deviceCookieAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return id, nil
}

func (s *deviceStore) identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptsHTML := strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && acceptsHTML {
			if _, err := s.ensureDeviceID(w, r); err != nil {
				http.Error(w, "Unable to create browser session", http.StatusInternalServerError)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *deviceStore) gateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.deviceIDFromRequest(r)
		if !ok || !s.isLocked(id) || devicePathAllowedWhileLocked(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		writeLockedNotFound(w)
	})
}

func devicePathAllowedWhileLocked(path string) bool {
	return path == "/api/v1/device/status" || path == "/api/v1/device/heartbeat" || path == "/api/v1/device/events" || path == "/api/v1/device/network-info" || path == "/api/v1/device/network-ping"
}

func writeLockedNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprint(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex,nofollow"><title>404 Not Found</title><style>html,body{height:100%;margin:0}body{display:grid;place-items:center;background:#f7f7f7;color:#333;font:16px system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}.box{text-align:center}.code{font-size:64px;font-weight:700;letter-spacing:.04em}.line{width:56px;height:3px;background:#aaa;margin:18px auto}.hint{color:#777}</style></head><body><main class="box"><div class="code">404</div><div class="line"></div><div class="hint">Page not found</div></main><script>(()=>{const check=()=>fetch('/api/v1/device/status',{cache:'no-store',credentials:'same-origin'}).then(r=>r.ok?r.json():null).then(v=>{if(v&&!v.locked)location.replace('/?r='+Date.now())}).catch(()=>{});check();setInterval(check,4000)})()</script></body></html>`)
}

func (s *deviceStore) isLocked(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	device := s.devices[id]
	return device != nil && device.Locked
}

func (s *deviceStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	devices := make([]*BrowserDevice, 0, len(s.devices))
	for _, device := range s.devices {
		copy := *device
		devices = append(devices, &copy)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].CreatedAt.Before(devices[j].CreatedAt) })
	data, err := json.MarshalIndent(struct {
		Devices []*BrowserDevice `json:"devices"`
	}{Devices: devices}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, append(data, '\n'), 0644)
}

func cleanDeviceLabel(value string, maxRunes int) (string, error) {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > maxRunes {
		return "", errors.New("value is too long")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("value contains control characters")
		}
	}
	return value, nil
}

func requestIP(r *http.Request) string {
	remote := parseRequestIP(r.RemoteAddr)
	// Forwarding headers are client-controlled unless the immediate peer is a
	// proxy explicitly listed in LCS_TRUSTED_PROXY_CIDRS. Without this check a
	// browser can forge an X-Forwarded-For value and change its network label.
	if remote != nil && trustedProxyIP(remote) {
		for _, value := range []string{strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0], r.Header.Get("X-Real-IP")} {
			if ip := net.ParseIP(strings.TrimSpace(value)); ip != nil {
				return ip.String()
			}
		}
		// A trusted proxy that does not forward the client address gives us no
		// reliable network classification. Do not report the proxy's private IP.
		return ""
	}
	if remote != nil {
		return remote.String()
	}
	return ""
}

func parseRequestIP(address string) net.IP {
	if host, _, err := net.SplitHostPort(strings.TrimSpace(address)); err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(strings.TrimSpace(address))
}

func trustedProxyIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// A request cannot arrive from loopback over the network. On this NAS the
	// front proxy forwards requests locally, so loopback is always a trusted
	// hop for reading its sanitized client-IP headers.
	if ip.IsLoopback() {
		return true
	}
	value := strings.TrimSpace(os.Getenv("LCS_TRUSTED_PROXY_CIDRS"))
	if value == "" {
		return false
	}
	for _, raw := range strings.Split(value, ",") {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func classifyDeviceIP(value string) string {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return ""
	}
	// A persisted address can be a known reverse-proxy/Docker gateway from an
	// older deployment. It is infrastructure, not evidence that the browser is
	// on the same LAN. Treat it as unknown until the next heartbeat records the
	// real source address.
	if trustedProxyIP(ip) {
		return "unknown"
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return "lan"
	}
	return "wan"
}

func (s *deviceStore) heartbeat(id string, input heartbeatRequest, ip string) (bool, error) {
	now := s.now()
	platform, err := cleanDeviceLabel(input.Platform, 40)
	if err != nil {
		return false, err
	}
	browser, err := cleanDeviceLabel(input.Browser, 40)
	if err != nil {
		return false, err
	}
	networkHint := input.Network
	if networkHint != "lan" && networkHint != "wan" {
		networkHint = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := id + "\x00" + input.SessionID
	if input.Gone {
		delete(s.sessions, key)
		device := s.devices[id]
		return device != nil && device.Locked, nil
	}
	device := s.devices[id]
	persist := false
	if device == nil {
		device = &BrowserDevice{ID: id, CreatedAt: now, LastActivity: now}
		s.devices[id] = device
		persist = true
	}
	if device.Platform != platform || device.Browser != browser || device.LastIP != ip || device.NetworkHint != networkHint {
		device.Platform, device.Browser = platform, browser
		// An empty address is intentional when a trusted proxy omitted the
		// client address. Clear an old gateway value instead of displaying it
		// forever as if it belonged to the browser.
		device.LastIP = ip
		device.NetworkHint = networkHint
		persist = true
	}
	if device.persistedSeen.IsZero() || now.Sub(device.persistedSeen) >= deviceSeenPersistInterval {
		persist = true
	}
	device.LastSeen = now
	session := s.sessions[key]
	if session == nil {
		session = &browserSession{ID: input.SessionID, DeviceID: id, LastActivity: now}
		s.sessions[key] = session
	}
	session.Visible = input.Visible
	session.LastSeen = now
	if input.Active {
		// Persist actual user activity at a bounded rate. Heartbeats remain
		// memory-only, while the last useful activity still survives restarts.
		if device.LastActivity.IsZero() || now.Sub(device.LastActivity) >= 30*time.Second {
			persist = true
		}
		session.LastActivity = now
		device.LastActivity = now
	} else if device.LastActivity.IsZero() {
		device.LastActivity = session.LastActivity
	}
	locked := device.Locked
	if persist {
		if err := s.saveLocked(); err != nil {
			return locked, err
		}
		device.persistedSeen = now
	}
	return locked, nil
}

func defaultDeviceName(device *BrowserDevice) string {
	parts := make([]string, 0, 2)
	if device.Platform != "" {
		parts = append(parts, device.Platform)
	}
	if device.Browser != "" {
		parts = append(parts, device.Browser)
	}
	if len(parts) == 0 {
		return "浏览器设备"
	}
	return strings.Join(parts, " · ")
}

func (s *deviceStore) list() []deviceView {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, session := range s.sessions {
		if now.Sub(session.LastSeen) > 10*deviceOnlineTTL {
			delete(s.sessions, key)
		}
	}
	removed := false
	for id, device := range s.devices {
		activeSession := false
		for _, session := range s.sessions {
			if session.DeviceID == id {
				activeSession = true
				break
			}
		}
		latest := device.CreatedAt
		if device.LastSeen.After(latest) {
			latest = device.LastSeen
		}
		if device.LastActivity.After(latest) {
			latest = device.LastActivity
		}
		if !activeSession && !latest.IsZero() && now.Sub(latest) > deviceRetention {
			delete(s.devices, id)
			delete(s.listeners, id)
			removed = true
		}
	}
	if removed {
		_ = s.saveLocked()
	}
	result := make([]deviceView, 0, len(s.devices))
	for _, device := range s.devices {
		state, tabs := "offline", 0
		lastSeen, lastActivity := device.LastSeen, device.LastActivity
		visible := false
		for _, session := range s.sessions {
			if session.DeviceID != device.ID || now.Sub(session.LastSeen) > deviceOnlineTTL {
				continue
			}
			tabs++
			visible = visible || session.Visible
			if session.LastSeen.After(lastSeen) {
				lastSeen = session.LastSeen
			}
			if session.LastActivity.After(lastActivity) {
				lastActivity = session.LastActivity
			}
		}
		if tabs > 0 {
			if visible {
				state = "online"
			} else {
				state = "background"
			}
		}
		if device.Locked {
			state = "locked"
		}
		network := classifyDeviceIP(device.LastIP)
		if network == "" || network == "unknown" {
			network = device.NetworkHint
		}
		result = append(result, deviceView{ID: device.ID, Name: device.Name, DisplayName: firstNonEmpty(device.Name, defaultDeviceName(device)), Platform: device.Platform, Browser: device.Browser, Locked: device.Locked, LockedAt: device.LockedAt, CreatedAt: device.CreatedAt, LastSeen: lastSeen, LastActivity: lastActivity, IP: device.LastIP, Network: network, State: state, Tabs: tabs})
	}
	sort.SliceStable(result, func(i, j int) bool {
		rank := func(state string) int {
			switch state {
			case "online":
				return 0
			case "background":
				return 1
			case "locked":
				return 2
			default:
				return 3
			}
		}
		if rank(result[i].State) != rank(result[j].State) {
			return rank(result[i].State) < rank(result[j].State)
		}
		return result[i].LastSeen.After(result[j].LastSeen)
	})
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (s *deviceStore) rename(id, name string) error {
	name, err := cleanDeviceLabel(name, 80)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	device := s.devices[id]
	if device == nil {
		return os.ErrNotExist
	}
	device.Name = name
	return s.saveLocked()
}

func (s *deviceStore) setLocked(id string, locked bool) error {
	s.mu.Lock()
	device := s.devices[id]
	if device == nil {
		s.mu.Unlock()
		return os.ErrNotExist
	}
	if device.Locked != locked {
		device.Locked = locked
		if locked {
			device.LockedAt = s.now()
		} else {
			device.LockedAt = time.Time{}
		}
		if err := s.saveLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	listeners := make([]chan deviceCommand, 0, len(s.listeners[id]))
	for listener := range s.listeners[id] {
		listeners = append(listeners, listener)
	}
	s.mu.Unlock()
	command := deviceCommand{Type: map[bool]string{true: "lock", false: "unlock"}[locked]}
	for _, listener := range listeners {
		select {
		case listener <- command:
		default:
			select {
			case <-listener:
			default:
			}
			select {
			case listener <- command:
			default:
			}
		}
	}
	return nil
}

func (s *deviceStore) addListener(id string) (chan deviceCommand, func()) {
	channel := make(chan deviceCommand, 4)
	s.mu.Lock()
	if s.listeners[id] == nil {
		s.listeners[id] = map[chan deviceCommand]struct{}{}
	}
	s.listeners[id][channel] = struct{}{}
	locked := s.devices[id] != nil && s.devices[id].Locked
	s.mu.Unlock()
	channel <- deviceCommand{Type: map[bool]string{true: "lock", false: "connected"}[locked]}
	return channel, func() {
		s.mu.Lock()
		delete(s.listeners[id], channel)
		if len(s.listeners[id]) == 0 {
			delete(s.listeners, id)
		}
		s.mu.Unlock()
	}
}

type heartbeatRequest struct {
	SessionID string `json:"sessionId"`
	Platform  string `json:"platform"`
	Browser   string `json:"browser"`
	Network   string `json:"network"`
	Visible   bool   `json:"visible"`
	Active    bool   `json:"active"`
	Gone      bool   `json:"gone"`
}

func localDeviceProbeAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	addresses := make([]string, 0, 2)
	for _, iface := range interfaces {
		values, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, value := range values {
			var ip net.IP
			switch address := value.(type) {
			case *net.IPNet:
				ip = address.IP
			case *net.IPAddr:
				ip = address.IP
			}
			if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsPrivate() {
				continue
			}
			text := ip.To4().String()
			if _, ok := seen[text]; ok {
				continue
			}
			seen[text] = struct{}{}
			addresses = append(addresses, text)
		}
	}
	sort.Strings(addresses)
	return addresses
}

func decodeSmallJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func registerDeviceHandlers(mux *http.ServeMux, store *deviceStore) {
	mux.HandleFunc("/api/v1/device/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, err := store.ensureDeviceID(w, r)
		if err != nil {
			http.Error(w, "Unable to create device", http.StatusInternalServerError)
			return
		}
		var input heartbeatRequest
		if !decodeSmallJSON(w, r, &input) {
			return
		}
		if !sessionIDPattern.MatchString(input.SessionID) {
			http.Error(w, "Invalid sessionId", http.StatusBadRequest)
			return
		}
		locked, err := store.heartbeat(id, input, requestIP(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"deviceId": id, "locked": locked})
	})

	mux.HandleFunc("/api/v1/device/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, err := store.ensureDeviceID(w, r)
		if err != nil {
			http.Error(w, "Unable to create device", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"locked": store.isLocked(id)})
	})

	mux.HandleFunc("/api/v1/device/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, err := store.ensureDeviceID(w, r)
		if err != nil {
			http.Error(w, "Unable to create device", http.StatusInternalServerError)
			return
		}
		if !sessionIDPattern.MatchString(r.URL.Query().Get("sessionId")) {
			http.Error(w, "Invalid sessionId", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		channel, remove := store.addListener(id)
		defer remove()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case command := <-channel:
				payload, _ := json.Marshal(command)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()
			case <-ticker.C:
				_, _ = fmt.Fprint(w, ": keep-alive\n\n")
				flusher.Flush()
			}
		}
	})

	// Browsers cannot inspect the DNS address selected for the current page.
	// They can, however, probe the NAS LAN addresses directly. This gives the
	// device center the same split-DNS result as the Android client when a
	// reverse proxy hides the source address.
	mux.HandleFunc("/api/v1/device/network-info", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"addresses": localDeviceProbeAddresses()})
	})
	mux.HandleFunc("/api/v1/device/network-ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/v1/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"devices": store.list()})
	})

	mux.HandleFunc("/api/v1/devices/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/devices/"), "/"), "/")
		if len(parts) != 2 || !deviceIDPattern.MatchString(parts[0]) {
			http.NotFound(w, r)
			return
		}
		id, action := parts[0], parts[1]
		var err error
		switch action {
		case "rename":
			var input struct {
				Name string `json:"name"`
			}
			if !decodeSmallJSON(w, r, &input) {
				return
			}
			err = store.rename(id, input.Name)
		case "lock":
			err = store.setLocked(id, true)
		case "unlock":
			err = store.setLocked(id, false)
		default:
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "Device not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"status": action})
	})
}
