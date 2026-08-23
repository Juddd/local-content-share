package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
)

// IdentityStore separates an item's durable identity from its mutable storage path.
// The file lives beside the payload data and is migrated lazily, so existing installs
// keep every payload in place.
type IdentityRecord struct {
	ID       string `json:"id"`
	Storage  string `json:"storageId"`
	Revision uint64 `json:"revision"`
}

type IdentityStore struct {
	mu      sync.Mutex
	ByID    map[string]*IdentityRecord `json:"byId"`
	ByStore map[string]string          `json:"byStorage"`
	path    string
}

var identities *IdentityStore
var errRevisionConflict = errors.New("revision conflict")
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func newIdentityStore(name string) *IdentityStore {
	s := &IdentityStore{ByID: map[string]*IdentityRecord{}, ByStore: map[string]string{}, path: name}
	if b, err := osReadFile(name); err == nil {
		_ = json.Unmarshal(b, s)
	}
	if s.ByID == nil {
		s.ByID = map[string]*IdentityRecord{}
	}
	if s.ByStore == nil {
		s.ByStore = map[string]string{}
	}
	return s
}

// osReadFile is a variable solely to keep migration tests deterministic.
var osReadFile = func(name string) ([]byte, error) { return os.ReadFile(name) }

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", x[:8], x[8:12], x[12:16], x[16:20], x[20:])
}

func (s *IdentityStore) saveLocked() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, b, 0644)
}

func (s *IdentityStore) Ensure(storage string) IdentityRecord {
	return s.EnsureWithID(storage, "")
}

func (s *IdentityStore) EnsureWithID(storage, preferred string) IdentityRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.ByStore[storage]; id != "" {
		if r := s.ByID[id]; r != nil {
			return *r
		}
	}
	if !uuidPattern.MatchString(preferred) || s.ByID[preferred] != nil {
		preferred = newUUID()
	}
	r := &IdentityRecord{ID: preferred, Storage: storage, Revision: 1}
	s.ByID[r.ID], s.ByStore[storage] = r, r.ID
	_ = s.saveLocked()
	return *r
}

func (s *IdentityStore) MigrateStorage(oldStorage, newStorage string) {
	if oldStorage == newStorage {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oldID, newID := s.ByStore[oldStorage], s.ByStore[newStorage]
	if oldID == "" {
		return
	}
	delete(s.ByStore, oldStorage)
	if newID != "" {
		if oldID != newID {
			delete(s.ByID, oldID)
		}
	} else if r := s.ByID[oldID]; r != nil {
		r.Storage = newStorage
		s.ByStore[newStorage] = oldID
	}
	_ = s.saveLocked()
}

func isStableUUID(value string) bool { return uuidPattern.MatchString(value) }

func (s *IdentityStore) Resolve(value string) (IdentityRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.ByID[value]; r != nil {
		return *r, true
	}
	if id := s.ByStore[value]; id != "" && s.ByID[id] != nil {
		return *s.ByID[id], true
	}
	return IdentityRecord{}, false
}

func (s *IdentityStore) Mutate(idOrStorage, newStorage string, expected uint64) (IdentityRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := idOrStorage
	if mapped := s.ByStore[idOrStorage]; mapped != "" {
		id = mapped
	}
	r := s.ByID[id]
	if r == nil {
		return IdentityRecord{}, fmt.Errorf("identity not found")
	}
	if expected > 0 && expected != r.Revision {
		return *r, errRevisionConflict
	}
	if newStorage != "" && newStorage != r.Storage {
		delete(s.ByStore, r.Storage)
		r.Storage = newStorage
		s.ByStore[newStorage] = r.ID
	}
	r.Revision++
	if err := s.saveLocked(); err != nil {
		return IdentityRecord{}, err
	}
	return *r, nil
}

func (s *IdentityStore) Delete(idOrStorage string, expected uint64) (IdentityRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := idOrStorage
	if mapped := s.ByStore[idOrStorage]; mapped != "" {
		id = mapped
	}
	r := s.ByID[id]
	if r == nil {
		return IdentityRecord{}, fmt.Errorf("identity not found")
	}
	if expected > 0 && expected != r.Revision {
		return *r, errRevisionConflict
	}
	copy := *r
	copy.Revision++
	delete(s.ByStore, r.Storage)
	delete(s.ByID, id)
	if err := s.saveLocked(); err != nil {
		return IdentityRecord{}, err
	}
	return copy, nil
}

func stableEntry(e *Entry) *Entry {
	if e == nil || identities == nil {
		return e
	}
	storage := e.ID
	r := identities.Ensure(storage)
	e.ID, e.StorageID, e.Revision = r.ID, storage, r.Revision
	return e
}

func resolveStorageID(value string) string {
	if identities != nil {
		if r, ok := identities.Resolve(value); ok {
			return r.Storage
		}
	}
	return value
}
