package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

type mutationResponse struct {
	Status  int         `json:"status"`
	Header  http.Header `json:"header"`
	Body    []byte      `json:"body"`
	Created time.Time   `json:"created"`
}
type mutationCache struct {
	sync.Mutex
	Values map[string]mutationResponse `json:"values"`
	path   string
}

var mutations *mutationCache

func newMutationCache(path string) *mutationCache {
	m := &mutationCache{Values: map[string]mutationResponse{}, path: path}
	if b, e := osReadFile(path); e == nil {
		_ = json.Unmarshal(b, m)
	}
	if m.Values == nil {
		m.Values = map[string]mutationResponse{}
	}
	return m
}
func (m *mutationCache) saveLocked() {
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = atomicWriteFile(m.path, b, 0600)
}

type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *captureWriter) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
	c.ResponseWriter.WriteHeader(s)
}
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}
func idempotencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if r.Method != "POST" || key == "" || r.URL.Path == "/upload-stream" {
			next.ServeHTTP(w, r)
			return
		}
		unlock := lockUploadKey("mutation:" + key)
		defer unlock()
		mutations.Lock()
		cached, ok := mutations.Values[key]
		mutations.Unlock()
		if ok {
			for k, vs := range cached.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(cached.Status)
			_, _ = w.Write(cached.Body)
			return
		}
		cw := &captureWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		if cw.status == 0 {
			cw.status = 200
		}
		if cw.status >= 200 && cw.status < 300 {
			mutations.Lock()
			mutations.Values[key] = mutationResponse{Status: cw.status, Header: cw.Header().Clone(), Body: append([]byte(nil), cw.body.Bytes()...), Created: time.Now()}
			for k, v := range mutations.Values {
				if time.Since(v.Created) > 30*24*time.Hour {
					delete(mutations.Values, k)
				}
			}
			mutations.saveLocked()
			mutations.Unlock()
		}
	})
}
