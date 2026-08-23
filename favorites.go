package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type favoriteStore struct {
	sync.RWMutex
	path  string
	items map[string]bool
}

func newFavoriteStore(path string) *favoriteStore {
	s := &favoriteStore{path: path, items: map[string]bool{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.items)
	}
	return s
}

func (s *favoriteStore) Is(id string) bool {
	s.RLock()
	defer s.RUnlock()
	return s.items[id]
}

func (s *favoriteStore) Set(id string, value bool) error {
	s.Lock()
	defer s.Unlock()
	if value {
		s.items[id] = true
	} else {
		delete(s.items, id)
	}
	return s.saveLocked()
}

func (s *favoriteStore) Delete(id string) error { return s.Set(id, false) }

func (s *favoriteStore) Rename(oldID, newID string) error {
	s.Lock()
	defer s.Unlock()
	if s.items[oldID] {
		delete(s.items, oldID)
		s.items[newID] = true
	}
	return s.saveLocked()
}

func (s *favoriteStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, append(data, '\n'), 0644)
}
