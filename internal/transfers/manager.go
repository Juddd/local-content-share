package transfers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type File struct {
	Storage  string
	Filename string
	Size     int64
	Expiry   string
}

type DownloadUpdate struct {
	Filename string
	Received int64
	Total    int64
}

type Observer func(DownloadUpdate)
type MetadataCommit func(storage, expiry, preferredID string) error

type transaction struct {
	ID          string `json:"id"`
	Temp        string `json:"temp"`
	Final       string `json:"final"`
	Storage     string `json:"storage"`
	Filename    string `json:"filename"`
	Expiry      string `json:"expiry"`
	PreferredID string `json:"preferredId,omitempty"`
	Size        int64  `json:"size"`
}

type cachedUpload struct {
	Files   []File
	Created time.Time
}
type keyLock struct {
	mutex sync.Mutex
	refs  int
}

// Manager owns the complete file publication lifecycle: staging, limits,
// durable commit intent, metadata commit, atomic publication and recovery.
type Manager struct {
	files        string
	transactions string
	cachePath    string
	maxUpload    int64
	maxDownload  int64
	commit       MetadataCommit
	mu           sync.Mutex
	cache        map[string]cachedUpload
	locks        map[string]*keyLock
	reserved     map[string]bool
}

func NewManager(root string, maxUpload, maxDownload int64, commit MetadataCommit) (*Manager, error) {
	m := &Manager{files: filepath.Join(root, "files"), transactions: filepath.Join(root, "transfer-transactions"), cachePath: filepath.Join(root, "transfer-results.json"), maxUpload: maxUpload, maxDownload: maxDownload, commit: commit, cache: map[string]cachedUpload{}, locks: map[string]*keyLock{}, reserved: map[string]bool{}}
	if err := os.MkdirAll(m.files, 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.transactions, 0700); err != nil {
		return nil, err
	}
	if err := m.loadCache(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Recover() ([]File, error) {
	entries, err := os.ReadDir(m.transactions)
	if err != nil {
		return nil, err
	}
	var recovered []File
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		journal := filepath.Join(m.transactions, entry.Name())
		data, readErr := os.ReadFile(journal)
		if readErr != nil {
			return recovered, readErr
		}
		var tx transaction
		if err = json.Unmarshal(data, &tx); err != nil {
			return recovered, err
		}
		if err = m.validateTransaction(tx); err != nil {
			return recovered, err
		}
		if _, err = os.Stat(tx.Final); os.IsNotExist(err) {
			if _, tempErr := os.Stat(tx.Temp); tempErr != nil {
				return recovered, tempErr
			}
		} else if err != nil {
			return recovered, err
		}
		if m.commit != nil {
			if err = m.commit(tx.Storage, tx.Expiry, tx.PreferredID); err != nil {
				return recovered, err
			}
		}
		if _, err = os.Stat(tx.Final); os.IsNotExist(err) {
			if err = os.Rename(tx.Temp, tx.Final); err != nil {
				return recovered, err
			}
			if err = syncDir(m.files); err != nil {
				return recovered, err
			}
		}
		_ = os.Remove(tx.Temp)
		if err = os.Remove(journal); err != nil && !os.IsNotExist(err) {
			return recovered, err
		}
		_ = syncDir(m.transactions)
		m.release(tx.Filename)
		recovered = append(recovered, File{Storage: tx.Storage, Filename: tx.Filename, Size: tx.Size, Expiry: tx.Expiry})
	}
	return recovered, nil
}

func (m *Manager) validateTransaction(tx transaction) error {
	if tx.Filename == "" || tx.Storage != filepath.ToSlash(filepath.Join("files", tx.Filename)) || filepath.Clean(tx.Final) != filepath.Join(m.files, tx.Filename) {
		return fmt.Errorf("invalid transfer transaction %q", tx.ID)
	}
	if !pathWithin(m.transactions, tx.Temp) || !pathWithin(m.files, tx.Final) {
		return fmt.Errorf("invalid transfer transaction paths %q", tx.ID)
	}
	return nil
}

func pathWithin(root, target string) bool {
	rootPath, rootErr := filepath.Abs(root)
	targetPath, targetErr := filepath.Abs(target)
	if rootErr != nil || targetErr != nil {
		return false
	}
	relative, err := filepath.Rel(rootPath, targetPath)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (m *Manager) SaveMultipart(key, contentType string, body io.Reader, requestedName, preferredID string) ([]File, error) {
	unlock := func() {}
	if strings.TrimSpace(key) != "" {
		unlock = m.lock(key)
		defer unlock()
		m.mu.Lock()
		cached, ok := m.cache[key]
		m.mu.Unlock()
		if ok {
			return append([]File(nil), cached.Files...), nil
		}
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("missing multipart boundary")
	}
	reader := multipart.NewReader(body, boundary)
	expiry := "Never"
	saved := false
	var files []File
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, nextErr
		}
		switch part.FormName() {
		case "expiry":
			value, readErr := io.ReadAll(io.LimitReader(part, 1<<20))
			if readErr != nil {
				return nil, readErr
			}
			if strings.TrimSpace(string(value)) != "" {
				expiry = string(value)
			}
		case "name":
			_, err = io.Copy(io.Discard, io.LimitReader(part, 1<<20))
			if err != nil {
				return nil, err
			}
		case "file-upload":
			name := requestedName
			if name == "" {
				name = part.FileName()
			}
			if name == "" {
				name = "upload.bin"
			}
			file, saveErr := m.saveReader(part, name, expiry, preferredID, m.maxUpload)
			if saveErr != nil {
				return nil, saveErr
			}
			files = append(files, file)
			saved = true
		}
	}
	if !saved {
		return nil, fmt.Errorf("file-upload field is required")
	}
	if key != "" {
		m.mu.Lock()
		m.cache[key] = cachedUpload{Files: append([]File(nil), files...), Created: time.Now()}
		for value, cached := range m.cache {
			if time.Since(cached.Created) > 24*time.Hour {
				delete(m.cache, value)
			}
		}
		err = m.saveCacheLocked()
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (m *Manager) loadCache() error {
	data, err := os.ReadFile(m.cachePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored map[string]cachedUpload
	if err = json.Unmarshal(data, &stored); err != nil {
		return err
	}
	now := time.Now()
	for key, cached := range stored {
		valid := key != "" && now.Sub(cached.Created) <= 24*time.Hour && len(cached.Files) > 0
		for _, file := range cached.Files {
			if !valid || file.Storage != filepath.ToSlash(filepath.Join("files", file.Filename)) {
				valid = false
				break
			}
			if _, statErr := os.Stat(filepath.Join(m.files, file.Filename)); statErr != nil {
				valid = false
				break
			}
		}
		if valid {
			m.cache[key] = cached
		}
	}
	return nil
}

func (m *Manager) saveCacheLocked() error {
	payload, err := json.Marshal(m.cache)
	if err != nil {
		return err
	}
	return atomicWrite(m.cachePath, append(payload, '\n'), 0600)
}

func (m *Manager) Publish(reader io.Reader, name, expiry, preferredID string) (File, error) {
	if strings.TrimSpace(name) == "" {
		name = "upload.bin"
	}
	if strings.TrimSpace(expiry) == "" {
		expiry = "Never"
	}
	return m.saveReader(reader, name, expiry, preferredID, m.maxUpload)
}

func (m *Manager) Download(ctx context.Context, source *url.URL, requestedName, expiry string, transport http.RoundTripper, observer Observer) (File, error) {
	client := &http.Client{Timeout: 30 * time.Minute, Transport: transport, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		_, err := PublicURL(request.URL.String())
		return err
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.String(), nil)
	if err != nil {
		return File{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return File{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return File{}, fmt.Errorf("remote server returned %s", response.Status)
	}
	if response.ContentLength > m.maxDownload {
		return File{}, fmt.Errorf("remote file exceeds 8 GB")
	}
	name := DownloadFilename(response, source, requestedName)
	unique := m.reserve(name)
	if observer != nil {
		observer(DownloadUpdate{Filename: unique, Total: response.ContentLength})
	}
	return m.saveReaderAs(&progressReader{reader: response.Body, total: response.ContentLength, limit: m.maxDownload, filename: unique, observer: observer}, unique, expiry, "", m.maxDownload)
}

func (m *Manager) saveReader(reader io.Reader, name, expiry, preferredID string, limit int64) (File, error) {
	unique := m.reserve(name)
	return m.saveReaderAs(reader, unique, expiry, preferredID, limit)
}

func (m *Manager) saveReaderAs(reader io.Reader, unique, expiry, preferredID string, limit int64) (File, error) {
	releaseReservation := true
	defer func() {
		if releaseReservation {
			m.release(unique)
		}
	}()
	temp, err := os.CreateTemp(m.transactions, ".payload-*.tmp")
	if err != nil {
		return File{}, err
	}
	tempName := temp.Name()
	journal := ""
	published := false
	defer func() {
		_ = temp.Close()
		if !published && journal == "" {
			_ = os.Remove(tempName)
		}
	}()
	size, err := io.Copy(temp, io.LimitReader(reader, limit+1))
	if err != nil {
		return File{}, err
	}
	if size > limit {
		return File{}, fmt.Errorf("file exceeds %s", formatLimit(limit))
	}
	if err = temp.Sync(); err != nil {
		return File{}, err
	}
	if err = temp.Close(); err != nil {
		return File{}, err
	}
	if filepath.Ext(unique) == "" {
		if extension := DetectedExtension(tempName); extension != "" {
			unique = m.replaceReservation(unique, unique+extension)
		}
	}
	storage := filepath.ToSlash(filepath.Join("files", unique))
	final := filepath.Join(m.files, unique)
	tx := transaction{ID: fmt.Sprintf("%d-%06d", time.Now().UnixNano(), rand.Intn(1000000)), Temp: tempName, Final: final, Storage: storage, Filename: unique, Expiry: expiry, PreferredID: preferredID, Size: size}
	journal = filepath.Join(m.transactions, tx.ID+".json")
	payload, _ := json.Marshal(tx)
	if err = atomicWrite(journal, payload, 0600); err != nil {
		journal = ""
		return File{}, err
	}
	releaseReservation = false
	if m.commit != nil {
		if err = m.commit(storage, expiry, preferredID); err != nil {
			return File{}, err
		}
	}
	if err = os.Rename(tempName, final); err != nil {
		return File{}, err
	}
	published = true
	_ = syncDir(m.files)
	if err = os.Remove(journal); err != nil {
		return File{}, err
	}
	journal = ""
	_ = syncDir(m.transactions)
	releaseReservation = true
	return File{Storage: storage, Filename: unique, Size: size, Expiry: expiry}, nil
}

func (m *Manager) lock(key string) func() {
	m.mu.Lock()
	entry := m.locks[key]
	if entry == nil {
		entry = &keyLock{}
		m.locks[key] = entry
	}
	entry.refs++
	m.mu.Unlock()
	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		m.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(m.locks, key)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) reserve(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserveLocked(name)
}
func (m *Manager) reserveLocked(name string) string {
	name = sanitizeFilename(name)
	candidate := name
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			candidate = fmt.Sprintf("%04d-%s", rand.Intn(10000), name)
		}
		if !m.reserved[candidate] {
			if _, err := os.Stat(filepath.Join(m.files, candidate)); os.IsNotExist(err) {
				m.reserved[candidate] = true
				return candidate
			}
		}
	}
}
func (m *Manager) release(name string) { m.mu.Lock(); delete(m.reserved, name); m.mu.Unlock() }
func (m *Manager) replaceReservation(oldName, newName string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reserved, oldName)
	return m.reserveLocked(newName)
}

type progressReader struct {
	reader                 io.Reader
	filename               string
	total, received, limit int64
	observer               Observer
}

func (p *progressReader) Read(buffer []byte) (int, error) {
	count, err := p.reader.Read(buffer)
	p.received += int64(count)
	if p.received > p.limit {
		return count, fmt.Errorf("remote file exceeds 8 GB")
	}
	if count > 0 && p.observer != nil {
		p.observer(DownloadUpdate{Filename: p.filename, Received: p.received, Total: p.total})
	}
	return count, err
}

func PublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid HTTP URL")
	}
	addresses, err := net.LookupIP(u.Hostname())
	if err != nil {
		return nil, err
	}
	for _, ip := range addresses {
		if unsafeIP(ip) {
			return nil, fmt.Errorf("private network URL is not allowed")
		}
	}
	return u, nil
}
func PublicTransport() *http.Transport {
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
			if unsafeIP(ip) {
				return nil, fmt.Errorf("private network URL is not allowed")
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("host has no IP address")
		}
		var last error
		for _, ip := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			last = dialErr
		}
		return nil, last
	}
	return transport
}
func unsafeIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

func DownloadFilename(response *http.Response, source *url.URL, requested string) string {
	if name := strings.TrimSpace(requested); name != "" {
		return filepath.Base(name)
	}
	if _, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition")); err == nil {
		if name := filepath.Base(params["filename"]); name != "." && name != "" {
			return name
		}
	}
	if name := filepath.Base(source.Path); name != "." && name != "/" && name != "" {
		if decoded, err := url.PathUnescape(name); err == nil {
			return decoded
		}
		return name
	}
	return "download-" + time.Now().Format("20060102-150405")
}
func UniqueFilename(baseDir, baseName string) string {
	name := sanitizeFilename(baseName)
	if _, err := os.Stat(filepath.Join(baseDir, name)); os.IsNotExist(err) {
		return name
	}
	for {
		candidate := fmt.Sprintf("%04d-%s", rand.Intn(10000), name)
		if _, err := os.Stat(filepath.Join(baseDir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
}
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	pattern := regexp.MustCompile(`[^\p{L}\p{N}\p{M}\s\.\-_()\[\]]`)
	return pattern.ReplaceAllString(name, "-")
}
func DetectedExtension(filename string) string {
	file, err := os.Open(filename)
	if err != nil {
		return ""
	}
	defer file.Close()
	header := make([]byte, 512)
	count, _ := io.ReadFull(file, header)
	switch strings.SplitN(http.DetectContentType(header[:count]), ";", 2)[0] {
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
	}
	return ""
}
func formatLimit(limit int64) string {
	if limit == 4<<30 {
		return "4 GB"
	}
	if limit == 8<<30 {
		return "8 GB"
	}
	return fmt.Sprintf("%d bytes", limit)
}
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".transaction-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	done := false
	defer func() {
		_ = temp.Close()
		if !done {
			_ = os.Remove(name)
		}
	}()
	if err = temp.Chmod(mode); err != nil {
		return err
	}
	if _, err = temp.Write(data); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	done = true
	_ = syncDir(filepath.Dir(path))
	return nil
}
func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
