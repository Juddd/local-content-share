package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/* static/*
var content embed.FS

type Entry struct {
	ID         string    `json:"id"`
	StorageID  string    `json:"storageId,omitempty"`
	Revision   uint64    `json:"revision"`
	Content    string    `json:"content"`
	Type       string    `json:"type"`
	Filename   string    `json:"filename"`
	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Size       int64     `json:"size"`
	Favorite   bool      `json:"favorite,omitempty"`
}

var favorites = newFavoriteStore(filepath.Join("data", "favorites.json"))

type DownloadTask struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Filename string `json:"filename,omitempty"`
	Received int64  `json:"received"`
	Total    int64  `json:"total"`
	Error    string `json:"error,omitempty"`
	Item     *Entry `json:"item,omitempty"`
	cancel   context.CancelFunc
}

type downloadTaskStore struct {
	sync.Mutex
	tasks map[string]*DownloadTask
}

var downloadTasks = downloadTaskStore{tasks: map[string]*DownloadTask{}}

type idempotentUpload struct {
	Entries []Entry
	Created time.Time
}

var uploadResults = struct {
	sync.Mutex
	values map[string]idempotentUpload
}{values: map[string]idempotentUpload{}}

type uploadKeyLock struct {
	mutex sync.Mutex
	refs  int
}

var uploadKeyLocks = struct {
	sync.Mutex
	values map[string]*uploadKeyLock
}{values: map[string]*uploadKeyLock{}}

func lockUploadKey(key string) func() {
	uploadKeyLocks.Lock()
	entry := uploadKeyLocks.values[key]
	if entry == nil {
		entry = &uploadKeyLock{}
		uploadKeyLocks.values[key] = entry
	}
	entry.refs++
	uploadKeyLocks.Unlock()
	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		uploadKeyLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(uploadKeyLocks.values, key)
		}
		uploadKeyLocks.Unlock()
	}
}

func (s *downloadTaskStore) snapshot(id string) (DownloadTask, bool) {
	s.Lock()
	defer s.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return DownloadTask{}, false
	}
	copy := *t
	copy.cancel = nil
	return copy, true
}

func (s *downloadTaskStore) update(id string, update func(*DownloadTask)) {
	s.Lock()
	defer s.Unlock()
	if task := s.tasks[id]; task != nil {
		update(task)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func expectedRevision(r *http.Request) uint64 {
	v := r.FormValue("expectedRevision")
	if v == "" {
		v = r.Header.Get("If-Match")
	}
	v = strings.Trim(v, "\"")
	n, _ := strconv.ParseUint(v, 10, 64)
	return n
}

func writeRevisionConflict(w http.ResponseWriter, record IdentityRecord) {
	item, _ := entryByStorage(record.Storage)
	writeJSON(w, http.StatusConflict, map[string]any{"error": "revision_conflict", "item": item})
}

func entryByStorage(storage string) (*Entry, error) {
	if strings.HasPrefix(storage, "files/") || strings.HasPrefix(storage, "text/") {
		return contentEntry(storage)
	}
	if raw, ok := strings.CutPrefix(storage, "link/"); ok {
		title, target := raw, raw
		if parts := strings.SplitN(raw, "\t", 2); len(parts) == 2 {
			title, target = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		}
		times := itemTimeTracker.Get(storage, time.Now())
		return stableEntry(&Entry{ID: storage, Type: "link", Filename: title, Content: target, CreatedAt: times.CreatedAt, ModifiedAt: times.ModifiedAt}), nil
	}
	return nil, fmt.Errorf("unsupported content id")
}

func contentFilePath(id string, allowedTypes ...string) (string, error) {
	id = resolveStorageID(id)
	if id == "" || strings.Contains(id, "\\") || path.IsAbs(id) || path.Clean(id) != id {
		return "", fmt.Errorf("invalid content ID")
	}
	parts := strings.Split(id, "/")
	if len(parts) != 2 || parts[1] == "" || parts[1] == "." || parts[1] == ".." {
		return "", fmt.Errorf("invalid content ID")
	}
	allowed := false
	for _, contentType := range allowedTypes {
		if parts[0] == contentType {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("invalid content type")
	}
	return filepath.Join("data", parts[0], parts[1]), nil
}

type ItemTimes struct{ CreatedAt, ModifiedAt time.Time }
type ItemTimeTracker struct {
	Items map[string]ItemTimes `json:"items"`
	mu    sync.Mutex
}

var itemTimeTracker *ItemTimeTracker

const thumbnailDir = "data/thumbnails"
const maxFileSize int64 = 4 << 30
const maxURLDownloadSize int64 = 8 << 30
const maxTextContentSize int64 = 10 << 20

func publicDownloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid HTTP URL")
	}
	addrs, err := net.LookupIP(u.Hostname())
	if err != nil {
		return nil, err
	}
	for _, ip := range addrs {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return nil, fmt.Errorf("private network URL is not allowed")
		}
	}
	return u, nil
}

func publicOnlyTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range addresses {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				return nil, fmt.Errorf("private network URL is not allowed")
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("host has no IP address")
		}
		var lastErr error
		for _, ip := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
	return transport
}

func downloadFilename(resp *http.Response, u *url.URL, requested string) string {
	if n := strings.TrimSpace(requested); n != "" {
		return filepath.Base(n)
	}
	if _, p, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if n := filepath.Base(p["filename"]); n != "." && n != "" {
			return n
		}
	}
	if n := filepath.Base(u.Path); n != "." && n != "/" && n != "" {
		if x, e := url.PathUnescape(n); e == nil {
			return x
		}
		return n
	}
	return "download-" + time.Now().Format("20060102-150405")
}

func detectedFileExtension(filename string) string {
	file, err := os.Open(filename)
	if err != nil {
		return ""
	}
	defer file.Close()
	header := make([]byte, 512)
	n, _ := io.ReadFull(file, header)
	switch strings.SplitN(http.DetectContentType(header[:n]), ";", 2)[0] {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "application/pdf":
		return ".pdf"
	default:
		return ""
	}
}

func saveStreamedFile(r *http.Request, expiry, requestedName string) ([]Entry, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("missing multipart boundary")
	}
	mr := multipart.NewReader(r.Body, boundary)
	var expiryValue string
	saved := false
	var entries []Entry
	for {
		part, e := mr.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		field := part.FormName()
		if field == "expiry" {
			b, e := io.ReadAll(io.LimitReader(part, 1<<20))
			if e != nil {
				return nil, e
			}
			expiryValue = string(b)
			continue
		}
		if field == "name" {
			if _, e := io.Copy(io.Discard, io.LimitReader(part, 1<<20)); e != nil {
				return nil, e
			}
			continue
		}
		if field != "file-upload" {
			continue
		}
		name := requestedName
		if name == "" {
			name = part.FileName()
		}
		if name == "" {
			name = "upload.bin"
		}
		unique := generateUniqueFilename("data/files", name)
		tmp, e := os.CreateTemp("data/files", ".upload-*.tmp")
		if e != nil {
			return nil, e
		}
		tmpName := tmp.Name()
		ok := false
		defer func() {
			tmp.Close()
			if !ok {
				os.Remove(tmpName)
			}
		}()
		n, e := io.Copy(tmp, io.LimitReader(part, maxFileSize+1))
		if e != nil {
			return nil, e
		}
		if n > maxFileSize {
			return nil, fmt.Errorf("file exceeds 4 GB")
		}
		if e = tmp.Close(); e != nil {
			return nil, e
		}
		if filepath.Ext(unique) == "" {
			unique += detectedFileExtension(tmpName)
			unique = generateUniqueFilename("data/files", unique)
		}
		if e = os.Rename(tmpName, filepath.Join("data/files", unique)); e != nil {
			return nil, e
		}
		ok = true
		id := filepath.Join("files", unique)
		itemTimeTracker.Create(id)
		if expiryValue != "" && expiryValue != "Never" {
			expirationTracker.SetExpiration(id, expiryValue)
		}
		if entry, entryErr := fileEntry(id); entryErr == nil {
			entries = append(entries, *entry)
		}
		saved = true
	}
	if !saved {
		return nil, fmt.Errorf("file-upload field is required")
	}
	_ = expiry
	return entries, nil
}

func thumbnailPath(id string) string { return filepath.Join(thumbnailDir, id+".jpg") }

func streamUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key != "" {
		defer lockUploadKey(key)()
	}
	if key != "" {
		uploadResults.Lock()
		cached, ok := uploadResults.values[key]
		uploadResults.Unlock()
		if ok {
			writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "items": cached.Entries})
			return
		}
	}
	entries, err := saveStreamedFile(r, "", "")
	if err != nil {
		log.Printf("Stream upload failed from %s: %v", r.RemoteAddr, err)
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "exceeds 4 GB") {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	for i := range entries {
		entry := entries[i]
		notifyContentItem("created", &entry)
	}
	if key != "" {
		uploadResults.Lock()
		uploadResults.values[key] = idempotentUpload{Entries: entries, Created: time.Now()}
		for k, v := range uploadResults.values {
			if time.Since(v.Created) > 24*time.Hour {
				delete(uploadResults.values, k)
			}
		}
		uploadResults.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "created", "items": entries})
}

func ensureThumbnail(id string) (string, error) {
	src := filepath.Join("data", filepath.FromSlash(id))
	info, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if filepath.Ext(src) == "" {
		return "", fmt.Errorf("not an image")
	}
	if err := os.MkdirAll(filepath.Dir(thumbnailPath(id)), 0755); err != nil {
		return "", err
	}
	dst := thumbnailPath(id)
	if ti, err := os.Stat(dst); err == nil && ti.ModTime().After(info.ModTime()) {
		return dst, nil
	}
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	srcImg, _, err := image.Decode(f)
	if err != nil {
		return "", err
	}
	maxW, maxH := 320, 180
	b := srcImg.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := 1.0
	if w > maxW || h > maxH {
		sw := float64(maxW) / float64(w)
		sh := float64(maxH) / float64(h)
		if sw < sh {
			scale = sw
		} else {
			scale = sh
		}
	}
	dw, dh := int(float64(w)*scale), int(float64(h)*scale)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	outImg := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			sx := b.Min.X + x*w/dw
			sy := b.Min.Y + y*h/dh
			outImg.Set(x, y, srcImg.At(sx, sy))
		}
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	encErr := jpeg.Encode(out, outImg, &jpeg.Options{Quality: 82})
	closeErr := out.Close()
	if encErr != nil {
		return "", encErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func initItemTimeTracker() *ItemTimeTracker {
	t := &ItemTimeTracker{Items: map[string]ItemTimes{}}
	if data, err := os.ReadFile(filepath.Join("data", "item-times.json")); err == nil {
		_ = json.Unmarshal(data, t)
	}
	if t.Items == nil {
		t.Items = map[string]ItemTimes{}
	}
	return t
}
func (t *ItemTimeTracker) save() {
	data, _ := json.MarshalIndent(t, "", "  ")
	if err := atomicWriteFile(filepath.Join("data", "item-times.json"), data, 0644); err != nil {
		log.Printf("Error saving item times: %v", err)
	}
}
func (t *ItemTimeTracker) Get(id string, fallback time.Time) ItemTimes {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.Items[id]
	if !ok {
		v = ItemTimes{fallback, fallback}
		t.Items[id] = v
		t.save()
	}
	return v
}
func (t *ItemTimeTracker) Create(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.Items[id] = ItemTimes{now, now}
	t.save()
}
func (t *ItemTimeTracker) Touch(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.Items[id]
	if !ok {
		v.CreatedAt = time.Now()
	}
	v.ModifiedAt = time.Now()
	t.Items[id] = v
	t.save()
}
func (t *ItemTimeTracker) Rename(oldID, newID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v, ok := t.Items[oldID]; ok {
		delete(t.Items, oldID)
		v.ModifiedAt = time.Now()
		t.Items[newID] = v
		t.save()
	}
}
func (t *ItemTimeTracker) Delete(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.Items, id)
	t.save()
}

type ExpirationTracker struct {
	Expirations map[string]time.Time `json:"expirations"`
	mu          sync.Mutex           // mutex for thread safety
}

var expirationTracker *ExpirationTracker
var expirationOptions = []string{"Never", "1 hour", "4 hours", "1 day", "Custom"}

func initExpirationTracker() *ExpirationTracker {
	tracker := &ExpirationTracker{
		Expirations: make(map[string]time.Time),
	}
	// Load existing expirations from file
	expirationFile := filepath.Join("data", "expirations.json")
	if _, err := os.Stat(expirationFile); err == nil {
		data, err := os.ReadFile(expirationFile)
		if err == nil {
			var storedTracker ExpirationTracker
			if err := json.Unmarshal(data, &storedTracker); err == nil {
				tracker.Expirations = storedTracker.Expirations
			}
		}
	}
	return tracker
}

func parseCustomDuration(customExpiry string) time.Duration {
	customExpiry = strings.TrimSpace(customExpiry)
	// Regex to match the format like 1h, 30m, 2d, etc.
	re := regexp.MustCompile(`^(\d+)([hmMdwy])$`)
	matches := re.FindStringSubmatch(customExpiry)
	if len(matches) < 2 { // bad value
		return 5 * time.Minute
	}
	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 5 * time.Minute
	}
	unit := matches[2]
	switch unit {
	case "m": // minutes
		if value < 5 {
			return 5 * time.Minute
		}
		return time.Duration(value) * time.Minute
	case "h", "H": // hours
		return time.Duration(value) * time.Hour
	case "d", "D": // days
		return time.Duration(value) * 24 * time.Hour
	case "w", "W": // weeks
		return time.Duration(value) * 7 * 24 * time.Hour
	case "M": // months
		return time.Duration(value) * 30 * 24 * time.Hour
	case "y", "Y": // years
		return time.Duration(value) * 365 * 24 * time.Hour
	default:
		return 5 * time.Minute
	}
}

func (t *ExpirationTracker) SetExpiration(fileID, expiryOption string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if expiryOption == "Never" {
		delete(t.Expirations, fileID)
	} else {
		var duration time.Duration
		switch expiryOption {
		case "1 hour":
			duration = 1 * time.Hour
		case "4 hours":
			duration = 4 * time.Hour
		case "1 day":
			duration = 24 * time.Hour
		case "Custom":
			// Should not happen anymore.
			return
		default:
			if len(expiryOption) > 0 {
				duration = parseCustomDuration(expiryOption)
			} else {
				delete(t.Expirations, fileID)
				return
			}
		}
		t.Expirations[fileID] = time.Now().Add(duration)
	}
	t.saveToFile()
}

func (t *ExpirationTracker) saveToFile() {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		log.Printf("Error marshaling expirations: %v", err)
		return
	}
	expirationFile := filepath.Join("data", "expirations.json")
	if err := atomicWriteFile(expirationFile, data, 0644); err != nil {
		log.Printf("Error saving expirations: %v", err)
	}
}

func (t *ExpirationTracker) CleanupExpired() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	var expiredFiles []string
	// Find expired files
	for fileID, expiryTime := range t.Expirations {
		if now.After(expiryTime) {
			expiredFiles = append(expiredFiles, fileID)
		}
	}
	// Delete expired files
	for _, fileID := range expiredFiles {
		err := os.Remove(filepath.Join("data", fileID))
		if err != nil && !os.IsNotExist(err) {
			log.Printf("Error removing expired file %s: %v", fileID, err)
		} else {
			log.Printf("Removed expired file: %s", fileID)
		}
		delete(t.Expirations, fileID)
		if strings.HasPrefix(fileID, "files/") {
			_ = os.Remove(thumbnailPath(fileID))
		}
		itemTimeTracker.Delete(fileID)
		_ = favorites.Delete(fileID)
	}
	if len(expiredFiles) > 0 {
		t.saveToFile()
		for _, id := range expiredFiles {
			if record, err := identities.Delete(id, 0); err == nil {
				notifyContentDelete(record.ID)
			} else {
				notifyContentDelete(id)
			}
		}
	}
	return expiredFiles
}

var listenAddress = flag.String("listen", ":8080", "host:port in which the server will listen")

// Placeholder content for notepad files
const mdPlaceholder = `# Welcome to Markdown Notepad

Start typing your markdown here...

## Features

- **Bold** and *italic* text
- [Links](https://example.com)
- Lists (ordered and unordered)
- Code blocks
- And more!

` + "```" + `
function example() {
  console.log("Hello, Markdown!");
}
` + "```"

func generateUniqueFilename(baseDir, baseName string) string {
	baseName = strings.TrimSpace(baseName)
	// Sanitize: allow only letters (+unicode), numbers, space, dot, hyphen, underscore, () and []
	reg := regexp.MustCompile(`[^\p{L}\p{N}\p{M}\s\.\-_()\[\]]`)
	sanitizedName := reg.ReplaceAllString(baseName, "-")
	log.Printf("Sanitized name %s TO %s\n", baseName, sanitizedName)
	// First try without random prefix
	if _, err := os.Stat(filepath.Join(baseDir, sanitizedName)); os.IsNotExist(err) {
		return sanitizedName
	}
	// If file exists, add random prefix until we find a unique name
	for {
		randChars := fmt.Sprintf("%04d", rand.Intn(10000))
		newName := fmt.Sprintf("%s-%s", randChars, sanitizedName)
		if _, err := os.Stat(filepath.Join(baseDir, newName)); os.IsNotExist(err) {
			return newName
		}
	}
}

func fileEntry(id string) (*Entry, error) {
	id = resolveStorageID(id)
	info, err := os.Stat(filepath.Join("data", filepath.FromSlash(id)))
	if err != nil {
		return nil, err
	}
	times := itemTimeTracker.Get(id, info.ModTime())
	return stableEntry(&Entry{ID: id, Type: "file", Filename: filepath.Base(id), CreatedAt: times.CreatedAt, ModifiedAt: times.ModifiedAt, Size: info.Size()}), nil
}

func contentEntry(id string) (*Entry, error) {
	id = resolveStorageID(id)
	if strings.HasPrefix(id, "files/") {
		return fileEntry(id)
	}
	if strings.HasPrefix(id, "text/") {
		path := filepath.Join("data", filepath.FromSlash(id))
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		times := itemTimeTracker.Get(id, info.ModTime())
		return stableEntry(&Entry{ID: id, Type: "text", Filename: filepath.Base(id), Content: string(body), CreatedAt: times.CreatedAt, ModifiedAt: times.ModifiedAt, Size: info.Size(), Favorite: favorites.Is(id)}), nil
	}
	return nil, fmt.Errorf("unsupported content id")
}

func startURLDownloadTask(rawURL, requestedName, expiry string) (DownloadTask, error) {
	u, err := publicDownloadURL(strings.TrimSpace(rawURL))
	if err != nil {
		return DownloadTask{}, err
	}
	id := fmt.Sprintf("%d-%06d", time.Now().UnixNano(), rand.Intn(1000000))
	ctx, cancel := context.WithCancel(context.Background())
	task := &DownloadTask{ID: id, Status: "queued", Total: -1, cancel: cancel}
	downloadTasks.Lock()
	downloadTasks.tasks[id] = task
	downloadTasks.Unlock()
	go runURLDownloadTask(ctx, id, u, requestedName, expiry)
	result, _ := downloadTasks.snapshot(id)
	return result, nil
}

func failURLDownloadTask(id string, err error) {
	status := "failed"
	if errors.Is(err, context.Canceled) || downloadTaskCancelling(id) {
		status = "cancelled"
	}
	downloadTasks.update(id, func(task *DownloadTask) {
		task.Status = status
		if status == "failed" {
			task.Error = err.Error()
		}
		task.cancel = nil
	})
	scheduleDownloadTaskCleanup(id)
}

func downloadTaskCancelling(id string) bool {
	task, ok := downloadTasks.snapshot(id)
	return ok && (task.Status == "cancelling" || task.Status == "cancelled")
}

func scheduleDownloadTaskCleanup(id string) {
	time.AfterFunc(time.Hour, func() {
		downloadTasks.Lock()
		defer downloadTasks.Unlock()
		if task := downloadTasks.tasks[id]; task != nil && (task.Status == "completed" || task.Status == "failed" || task.Status == "cancelled") {
			delete(downloadTasks.tasks, id)
		}
	})
}

func runURLDownloadTask(ctx context.Context, id string, u *url.URL, requestedName, expiry string) {
	runURLDownloadTaskWithClient(ctx, id, u, requestedName, expiry, publicOnlyTransport())
}

func runURLDownloadTaskWithClient(ctx context.Context, id string, u *url.URL, requestedName, expiry string, transport http.RoundTripper) {
	client := &http.Client{Timeout: 30 * time.Minute, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		_, err := publicDownloadURL(req.URL.String())
		return err
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		failURLDownloadTask(id, err)
		return
	}
	downloadTasks.update(id, func(task *DownloadTask) { task.Status = "downloading" })
	resp, err := client.Do(req)
	if err != nil {
		failURLDownloadTask(id, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failURLDownloadTask(id, fmt.Errorf("remote server returned %s", resp.Status))
		return
	}
	if resp.ContentLength > maxURLDownloadSize {
		failURLDownloadTask(id, fmt.Errorf("remote file exceeds 8 GB"))
		return
	}
	name := downloadFilename(resp, u, requestedName)
	unique := generateUniqueFilename("data/files", name)
	downloadTasks.update(id, func(task *DownloadTask) {
		task.Filename = unique
		task.Total = resp.ContentLength
	})
	tmp, err := os.CreateTemp("data/files", ".url-download-*.tmp")
	if err != nil {
		failURLDownloadTask(id, err)
		return
	}
	tmpName := tmp.Name()
	completed := false
	defer func() {
		_ = tmp.Close()
		if !completed {
			_ = os.Remove(tmpName)
		}
	}()
	buf := make([]byte, 128*1024)
	var received int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			received += int64(n)
			if received > maxURLDownloadSize {
				failURLDownloadTask(id, fmt.Errorf("remote file exceeds 8 GB"))
				return
			}
			if _, err = tmp.Write(buf[:n]); err != nil {
				failURLDownloadTask(id, err)
				return
			}
			downloadTasks.update(id, func(task *DownloadTask) { task.Received = received })
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			failURLDownloadTask(id, readErr)
			return
		}
		if err = ctx.Err(); err != nil {
			failURLDownloadTask(id, err)
			return
		}
	}
	if err = tmp.Close(); err != nil {
		failURLDownloadTask(id, err)
		return
	}
	dst := filepath.Join("data/files", unique)
	if err = os.Rename(tmpName, dst); err != nil {
		failURLDownloadTask(id, err)
		return
	}
	completed = true
	fileID := filepath.Join("files", unique)
	itemTimeTracker.Create(fileID)
	if expiry != "" && expiry != "Never" {
		expirationTracker.SetExpiration(fileID, expiry)
	}
	entry, err := fileEntry(fileID)
	if err != nil {
		failURLDownloadTask(id, err)
		return
	}
	downloadTasks.update(id, func(task *DownloadTask) {
		task.Status = "completed"
		task.Received = received
		task.Item = entry
		task.cancel = nil
	})
	scheduleDownloadTaskCleanup(id)
	notifyContentItem("created", entry)
}

func main() {
	flag.Parse()

	if err := os.MkdirAll(filepath.Join("data", "files"), 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("data", "text"), 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("data", "notepad"), 0755); err != nil {
		log.Fatal(err)
	}
	log.Println("Data directory created/reused without errors.")
	createFileIfNotExists("notepad/md.file", mdPlaceholder)
	createFileIfNotExists("links.file", "")

	// Initialize the expiration tracker
	expirationTracker = initExpirationTracker()
	itemTimeTracker = initItemTimeTracker()
	identities = newIdentityStore(filepath.Join("data", "identities.json"))
	mutations = newMutationCache(filepath.Join("data", "mutation-results.json"))
	customExpiry := os.Getenv("DEFAULT_EXPIRY")
	if customExpiry != "" {
		switch customExpiry {
		case "1d":
			expirationOptions = []string{"1 day", "Never", "1 hour", "4 hours", "Custom"}
		case "4h":
			expirationOptions = []string{"4 hours", "Never", "1 hour", "1 day", "Custom"}
		case "1h":
			expirationOptions = []string{"1 hour", "Never", "4 hours", "1 day", "Custom"}
		default:
			expirationOptions = append([]string{customExpiry}, expirationOptions...)
		}
	}

	// Goroutine to periodically expire files
	go func() {
		ticker := time.NewTicker(3 * time.Minute) // 3 minutes is sparse enough, load is extremely minimal as the operation is fast (in memory tracker)
		defer ticker.Stop()
		for range ticker.C {
			expirationTracker.CleanupExpired()
		}
	}()

	funcMap := template.FuncMap{
		"previewText": func(s string, max int) string {
			runes := []rune(s)
			if len(runes) <= max {
				return s
			}
			return string(runes[:max])
		},
		"isTruncated": func(s string, max int) bool {
			return len([]rune(s)) > max
		},
	}
	tmpl := template.Must(template.New("").Funcs(funcMap).ParseFS(content, "templates/*.html"))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		clientMux.Lock()
		snapshotSequence := eventSequence
		clientMux.Unlock()
		w.Header().Set("X-Content-Sequence", strconv.FormatUint(snapshotSequence, 10))
		// Clean up expired files on page load
		expirationTracker.CleanupExpired()
		entries := []Entry{}
		// Read text snippets
		textFiles, _ := os.ReadDir(filepath.Join("data", "text"))
		for _, file := range textFiles {
			if file.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join("data", "text", file.Name()))
			if err != nil {
				continue
			}
			info, err := file.Info()
			if err != nil {
				continue
			}
			id := filepath.Join("text", file.Name())
			itemTimes := itemTimeTracker.Get(id, info.ModTime())
			entries = append(entries, Entry{
				ID:         id,
				Type:       "text",
				Content:    string(data),
				Filename:   file.Name(),
				CreatedAt:  itemTimes.CreatedAt,
				ModifiedAt: itemTimes.ModifiedAt,
				Favorite:   favorites.Is(id),
			})
		}
		// Read files
		files, _ := os.ReadDir(filepath.Join("data", "files"))
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			info, err := file.Info()
			if err != nil {
				continue
			}
			id := filepath.Join("files", file.Name())
			itemTimes := itemTimeTracker.Get(id, info.ModTime())
			entries = append(entries, Entry{
				ID:         id,
				Type:       "file",
				Filename:   file.Name(),
				CreatedAt:  itemTimes.CreatedAt,
				ModifiedAt: itemTimes.ModifiedAt,
				Size:       info.Size(),
			})
		}
		// Read links
		data, err := os.ReadFile(filepath.Join("data", "links.file"))
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			linksInfo, _ := os.Stat(filepath.Join("data", "links.file"))
			for lineIndex, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				storedLine := line
				linkTitle := line
				linkURL := line
				if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
					linkTitle = strings.TrimSpace(parts[0])
					linkURL = strings.TrimSpace(parts[1])
					if linkTitle == "" {
						linkTitle = linkURL
					}
				}
				fallback := time.Now().Add(time.Duration(lineIndex-len(lines)) * time.Second)
				if linksInfo != nil {
					fallback = linksInfo.ModTime().Add(time.Duration(lineIndex-len(lines)) * time.Second)
				}
				itemTimes := itemTimeTracker.Get("link/"+storedLine, fallback)
				entries = append(entries, Entry{
					ID:         "link/" + url.PathEscape(storedLine),
					Type:       "link",
					Content:    linkURL,
					Filename:   linkTitle,
					CreatedAt:  itemTimes.CreatedAt,
					ModifiedAt: itemTimes.ModifiedAt,
				})
			}
		}
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		})
		for i := range entries {
			stableEntry(&entries[i])
		}
		tmpl.ExecuteTemplate(w, "index.html", entries)
	})

	http.HandleFunc("/md", func(w http.ResponseWriter, r *http.Request) {
		tmpl.ExecuteTemplate(w, "md.html", nil)
	})

	// Native client API: return all content without requiring HTML parsing.
	http.HandleFunc("/api/v1/items", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		expirationTracker.CleanupExpired()
		entries := []Entry{}
		textFiles, _ := os.ReadDir(filepath.Join("data", "text"))
		for _, file := range textFiles {
			if file.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join("data", "text", file.Name()))
			if err != nil {
				continue
			}
			info, err := file.Info()
			if err != nil {
				continue
			}
			id := filepath.Join("text", file.Name())
			times := itemTimeTracker.Get(id, info.ModTime())
			entries = append(entries, Entry{ID: id, Type: "text", Content: string(data), Filename: file.Name(), CreatedAt: times.CreatedAt, ModifiedAt: times.ModifiedAt, Favorite: favorites.Is(id)})
		}
		files, _ := os.ReadDir(filepath.Join("data", "files"))
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			info, err := file.Info()
			if err != nil {
				continue
			}
			id := filepath.Join("files", file.Name())
			times := itemTimeTracker.Get(id, info.ModTime())
			entries = append(entries, Entry{ID: id, Type: "file", Filename: file.Name(), CreatedAt: times.CreatedAt, ModifiedAt: times.ModifiedAt, Size: info.Size()})
		}
		linksData, err := os.ReadFile(filepath.Join("data", "links.file"))
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(linksData)), "\n")
			linksInfo, _ := os.Stat(filepath.Join("data", "links.file"))
			for index, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				stored, title, linkURL := line, line, line
				if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
					title, linkURL = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
					if title == "" {
						title = linkURL
					}
				}
				fallback := time.Now().Add(time.Duration(index-len(lines)) * time.Second)
				if linksInfo != nil {
					fallback = linksInfo.ModTime().Add(time.Duration(index-len(lines)) * time.Second)
				}
				times := itemTimeTracker.Get("link/"+stored, fallback)
				entries = append(entries, Entry{ID: "link/" + url.PathEscape(stored), Type: "link", Content: linkURL, Filename: title, CreatedAt: times.CreatedAt, ModifiedAt: times.ModifiedAt})
			}
		}
		for i := range entries {
			stableEntry(&entries[i])
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt.After(entries[j].CreatedAt) })
		clientMux.Lock()
		snapshotSequence := eventSequence
		clientMux.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Sequence", strconv.FormatUint(snapshotSequence, 10))
		json.NewEncoder(w).Encode(entries)
	})

	// Retrieve custom expiration options
	http.HandleFunc("/getExpiryOptions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expirationOptions)
	})

	// Serve static files from embedded filesystem
	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		log.Fatalf("Failed to create static sub-filesystem: %v", err)
	}
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	http.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("style.css")
		if err != nil {
			http.Error(w, "Style not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "text/css")
		io.Copy(w, file)
	})

	http.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("manifest.json")
		if err != nil {
			http.Error(w, "Manifest not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/json")
		io.Copy(w, file)
	})

	http.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("sw.js")
		if err != nil {
			http.Error(w, "Service worker not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/javascript")
		io.Copy(w, file)
	})

	http.HandleFunc("/md.js", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("md.js")
		if err != nil {
			http.Error(w, "JavaScript not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/javascript")
		io.Copy(w, file)
	})

	// Handle favicon and icons
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("favicon.ico")
		if err != nil {
			http.Error(w, "Favicon not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "image/x-icon")
		io.Copy(w, file)
	})

	http.HandleFunc("/icon-192.png", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("icon-192.png")
		if err != nil {
			http.Error(w, "Icon not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "image/png")
		io.Copy(w, file)
	})

	http.HandleFunc("/icon-512.png", func(w http.ResponseWriter, r *http.Request) {
		file, err := staticFS.Open("icon-512.png")
		if err != nil {
			http.Error(w, "Icon not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "image/png")
		io.Copy(w, file)
	})

	// API endpoint to load notepad content
	http.HandleFunc("/notepad/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			filename := strings.TrimPrefix(r.URL.Path, "/notepad/")
			if filename != "md.file" { // && filename != "rtext.file" {
				http.Error(w, "Invalid notepad file", http.StatusBadRequest)
				return
			}
			content, err := os.ReadFile(filepath.Join("data", "notepad", filename))
			if err != nil {
				http.Error(w, "Error reading notepad file", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(content)
			return
		case "POST":
			filename := strings.TrimPrefix(r.URL.Path, "/notepad/")
			if filename != "md.file" { // && filename != "rtext.file" {
				http.Error(w, "Invalid notepad file", http.StatusBadRequest)
				return
			}
			content, err := io.ReadAll(io.LimitReader(r.Body, maxTextContentSize+1))
			if err != nil {
				http.Error(w, "Error reading request body", http.StatusInternalServerError)
				return
			}
			if int64(len(content)) > maxTextContentSize {
				http.Error(w, "Notepad content exceeds 10 MB", http.StatusRequestEntityTooLarge)
				return
			}
			err = os.WriteFile(filepath.Join("data", "notepad", filename), content, 0644)
			if err != nil {
				http.Error(w, "Error saving notepad file", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Saved"))
			log.Printf("Saved notepad content to %s\n", filename)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	http.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		contentType := r.Header.Get("Content-Type")
		var parseErr error
		if strings.HasPrefix(contentType, "multipart/form-data") {
			parseErr = r.ParseMultipartForm(100 << 20)
		} else if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
			parseErr = r.ParseForm()
		} else {
			parseErr = fmt.Errorf("unsupported Content-Type: %s", contentType)
		}
		if parseErr != nil {
			http.Error(w, parseErr.Error(), http.StatusBadRequest)
			return
		}
		entryType := r.FormValue("type")
		expiryOption := r.FormValue("expiry")
		content := r.FormValue("content")
		name := r.FormValue("name")
		clientID := strings.TrimSpace(r.FormValue("clientId"))
		createdName := ""
		var createdItem *Entry
		var createdItems []*Entry
		if entryType == "link" {
			// Handle link submission
			if content == "" {
				http.Error(w, "URL content cannot be empty", http.StatusBadRequest)
				return
			}
			u, err := url.ParseRequestURI(content)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "Invalid URL format. Must start with http:// or https://", http.StatusBadRequest)
				return
			}
			linksFilePath := filepath.Join("data", "links.file")
			f, err := os.OpenFile(linksFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer f.Close()
			name = strings.TrimSpace(strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(name))
			storedLink := content
			if name != "" {
				storedLink = name + "\t" + content
			}
			if _, err := f.WriteString(storedLink + "\n"); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			itemTimeTracker.Create("link/" + storedLink)
			identities.EnsureWithID("link/"+storedLink, clientID)
			now := time.Now()
			createdItem = stableEntry(&Entry{ID: "link/" + storedLink, Type: "link", Content: content, Filename: name, CreatedAt: now, ModifiedAt: now})
			if createdItem.Filename == "" {
				createdItem.Filename = content
			}
			log.Printf("Saved link %s\n", content)
		} else {
			// Handle file and text submission
			var files []*multipart.FileHeader
			if r.MultipartForm != nil {
				files = r.MultipartForm.File["file-upload"]
			}
			if len(files) > 0 {
				// File submission
				for _, fileHeader := range files {
					err := func() error {
						file, err := fileHeader.Open()
						if err != nil {
							return err
						}
						defer file.Close()
						fileName := name
						if fileName == "" {
							fileName = fileHeader.Filename
						}
						uniqueFileName := generateUniqueFilename("data/files", fileName)
						f, err := os.Create(filepath.Join("data/files", uniqueFileName))
						if err != nil {
							return err
						}
						defer f.Close()
						if _, err := io.Copy(f, file); err != nil {
							return err
						}
						itemTimeTracker.Create(filepath.Join("files", uniqueFileName))
						if entry, entryErr := fileEntry(filepath.Join("files", uniqueFileName)); entryErr == nil {
							createdItems = append(createdItems, entry)
						}
						if expiryOption != "Never" {
							fileID := filepath.Join("files", uniqueFileName)
							expirationTracker.SetExpiration(fileID, expiryOption)
						}
						log.Printf("Saved file %s with expiry %s\n", uniqueFileName, expiryOption)
						return nil
					}()
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
				}
			} else if content != "" {
				// Text snippet submission
				filename := name
				if filename == "" {
					// Use a full-width slash so the title reads as month/day while
					// remaining a safe single filename on disk.
					chinaStandardTime := time.FixedZone("China Standard Time", 8*60*60)
					filename = time.Now().In(chinaStandardTime).Format("01／02 15-04-05")
				}
				uniqueFileName := generateUniqueFilename("data/text", filename)
				err := os.WriteFile(filepath.Join("data/text", uniqueFileName), []byte(content), 0644)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				itemTimeTracker.Create(filepath.Join("text", uniqueFileName))
				identities.EnsureWithID(filepath.Join("text", uniqueFileName), clientID)
				createdName = uniqueFileName
				now := time.Now()
				createdItem = stableEntry(&Entry{ID: filepath.Join("text", uniqueFileName), Type: "text", Content: content, Filename: uniqueFileName, CreatedAt: now, ModifiedAt: now})
				if expiryOption != "Never" {
					fileID := filepath.Join("text", uniqueFileName)
					expirationTracker.SetExpiration(fileID, expiryOption)
				}
				log.Printf("Saved text snippet %s with expiry %s\n", uniqueFileName, expiryOption)
			}
		}
		if createdItem != nil {
			notifyContentItem("created", createdItem)
		}
		for _, item := range createdItems {
			notifyContentItem("created", item)
		}
		if createdItem == nil && len(createdItems) == 0 {
			notifyContentChange()
		}
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			writeJSON(w, http.StatusCreated, map[string]any{"title": createdName, "item": createdItem})
			return
		}
		// Send succes for AJAX
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Success"))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	http.HandleFunc("/api/v1/download-tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		task, err := startURLDownloadTask(r.FormValue("url"), r.FormValue("name"), r.FormValue("expiry"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusAccepted, task)
	})

	http.HandleFunc("/api/v1/download-tasks/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/download-tasks/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			task, ok := downloadTasks.snapshot(id)
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusOK, task)
		case http.MethodDelete:
			downloadTasks.Lock()
			task := downloadTasks.tasks[id]
			if task == nil {
				downloadTasks.Unlock()
				http.NotFound(w, r)
				return
			}
			if task.cancel != nil && task.Status != "completed" && task.Status != "failed" && task.Status != "cancelled" {
				task.Status = "cancelling"
				task.cancel()
			}
			copy := *task
			copy.cancel = nil
			downloadTasks.Unlock()
			writeJSON(w, http.StatusOK, copy)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	http.HandleFunc("/download-url", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		var parseErr error
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			parseErr = r.ParseMultipartForm(1 << 20)
		} else {
			parseErr = r.ParseForm()
		}
		if parseErr != nil {
			http.Error(w, parseErr.Error(), 400)
			return
		}
		u, err := publicDownloadURL(strings.TrimSpace(r.FormValue("url")))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		client := &http.Client{Timeout: 30 * time.Minute, Transport: publicOnlyTransport(), CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			_, err := publicDownloadURL(req.URL.String())
			return err
		}}
		resp, err := client.Get(u.String())
		if err != nil {
			http.Error(w, "Download failed: "+err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			http.Error(w, "Remote server returned "+resp.Status, 502)
			return
		}
		if resp.ContentLength > maxURLDownloadSize {
			http.Error(w, "Remote file exceeds 8 GB", 413)
			return
		}
		name := downloadFilename(resp, u, r.FormValue("name"))
		unique := generateUniqueFilename("data/files", name)
		tmp, err := os.CreateTemp("data/files", ".url-download-*.tmp")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		tmpName := tmp.Name()
		ok := false
		defer func() {
			tmp.Close()
			if !ok {
				os.Remove(tmpName)
			}
		}()
		n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxURLDownloadSize+1))
		if err != nil || n > maxURLDownloadSize {
			http.Error(w, "Download failed or exceeds 8 GB", 502)
			return
		}
		if err = tmp.Close(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		dst := filepath.Join("data/files", unique)
		if err = os.Rename(tmpName, dst); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ok = true
		id := filepath.Join("files", unique)
		itemTimeTracker.Create(id)
		expiry := r.FormValue("expiry")
		if expiry != "" && expiry != "Never" {
			expirationTracker.SetExpiration(id, expiry)
		}
		if entry, entryErr := fileEntry(id); entryErr == nil {
			notifyContentItem("created", entry)
		} else {
			notifyContentChange()
		}
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.Write([]byte("Success"))
			return
		}
		http.Redirect(w, r, "/", 303)
	})

	http.HandleFunc("/upload-stream", streamUploadHandler)

	http.HandleFunc("/rename/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requestedID := strings.TrimPrefix(r.URL.Path, "/rename/")
		oldPath := resolveStorageID(requestedID)
		if record, ok := identities.Resolve(requestedID); ok && expectedRevision(r) > 0 && record.Revision != expectedRevision(r) {
			writeRevisionConflict(w, record)
			return
		}
		newName := r.FormValue("newname")
		if newName == "" {
			http.Error(w, "New name cannot be empty", http.StatusBadRequest)
			return
		}
		if storedLink, ok := strings.CutPrefix(oldPath, "link/"); ok {
			newName = strings.TrimSpace(strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(newName))
			if newName == "" {
				http.Error(w, "New name cannot be empty", http.StatusBadRequest)
				return
			}
			linkURL := storedLink
			if parts := strings.SplitN(storedLink, "\t", 2); len(parts) == 2 {
				linkURL = strings.TrimSpace(parts[1])
			}
			linksFilePath := filepath.Join("data", "links.file")
			data, err := os.ReadFile(linksFilePath)
			if err != nil {
				http.Error(w, "Failed to read links file", http.StatusInternalServerError)
				return
			}
			lines := strings.Split(string(data), "\n")
			found := false
			for i, line := range lines {
				if !found && strings.TrimSpace(line) == strings.TrimSpace(storedLink) {
					lines[i] = newName + "\t" + linkURL
					found = true
				}
			}
			if !found {
				http.Error(w, "Link not found", http.StatusNotFound)
				return
			}
			if err := os.WriteFile(linksFilePath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
				http.Error(w, "Failed to save link title", http.StatusInternalServerError)
				return
			}
			itemTimeTracker.Rename("link/"+storedLink, "link/"+newName+"\t"+linkURL)
			newID := "link/" + newName + "\t" + linkURL
			record, mutationErr := identities.Mutate(requestedID, newID, expectedRevision(r))
			if mutationErr != nil {
				if errors.Is(mutationErr, errRevisionConflict) {
					writeRevisionConflict(w, record)
					return
				}
				http.Error(w, mutationErr.Error(), 500)
				return
			}
			times := itemTimeTracker.Get(newID, time.Now())
			notifyContentRename(record.ID, stableEntry(&Entry{ID: newID, Type: "link", Filename: newName, Content: linkURL, CreatedAt: times.CreatedAt, ModifiedAt: times.ModifiedAt}))
			if strings.Contains(r.Header.Get("Accept"), "application/json") {
				item, _ := entryByStorage(newID)
				writeJSON(w, 200, map[string]any{"status": "renamed", "item": item})
				return
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			log.Printf("Renamed link title to %s\n", newName)
			return
		}
		oldFullPath, err := contentFilePath(oldPath, "files", "text")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		baseDir := filepath.Dir(oldFullPath)
		newName = generateUniqueFilename(baseDir, newName)

		// Get the new full path
		newPath := filepath.Join(baseDir, newName)
		// Rename the file
		err = os.Rename(oldFullPath, newPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if strings.HasPrefix(oldPath, "files/") {
			_ = os.Remove(thumbnailPath(oldPath))
			_ = os.Remove(thumbnailPath(strings.TrimPrefix(newPath, "data/")))
		}
		newID := strings.TrimPrefix(newPath, "data/")
		newID = strings.ReplaceAll(newID, "\\", "/")
		expirationTracker.mu.Lock()
		if expiryTime, hasExpiry := expirationTracker.Expirations[oldPath]; hasExpiry {
			delete(expirationTracker.Expirations, oldPath)
			expirationTracker.Expirations[newID] = expiryTime
			expirationTracker.saveToFile()
		}
		expirationTracker.mu.Unlock()
		itemTimeTracker.Rename(oldPath, newID)
		_ = favorites.Rename(oldPath, newID)
		record, mutationErr := identities.Mutate(requestedID, newID, expectedRevision(r))
		if mutationErr != nil {
			if errors.Is(mutationErr, errRevisionConflict) {
				writeRevisionConflict(w, record)
				return
			}
			http.Error(w, mutationErr.Error(), 500)
			return
		}
		if entry, entryErr := contentEntry(newID); entryErr == nil {
			notifyContentRename(record.ID, entry)
			if strings.Contains(r.Header.Get("Accept"), "application/json") {
				writeJSON(w, 200, map[string]any{"status": "renamed", "item": entry})
				return
			}
		} else {
			notifyContentChange()
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Renamed %s to %s\n", oldPath, newName)
	})

	http.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/raw/")
		filePath, pathErr := contentFilePath(id, "text")
		if pathErr != nil {
			http.Error(w, "Only text files can be accessed", http.StatusBadRequest)
			return
		}
		content, err := os.ReadFile(filePath)
		if err != nil {
			http.Error(w, "File not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(content)
	})

	http.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/download/")
		filePath, pathErr := contentFilePath(filename, "files", "text")
		if pathErr != nil {
			http.Error(w, "Invalid file", http.StatusBadRequest)
			return
		}
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		file, err := os.Open(filePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()

		// Brute force method to determine content type
		ext := strings.ToLower(filepath.Ext(filename))
		var contentType string
		switch ext {
		case ".pdf":
			contentType = "application/pdf"
		case ".jpg", ".jpeg":
			contentType = "image/jpeg"
		case ".png":
			contentType = "image/png"
		case ".gif":
			contentType = "image/gif"
		case ".svg":
			contentType = "image/svg+xml"
		default:
			buffer := make([]byte, 512)
			_, err = file.Read(buffer)
			if err != nil && err != io.EOF {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			contentType = http.DetectContentType(buffer)
			_, err = file.Seek(0, 0)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		baseFilename := filepath.Base(filename)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": baseFilename}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, baseFilename, fileInfo.ModTime(), file)
		log.Printf("Served %s for download\n", filename)
	})

	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/view/")
		filePath, err := contentFilePath(filename, "files")
		if err != nil {
			http.Error(w, "Invalid file", http.StatusBadRequest)
			return
		}
		http.ServeFile(w, r, filePath)
		log.Printf("Served %s for viewing\n", filename)
	})

	http.HandleFunc("/thumbnail/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/thumbnail/")
		if _, err := contentFilePath(id, "files"); err != nil {
			http.Error(w, "Only images can be thumbnailed", http.StatusBadRequest)
			return
		}
		id = resolveStorageID(id)
		path, err := ensureThumbnail(id)
		if err != nil {
			http.Error(w, "Thumbnail unavailable", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, path)
	})

	http.HandleFunc("/delete/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requestedID := strings.TrimPrefix(r.URL.Path, "/delete/")
		id := resolveStorageID(requestedID)
		if record, ok := identities.Resolve(requestedID); ok && expectedRevision(r) > 0 && record.Revision != expectedRevision(r) {
			writeRevisionConflict(w, record)
			return
		}
		// Handle link deletion
		if after, ok := strings.CutPrefix(id, "link/"); ok {
			linkToDelete := after
			linksFilePath := filepath.Join("data", "links.file")
			data, err := os.ReadFile(linksFilePath)
			if err != nil {
				http.Error(w, "Failed to read links file for deletion", http.StatusInternalServerError)
				return
			}
			lines := strings.Split(string(data), "\n")
			var newLines []string
			var found bool
			for _, line := range lines {
				if strings.TrimSpace(line) == strings.TrimSpace(linkToDelete) && !found {
					found = true // Remove only the first occurrence
					continue
				}
				if strings.TrimSpace(line) != "" {
					newLines = append(newLines, line)
				}
			}
			output := strings.Join(newLines, "\n")
			// Add newline for correctness
			if output != "" {
				output += "\n"
			}
			err = os.WriteFile(linksFilePath, []byte(output), 0644)
			if err != nil {
				http.Error(w, "Failed to write links file after deletion", http.StatusInternalServerError)
				return
			}
			itemTimeTracker.Delete("link/" + linkToDelete)
			record, _ := identities.Delete(requestedID, expectedRevision(r))
			notifyContentDelete(record.ID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status": "ok"}`))
			log.Printf("Deleted link %s\n", linkToDelete)
			return
		}
		// Handle file and snippet deletion
		filePath, pathErr := contentFilePath(id, "files", "text")
		if pathErr != nil {
			http.Error(w, "Invalid item", http.StatusBadRequest)
			return
		}
		err := os.Remove(filePath)
		if err != nil {
			log.Printf("Failed to delete %s: %v", id, err)
			http.Error(w, "Failed to delete file", http.StatusInternalServerError)
			return
		}
		if strings.HasPrefix(id, "files/") {
			_ = os.Remove(thumbnailPath(id))
		}
		itemTimeTracker.Delete(id)
		_ = favorites.Delete(id)
		expirationTracker.mu.Lock()
		delete(expirationTracker.Expirations, id)
		expirationTracker.saveToFile()
		expirationTracker.mu.Unlock()
		record, _ := identities.Delete(requestedID, expectedRevision(r))
		notifyContentDelete(record.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
		log.Printf("Deleted %s\n", id)
	})

	http.HandleFunc("/favorite/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requestedID := strings.TrimPrefix(r.URL.Path, "/favorite/")
		id := resolveStorageID(requestedID)
		if record, ok := identities.Resolve(requestedID); ok && expectedRevision(r) > 0 && record.Revision != expectedRevision(r) {
			writeRevisionConflict(w, record)
			return
		}
		if _, err := contentFilePath(id, "text"); err != nil {
			http.Error(w, "Can only favorite text snippets", http.StatusBadRequest)
			return
		}
		if _, err := os.Stat(filepath.Join("data", filepath.FromSlash(id))); err != nil {
			http.Error(w, "Snippet not found", http.StatusNotFound)
			return
		}
		raw := r.FormValue("favorite")
		if raw != "true" && raw != "false" && raw != "1" && raw != "0" {
			http.Error(w, "favorite must be true or false", http.StatusBadRequest)
			return
		}
		value := raw == "true" || raw == "1"
		if err := favorites.Set(id, value); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		record, mutationErr := identities.Mutate(requestedID, "", expectedRevision(r))
		if mutationErr != nil {
			if errors.Is(mutationErr, errRevisionConflict) {
				writeRevisionConflict(w, record)
				return
			}
			http.Error(w, mutationErr.Error(), 500)
			return
		}
		entry, err := contentEntry(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		notifyContentItem("updated", entry)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "item": entry})
	})

	http.HandleFunc("/edit/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requestedID := strings.TrimPrefix(r.URL.Path, "/edit/")
		id := resolveStorageID(requestedID)
		if record, ok := identities.Resolve(requestedID); ok && expectedRevision(r) > 0 && record.Revision != expectedRevision(r) {
			writeRevisionConflict(w, record)
			return
		}
		filePath, pathErr := contentFilePath(id, "text")
		if pathErr != nil {
			http.Error(w, "Can only edit text snippets", http.StatusBadRequest)
			return
		}
		content := r.FormValue("content")
		if content == "" {
			http.Error(w, "Content cannot be empty", http.StatusBadRequest)
			return
		}
		info, statErr := os.Stat(filePath)
		err := os.WriteFile(filePath, []byte(content), 0644)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if statErr == nil {
			itemTimeTracker.Get(id, info.ModTime())
		}
		itemTimeTracker.Touch(id)
		record, mutationErr := identities.Mutate(requestedID, "", expectedRevision(r))
		if mutationErr != nil {
			if errors.Is(mutationErr, errRevisionConflict) {
				writeRevisionConflict(w, record)
				return
			}
			http.Error(w, mutationErr.Error(), 500)
			return
		}
		if entry, entryErr := contentEntry(id); entryErr == nil {
			notifyContentItem("updated", entry)
			if strings.Contains(r.Header.Get("Accept"), "application/json") {
				writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "item": entry})
				return
			}
		} else {
			notifyContentChange()
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Edited %s\n", id)
	})

	// SSE Updates for content refresh
	http.HandleFunc("/api/updates", handleContentUpdates)

	// Start server
	server := &http.Server{Addr: *listenAddress, Handler: idempotencyMiddleware(http.DefaultServeMux), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	log.Fatal(server.ListenAndServe())
}

// Helper function to create files if they don't exist
func createFileIfNotExists(filename string, defaultContent string) {
	dir := filepath.Dir(filepath.Join("data", filename))
	if dir != "." && dir != "data" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("Error creating directory %s: %v\n", dir, err)
		}
	}
	filePath := filepath.Join("data", filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		err := os.WriteFile(filePath, []byte(defaultContent), 0644)
		if err != nil {
			log.Printf("Error creating file %s: %v\n", filename, err)
		} else {
			log.Printf("Created file %s with default content\n", filename)
		}
	}
}
