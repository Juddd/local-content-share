warning: /bin/bash: setlocale: LC_ALL: cannot change locale (C.UTF-8)
package main

import (
	"embed"
	"encoding/json"
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

// SSE client management
var (
	clients   = make(map[chan string]bool)
	clientMux sync.Mutex
)

type Entry struct {
	ID         string    `json:"id"`
	Content    string    `json:"content"`
	Type       string    `json:"type"`
	Filename   string    `json:"filename"`
	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Size       int64     `json:"size"`
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

func saveStreamedFile(r *http.Request, expiry, requestedName string) error {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return err
	}
	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Errorf("missing multipart boundary")
	}
	mr := multipart.NewReader(r.Body, boundary)
	var expiryValue string
	saved := false
	for {
		part, e := mr.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		field := part.FormName()
		if field == "expiry" {
			b, e := io.ReadAll(io.LimitReader(part, 1<<20))
			if e != nil {
				return e
			}
			expiryValue = string(b)
			continue
		}
		if field == "name" {
			if _, e := io.Copy(io.Discard, io.LimitReader(part, 1<<20)); e != nil {
				return e
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
			return e
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
			return e
		}
		if n > maxFileSize {
			return fmt.Errorf("file exceeds 4 GB")
		}
		if e = tmp.Close(); e != nil {
			return e
		}
		if e = os.Rename(tmpName, filepath.Join("data/files", unique)); e != nil {
			return e
		}
		ok = true
		id := filepath.Join("files", unique)
		itemTimeTracker.Create(id)
		if expiryValue != "" && expiryValue != "Never" {
			expirationTracker.SetExpiration(id, expiryValue)
		}
		saved = true
	}
	if !saved {
		return fmt.Errorf("file-upload field is required")
	}
	_ = expiry
	return nil
}

func thumbnailPath(id string) string { return filepath.Join(thumbnailDir, id+".jpg") }

func streamUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := saveStreamedFile(r, "", ""); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "exceeds 4 GB") {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	notifyContentChange()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"status":"created"}`))
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
	_ = os.WriteFile(filepath.Join("data", "item-times.json"), data, 0644)
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
	unit := strings.ToLower(matches[2])
	switch unit {
	case "m": // minutes
		if value < 5 {
			return 5 * time.Minute
		}
		return time.Duration(value) * time.Minute
	case "h": // hours
		return time.Duration(value) * time.Hour
	case "d": // days
		return time.Duration(value) * 24 * time.Hour
	case "w": // weeks
		return time.Duration(value) * 7 * 24 * time.Hour
	case "M": // months
		return time.Duration(value) * 30 * 24 * time.Hour
	case "y": // years
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
	if err := os.WriteFile(expirationFile, data, 0644); err != nil {
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
	}
	if len(expiredFiles) > 0 {
		t.saveToFile()
		notifyContentChange()
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

func handleContentUpdates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	messageChan := make(chan string)
	clientMux.Lock()
	clients[messageChan] = true
	clientMux.Unlock()

	defer func() {
		clientMux.Lock()
		delete(clients, messageChan)
		clientMux.Unlock()
		close(messageChan)
	}()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	// Send an initial message
	fmt.Fprintf(w, "data: %s\n\n", "connected")
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-messageChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			w.(http.Flusher).Flush()
		case <-ticker.C: // send keep-alive msg
			fmt.Fprintf(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func notifyContentChange() {
	clientMux.Lock()
	defer clientMux.Unlock()
	for client := range clients {
		select {
		case client <- "content_updated":
		default:
		}
	}
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
			entries = append(entries, Entry{ID: id, Type: "text", Content: string(data), Filename: file.Name(), CreatedAt: times.CreatedAt, ModifiedAt: times.ModifiedAt})
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
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt.After(entries[j].CreatedAt) })
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
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
			content, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Error reading request body", http.StatusInternalServerError)
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
					filename = time.Now().Format("Jan-02 15-04-05")
				}
				uniqueFileName := generateUniqueFilename("data/text", filename)
				err := os.WriteFile(filepath.Join("data/text", uniqueFileName), []byte(content), 0644)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				itemTimeTracker.Create(filepath.Join("text", uniqueFileName))
				if expiryOption != "Never" {
					fileID := filepath.Join("text", uniqueFileName)
					expirationTracker.SetExpiration(fileID, expiryOption)
				}
				log.Printf("Saved text snippet %s with expiry %s\n", uniqueFileName, expiryOption)
			}
		}
		notifyContentChange()
		// Send succes for AJAX
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Success"))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
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
		client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
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
		notifyContentChange()
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
		oldPath := strings.TrimPrefix(r.URL.Path, "/rename/")
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
			notifyContentChange()
			http.Redirect(w, r, "/", http.StatusSeeOther)
			log.Printf("Renamed link title to %s\n", newName)
			return
		}
		baseDir := filepath.Dir(filepath.Join("data", oldPath))
		newName = generateUniqueFilename(baseDir, newName)

		// Get the new full path
		newPath := filepath.Join(baseDir, newName)
		oldFullPath := filepath.Join("data", oldPath)
		// Check if there's an expiration for this file
		expirationTracker.mu.Lock()
		expiryTime, hasExpiry := expirationTracker.Expirations[oldPath]
		if hasExpiry {
			// Remove old entry and add new one
			delete(expirationTracker.Expirations, oldPath)
			relNewPath := strings.TrimPrefix(newPath, "data/")
			relNewPath = strings.ReplaceAll(relNewPath, "\\", "/") // Ensure cross-platform path separators
			expirationTracker.Expirations[relNewPath] = expiryTime
			expirationTracker.saveToFile()
		}
		expirationTracker.mu.Unlock()
		// Rename the file
		err := os.Rename(oldFullPath, newPath)
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
		itemTimeTracker.Rename(oldPath, newID)
		notifyContentChange()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Renamed %s to %s\n", oldPath, newName)
	})

