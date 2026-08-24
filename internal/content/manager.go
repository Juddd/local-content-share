package content

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var ErrRevisionConflict = errors.New("revision conflict")

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func IsStableID(value string) bool { return uuidPattern.MatchString(value) }

type Identity struct {
	ID       string `json:"id"`
	Storage  string `json:"storageId"`
	Revision uint64 `json:"revision"`
}

type Times struct {
	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

type Snapshot struct {
	Identity
	Times
	Favorite bool
}

type state struct {
	ByID      map[string]*Identity `json:"byId"`
	ByStorage map[string]string    `json:"byStorage"`
	Favorites map[string]bool      `json:"favorites"`
	Times     map[string]Times     `json:"times"`
}

// Manager owns every metadata invariant for a Content Item. Payload bytes stay
// behind the caller's storage adapter; identity, revision, time and favorite
// state are committed together through this interface.
type Manager struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
	data state
}

func NewManager(root string) (*Manager, error) {
	m := &Manager{path: filepath.Join(root, "content-state.json"), now: time.Now}
	m.data = state{ByID: map[string]*Identity{}, ByStorage: map[string]string{}, Favorites: map[string]bool{}, Times: map[string]Times{}}
	if data, err := os.ReadFile(m.path); err == nil {
		if err := json.Unmarshal(data, &m.data); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else if err := m.migrateLegacy(root); err != nil {
		return nil, err
	}
	m.ensureMaps()
	return m, nil
}

func (m *Manager) ensureMaps() {
	if m.data.ByID == nil {
		m.data.ByID = map[string]*Identity{}
	}
	if m.data.ByStorage == nil {
		m.data.ByStorage = map[string]string{}
	}
	if m.data.Favorites == nil {
		m.data.Favorites = map[string]bool{}
	}
	if m.data.Times == nil {
		m.data.Times = map[string]Times{}
	}
}

func (m *Manager) migrateLegacy(root string) error {
	var identities struct {
		ByID      map[string]*Identity `json:"byId"`
		ByStorage map[string]string    `json:"byStorage"`
	}
	if data, err := os.ReadFile(filepath.Join(root, "identities.json")); err == nil {
		if err := json.Unmarshal(data, &identities); err != nil {
			return err
		}
		m.data.ByID, m.data.ByStorage = identities.ByID, identities.ByStorage
	}
	if data, err := os.ReadFile(filepath.Join(root, "favorites.json")); err == nil {
		if err := json.Unmarshal(data, &m.data.Favorites); err != nil {
			return err
		}
	}
	var times struct {
		Items map[string]Times `json:"items"`
	}
	if data, err := os.ReadFile(filepath.Join(root, "item-times.json")); err == nil {
		if err := json.Unmarshal(data, &times); err != nil {
			return err
		}
		m.data.Times = times.Items
	}
	m.ensureMaps()
	if len(m.data.ByID)+len(m.data.Favorites)+len(m.data.Times) > 0 {
		return m.saveLocked()
	}
	return nil
}

func (m *Manager) View(storage string, fallback time.Time) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, changed := m.ensureLocked(storage, "")
	times, ok := m.data.Times[storage]
	if !ok {
		times = Times{CreatedAt: fallback, ModifiedAt: fallback}
		m.data.Times[storage] = times
		changed = true
	}
	favorite := m.data.Favorites[storage]
	if changed {
		_ = m.saveLocked()
	}
	return Snapshot{Identity: record, Times: times, Favorite: favorite}
}

func (m *Manager) Add(storage, preferredID string) (Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.beginMutationLocked()
	record, changed := m.ensureLocked(storage, preferredID)
	if _, ok := m.data.Times[storage]; !ok {
		now := m.now()
		m.data.Times[storage] = Times{CreatedAt: now, ModifiedAt: now}
		changed = true
	}
	if changed {
		return record, m.saveMutationLocked(previous)
	}
	return record, nil
}

func (m *Manager) Resolve(value string) (Identity, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolveLocked(value)
}

func (m *Manager) ResolveStorage(value string) string {
	if record, ok := m.Resolve(value); ok {
		return record.Storage
	}
	return value
}

func (m *Manager) Rename(value, newStorage string, expected uint64) (Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.resolvePointerLocked(value)
	if !ok {
		return Identity{}, fmt.Errorf("identity not found")
	}
	if expected > 0 && expected != record.Revision {
		return *record, ErrRevisionConflict
	}
	previous := m.beginMutationLocked()
	record, _ = m.resolvePointerLocked(value)
	oldStorage := record.Storage
	if oldStorage != newStorage {
		delete(m.data.ByStorage, oldStorage)
		record.Storage = newStorage
		m.data.ByStorage[newStorage] = record.ID
		if times, found := m.data.Times[oldStorage]; found {
			delete(m.data.Times, oldStorage)
			times.ModifiedAt = m.now()
			m.data.Times[newStorage] = times
		}
		if m.data.Favorites[oldStorage] {
			delete(m.data.Favorites, oldStorage)
			m.data.Favorites[newStorage] = true
		}
	}
	record.Revision++
	return *record, m.saveMutationLocked(previous)
}

func (m *Manager) Edit(value string, expected uint64) (Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.resolvePointerLocked(value)
	if !ok {
		return Identity{}, fmt.Errorf("identity not found")
	}
	if expected > 0 && expected != record.Revision {
		return *record, ErrRevisionConflict
	}
	previous := m.beginMutationLocked()
	record, _ = m.resolvePointerLocked(value)
	times := m.data.Times[record.Storage]
	if times.CreatedAt.IsZero() {
		times.CreatedAt = m.now()
	}
	times.ModifiedAt = m.now()
	m.data.Times[record.Storage] = times
	record.Revision++
	return *record, m.saveMutationLocked(previous)
}

func (m *Manager) SetFavorite(value string, favorite bool, expected uint64) (Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.resolvePointerLocked(value)
	if !ok {
		return Identity{}, fmt.Errorf("identity not found")
	}
	if expected > 0 && expected != record.Revision {
		return *record, ErrRevisionConflict
	}
	previous := m.beginMutationLocked()
	record, _ = m.resolvePointerLocked(value)
	if favorite {
		m.data.Favorites[record.Storage] = true
	} else {
		delete(m.data.Favorites, record.Storage)
	}
	record.Revision++
	return *record, m.saveMutationLocked(previous)
}

func (m *Manager) Remove(value string, expected uint64) (Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.resolvePointerLocked(value)
	if !ok {
		return Identity{}, fmt.Errorf("identity not found")
	}
	if expected > 0 && expected != record.Revision {
		return *record, ErrRevisionConflict
	}
	previous := m.beginMutationLocked()
	record, _ = m.resolvePointerLocked(value)
	removed := *record
	removed.Revision++
	delete(m.data.ByID, record.ID)
	delete(m.data.ByStorage, record.Storage)
	delete(m.data.Favorites, record.Storage)
	delete(m.data.Times, record.Storage)
	return removed, m.saveMutationLocked(previous)
}

func (m *Manager) RemoveStorage(storage string) (Identity, error) { return m.Remove(storage, 0) }

func (m *Manager) MigrateLegacyStorage(oldStorage, newStorage string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	oldID, newID := m.data.ByStorage[oldStorage], m.data.ByStorage[newStorage]
	if oldID == "" || oldStorage == newStorage {
		return nil
	}
	previous := m.beginMutationLocked()
	oldID, newID = m.data.ByStorage[oldStorage], m.data.ByStorage[newStorage]
	delete(m.data.ByStorage, oldStorage)
	if newID != "" {
		if oldID != newID {
			delete(m.data.ByID, oldID)
		}
	} else if record := m.data.ByID[oldID]; record != nil {
		record.Storage = newStorage
		m.data.ByStorage[newStorage] = oldID
	}
	if times, ok := m.data.Times[oldStorage]; ok {
		delete(m.data.Times, oldStorage)
		m.data.Times[newStorage] = times
	}
	if m.data.Favorites[oldStorage] {
		delete(m.data.Favorites, oldStorage)
		m.data.Favorites[newStorage] = true
	}
	return m.saveMutationLocked(previous)
}

func (m *Manager) beginMutationLocked() state {
	previous := m.data
	m.data = cloneState(previous)
	return previous
}

func (m *Manager) saveMutationLocked(previous state) error {
	if err := m.saveLocked(); err != nil {
		m.data = previous
		return err
	}
	return nil
}

func cloneState(source state) state {
	copy := state{ByID: make(map[string]*Identity, len(source.ByID)), ByStorage: make(map[string]string, len(source.ByStorage)), Favorites: make(map[string]bool, len(source.Favorites)), Times: make(map[string]Times, len(source.Times))}
	for id, identity := range source.ByID {
		if identity != nil {
			value := *identity
			copy.ByID[id] = &value
		}
	}
	for storage, id := range source.ByStorage {
		copy.ByStorage[storage] = id
	}
	for storage, favorite := range source.Favorites {
		copy.Favorites[storage] = favorite
	}
	for storage, times := range source.Times {
		copy.Times[storage] = times
	}
	return copy
}

func (m *Manager) ensureLocked(storage, preferred string) (Identity, bool) {
	if id := m.data.ByStorage[storage]; id != "" {
		if record := m.data.ByID[id]; record != nil {
			return *record, false
		}
	}
	if !uuidPattern.MatchString(preferred) || m.data.ByID[preferred] != nil {
		preferred = newUUID()
	}
	record := &Identity{ID: preferred, Storage: storage, Revision: 1}
	m.data.ByID[record.ID], m.data.ByStorage[storage] = record, record.ID
	return *record, true
}

func (m *Manager) resolveLocked(value string) (Identity, bool) {
	record, ok := m.resolvePointerLocked(value)
	if !ok {
		return Identity{}, false
	}
	return *record, true
}

func (m *Manager) resolvePointerLocked(value string) (*Identity, bool) {
	if record := m.data.ByID[value]; record != nil {
		return record, true
	}
	if id := m.data.ByStorage[value]; id != "" && m.data.ByID[id] != nil {
		return m.data.ByID[id], true
	}
	return nil, false
}

func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(m.data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.path, append(data, '\n'), 0644)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".content-state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(name)
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	committed = true
	if directory, openErr := os.Open(filepath.Dir(path)); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func newUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	value := hex.EncodeToString(bytes[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", value[:8], value[8:12], value[12:16], value[16:20], value[20:])
}
