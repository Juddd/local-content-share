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

type imagePreviewData struct {
	Title    string
	ImageURL string
}

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
		return stableEntry(&Entry{ID: storage, Type: "link", Filename: title, Content: target, CreatedAt: time.Now(), ModifiedAt: time.Now()}), nil
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

func escapedContentURL(prefix, id string) string {
	return (&url.URL{Path: prefix + id}).String()
}

func isPreviewableImage(filePath string) bool {
	if strings.HasPrefix(mime.TypeByExtension(strings.ToLower(filepath.Ext(filePath))), "image/") {
		return true
	}
	file, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, 512)
	count, err := file.Read(header)
	if err != nil && err != io.EOF {
		return false
	}
	return strings.HasPrefix(http.DetectContentType(header[:count]), "image/")
}

func serveFilePreview(tmpl *template.Template, w http.ResponseWriter, r *http.Request) {
	requestedID := strings.TrimPrefix(r.URL.Path, "/preview/")
	filePath, err := contentFilePath(requestedID, "files")
	if err != nil {
		http.Error(w, "Invalid file", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	if !isPreviewableImage(filePath) {
		http.Redirect(w, r, escapedContentURL("/view/", requestedID), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-cache")
	if err := tmpl.ExecuteTemplate(w, "image-preview.html", imagePreviewData{
		Title:    filepath.Base(filePath),
		ImageURL: escapedContentURL("/view/", requestedID),
	}); err != nil {
		log.Printf("Render image preview failed: %v\n", err)
	}
}

const thumbnailDir = "data/thumbnails"
const maxFileSize int64 = 4 << 30
const maxURLDownloadSize int64 = 8 << 30
const maxTextContentSize int64 = 10 << 20

func thumbnailPath(id string) string { return filepath.Join(thumbnailDir, id+".jpg") }

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
	var candidates []string
	// Find expired files
	for fileID, expiryTime := range t.Expirations {
		if now.After(expiryTime) {
			candidates = append(candidates, fileID)
		}
	}
	var expiredFiles []string
	// Delete expired payload and metadata as one retriable lifecycle.
	for _, fileID := range candidates {
		err := os.Remove(filepath.Join("data", fileID))
		if err != nil && !os.IsNotExist(err) {
			log.Printf("Error removing expired file %s: %v", fileID, err)
			continue
		}
		record, found := contentLifecycle.Resolve(fileID)
		if found {
			var mutationErr error
			record, mutationErr = contentLifecycle.RemoveStorage(fileID)
			if mutationErr != nil {
				log.Printf("Error removing expired metadata %s: %v", fileID, mutationErr)
				continue
			}
		} else {
			record = IdentityRecord{ID: fileID, Storage: fileID}
		}
		log.Printf("Removed expired file: %s", fileID)
		delete(t.Expirations, fileID)
		if strings.HasPrefix(fileID, "files/") {
			_ = os.Remove(thumbnailPath(fileID))
		}
		notifyContentDelete(record.ID)
		expiredFiles = append(expiredFiles, fileID)
	}
	if len(expiredFiles) > 0 {
		t.saveToFile()
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

func fileEntry(id string) (*Entry, error) {
	id = resolveStorageID(id)
	info, err := os.Stat(filepath.Join("data", filepath.FromSlash(id)))
	if err != nil {
		return nil, err
	}
	return stableEntry(&Entry{ID: id, Type: "file", Filename: filepath.Base(id), CreatedAt: info.ModTime(), ModifiedAt: info.ModTime(), Size: info.Size()}), nil
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
		return stableEntry(&Entry{ID: id, Type: "text", Filename: filepath.Base(id), Content: string(body), CreatedAt: info.ModTime(), ModifiedAt: info.ModTime(), Size: info.Size()}), nil
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
	browserDevices := newDeviceStore(filepath.Join("data", "devices.json"))
	log.Println("Data directory created/reused without errors.")
	createFileIfNotExists("notepad/md.file", mdPlaceholder)
	createFileIfNotExists("links.file", "")

	// Initialize the expiration tracker
	expirationTracker = initExpirationTracker()
	if err := initContentLifecycle("data"); err != nil {
		log.Fatal(err)
	}
	if err := initFileTransfers(); err != nil {
		log.Fatal(err)
	}
	if links, err := os.ReadFile(filepath.Join("data", "links.file")); err == nil {
		for _, stored := range strings.Split(strings.TrimSpace(string(links)), "\n") {
			stored = strings.TrimSpace(stored)
			if stored != "" {
				_ = contentLifecycle.MigrateLegacyStorage("link/"+url.PathEscape(stored), "link/"+stored)
			}
		}
	}
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
	registerDeviceHandlers(http.DefaultServeMux, browserDevices)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		snapshotSequence := contentEvents.Sequence()
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
			entries = append(entries, Entry{
				ID:         id,
				Type:       "text",
				Content:    string(data),
				Filename:   file.Name(),
				CreatedAt:  info.ModTime(),
				ModifiedAt: info.ModTime(),
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
			entries = append(entries, Entry{
				ID:         id,
				Type:       "file",
				Filename:   file.Name(),
				CreatedAt:  info.ModTime(),
				ModifiedAt: info.ModTime(),
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
				entries = append(entries, Entry{
					ID:         "link/" + storedLine,
					Type:       "link",
					Content:    linkURL,
					Filename:   linkTitle,
					CreatedAt:  fallback,
					ModifiedAt: fallback,
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
			entries = append(entries, Entry{ID: id, Type: "text", Content: string(data), Filename: file.Name(), CreatedAt: info.ModTime(), ModifiedAt: info.ModTime()})
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
			entries = append(entries, Entry{ID: id, Type: "file", Filename: file.Name(), CreatedAt: info.ModTime(), ModifiedAt: info.ModTime(), Size: info.Size()})
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
				entries = append(entries, Entry{ID: "link/" + stored, Type: "link", Content: linkURL, Filename: title, CreatedAt: fallback, ModifiedAt: fallback})
			}
		}
		for i := range entries {
			stableEntry(&entries[i])
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt.After(entries[j].CreatedAt) })
		snapshotSequence := contentEvents.Sequence()
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
			if filename != "md.file" {
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
			if filename != "md.file" {
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
			previousLinks, err := os.ReadFile(linksFilePath)
			if err != nil && !os.IsNotExist(err) {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			name = strings.TrimSpace(strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(name))
			storedLink := content
			if name != "" {
				storedLink = name + "\t" + content
			}
			nextLinks := append(append([]byte(nil), previousLinks...), []byte(storedLink+"\n")...)
			if err := atomicWriteFile(linksFilePath, nextLinks, 0644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if _, err := contentLifecycle.Add("link/"+storedLink, clientID); err != nil {
				_ = atomicWriteFile(linksFilePath, previousLinks, 0644)
				http.Error(w, err.Error(), 500)
				return
			}
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
						published, err := fileTransfers.Publish(file, fileName, expiryOption, clientID)
						if err != nil {
							return err
						}
						if entry, entryErr := fileEntry(published.Storage); entryErr == nil {
							createdItems = append(createdItems, entry)
						}
						log.Printf("Saved file %s with expiry %s\n", published.Filename, expiryOption)
						return nil
					}()
					if err != nil {
						status := http.StatusInternalServerError
						if strings.Contains(err.Error(), "exceeds 4 GB") {
							status = http.StatusRequestEntityTooLarge
						}
						http.Error(w, err.Error(), status)
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
				err := atomicWriteFile(filepath.Join("data/text", uniqueFileName), []byte(content), 0644)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if _, err := contentLifecycle.Add(filepath.Join("text", uniqueFileName), clientID); err != nil {
					_ = os.Remove(filepath.Join("data/text", uniqueFileName))
					http.Error(w, err.Error(), 500)
					return
				}
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
		expiry := r.FormValue("expiry")
		published, err := fileTransfers.Download(r.Context(), u, r.FormValue("name"), expiry, publicOnlyTransport(), nil)
		if err != nil {
			status := http.StatusBadGateway
			if strings.Contains(err.Error(), "exceeds 8 GB") {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "Download failed: "+err.Error(), status)
			return
		}
		if entry, entryErr := fileEntry(published.Storage); entryErr == nil {
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
		if record, ok := contentLifecycle.Resolve(requestedID); ok && expectedRevision(r) > 0 && record.Revision != expectedRevision(r) {
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
			if err := atomicWriteFile(linksFilePath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
				http.Error(w, "Failed to save link title", http.StatusInternalServerError)
				return
			}
			newID := "link/" + newName + "\t" + linkURL
			record, mutationErr := contentLifecycle.Rename(requestedID, newID, expectedRevision(r))
			if mutationErr != nil {
				_ = atomicWriteFile(linksFilePath, data, 0644)
				if errors.Is(mutationErr, errRevisionConflict) {
					writeRevisionConflict(w, record)
					return
				}
				http.Error(w, mutationErr.Error(), 500)
				return
			}
			notifyContentRename(record.ID, stableEntry(&Entry{ID: newID, Type: "link", Filename: newName, Content: linkURL, CreatedAt: time.Now(), ModifiedAt: time.Now()}))
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
		newID := strings.TrimPrefix(newPath, "data/")
		newID = strings.ReplaceAll(newID, "\\", "/")
		record, mutationErr := contentLifecycle.Rename(requestedID, newID, expectedRevision(r))
		if mutationErr != nil {
			_ = os.Rename(newPath, oldFullPath)
			if errors.Is(mutationErr, errRevisionConflict) {
				writeRevisionConflict(w, record)
				return
			}
			http.Error(w, mutationErr.Error(), 500)
			return
		}
		if strings.HasPrefix(oldPath, "files/") {
			_ = os.Remove(thumbnailPath(oldPath))
			_ = os.Remove(thumbnailPath(strings.TrimPrefix(newPath, "data/")))
		}
		expirationTracker.mu.Lock()
		if expiryTime, hasExpiry := expirationTracker.Expirations[oldPath]; hasExpiry {
			delete(expirationTracker.Expirations, oldPath)
			expirationTracker.Expirations[newID] = expiryTime
			expirationTracker.saveToFile()
		}
		expirationTracker.mu.Unlock()
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

	http.HandleFunc("/preview/", func(w http.ResponseWriter, r *http.Request) {
		serveFilePreview(tmpl, w, r)
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
		if id == requestedID && isStableUUID(requestedID) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "already_deleted"})
			return
		}
		if record, ok := contentLifecycle.Resolve(requestedID); ok && expectedRevision(r) > 0 && record.Revision != expectedRevision(r) {
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
			err = atomicWriteFile(linksFilePath, []byte(output), 0644)
			if err != nil {
				http.Error(w, "Failed to write links file after deletion", http.StatusInternalServerError)
				return
			}
			record, mutationErr := contentLifecycle.Remove(requestedID, expectedRevision(r))
			if mutationErr != nil {
				_ = atomicWriteFile(linksFilePath, data, 0644)
				if errors.Is(mutationErr, errRevisionConflict) {
					writeRevisionConflict(w, record)
					return
				}
				http.Error(w, mutationErr.Error(), http.StatusInternalServerError)
				return
			}
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
		if os.IsNotExist(err) {
			record, mutationErr := contentLifecycle.Remove(requestedID, 0)
			if mutationErr != nil {
				http.Error(w, mutationErr.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "already_deleted", "id": record.ID})
			return
		}
		if err != nil {
			log.Printf("Failed to delete %s: %v", id, err)
			http.Error(w, "Failed to delete file", http.StatusInternalServerError)
			return
		}
		record, mutationErr := contentLifecycle.Remove(requestedID, expectedRevision(r))
		if mutationErr != nil {
			if errors.Is(mutationErr, errRevisionConflict) {
				writeRevisionConflict(w, record)
				return
			}
			http.Error(w, mutationErr.Error(), http.StatusInternalServerError)
			return
		}
		if strings.HasPrefix(id, "files/") {
			_ = os.Remove(thumbnailPath(id))
		}
		expirationTracker.mu.Lock()
		delete(expirationTracker.Expirations, id)
		expirationTracker.saveToFile()
		expirationTracker.mu.Unlock()
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
		if record, ok := contentLifecycle.Resolve(requestedID); ok && expectedRevision(r) > 0 && record.Revision != expectedRevision(r) {
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
		record, mutationErr := contentLifecycle.SetFavorite(requestedID, value, expectedRevision(r))
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
		if record, ok := contentLifecycle.Resolve(requestedID); ok && expectedRevision(r) > 0 && record.Revision != expectedRevision(r) {
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
		previousContent, err := os.ReadFile(filePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		err = atomicWriteFile(filePath, []byte(content), 0644)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		record, mutationErr := contentLifecycle.Edit(requestedID, expectedRevision(r))
		if mutationErr != nil {
			_ = atomicWriteFile(filePath, previousContent, 0644)
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
	appHandler := browserDevices.gateMiddleware(browserDevices.identityMiddleware(idempotencyMiddleware(http.DefaultServeMux)))
	server := &http.Server{Addr: *listenAddress, Handler: appHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
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
