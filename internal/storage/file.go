package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// fileStore implements Store interface using JSON file storage
type fileStore struct {
	mu sync.RWMutex

	// file path for storage
	filePath string

	// in-memory data
	userSessions     map[int64]string
	sessions         map[string]*SessionMeta
	models           map[string]*ModelMeta
	userLastModels   map[int64]*modelPreference
	userLastSessions map[int64]string

	// dirty flag to track changes
	dirty bool
}

// modelPreference stores provider and model ID for a user
type modelPreference struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// NewFileStore creates a new file-based store
func NewFileStore(filePath string) (Store, error) {
	if err := migrateLegacyStateFileIfNeeded(filePath); err != nil {
		return nil, fmt.Errorf("failed to migrate legacy state file: %w", err)
	}

	store := &fileStore{
		filePath:         filePath,
		userSessions:     make(map[int64]string),
		sessions:         make(map[string]*SessionMeta),
		models:           make(map[string]*ModelMeta),
		userLastModels:   make(map[int64]*modelPreference),
		userLastSessions: make(map[int64]string),
		dirty:            false,
	}

	// Try to load existing data
	if err := store.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load storage file: %w", err)
	}

	return store, nil
}

func migrateLegacyStateFileIfNeeded(filePath string) error {
	if filepath.Base(filePath) != "bot-state.json" {
		return nil
	}

	if _, err := os.Stat(filePath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	legacyPath := filepath.Join(filepath.Dir(filePath), "sessions.json")
	if _, err := os.Stat(legacyPath); err == nil {
		return os.Rename(legacyPath, filePath)
	} else if os.IsNotExist(err) {
		return nil
	} else {
		return err
	}
}

// load reads data from file
func (f *fileStore) load() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, err := os.ReadFile(f.filePath)
	if err != nil {
		return err
	}

	var storedData struct {
		UserSessions     map[int64]string           `json:"user_sessions"`
		Sessions         map[string]*SessionMeta    `json:"sessions"`
		Models           map[string]*ModelMeta      `json:"models,omitempty"`
		UserLastModels   map[int64]*modelPreference `json:"user_last_models,omitempty"`
		UserLastSessions map[int64]string           `json:"user_last_sessions,omitempty"`
	}

	if err := json.Unmarshal(data, &storedData); err != nil {
		return fmt.Errorf("failed to unmarshal storage data: %w", err)
	}

	f.userSessions = storedData.UserSessions
	f.sessions = storedData.Sessions
	f.models = storedData.Models
	if f.models == nil {
		f.models = make(map[string]*ModelMeta)
	}
	f.userLastModels = storedData.UserLastModels
	if f.userLastModels == nil {
		f.userLastModels = make(map[int64]*modelPreference)
	}
	f.userLastSessions = storedData.UserLastSessions
	if f.userLastSessions == nil {
		f.userLastSessions = make(map[int64]string)
	}
	f.dirty = false

	return nil
}

// save writes data to file
func (f *fileStore) save() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.saveLocked()
}

// saveLocked writes data to file without acquiring locks
// Caller must hold at least a read lock
func (f *fileStore) saveLocked() error {
	storedData := struct {
		UserSessions     map[int64]string           `json:"user_sessions"`
		Sessions         map[string]*SessionMeta    `json:"sessions"`
		Models           map[string]*ModelMeta      `json:"models,omitempty"`
		UserLastModels   map[int64]*modelPreference `json:"user_last_models,omitempty"`
		UserLastSessions map[int64]string           `json:"user_last_sessions,omitempty"`
	}{
		UserSessions:     f.userSessions,
		Sessions:         f.sessions,
		Models:           f.models,
		UserLastModels:   f.userLastModels,
		UserLastSessions: f.userLastSessions,
	}

	data, err := json.MarshalIndent(storedData, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal storage data: %w", err)
	}

	// Write to temporary file first
	tmpPath := f.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temporary file: %w", err)
	}

	// Rename to final path (atomic replace)
	if err := os.Rename(tmpPath, f.filePath); err != nil {
		// Try to clean up temporary file
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temporary file: %w", err)
	}

	f.dirty = false
	return nil
}

// markDirty marks the store as needing save
func (f *fileStore) markDirty() {
	f.dirty = true
	// Could implement asynchronous saving here
}

// StoreUserSession stores a user-to-session mapping
func (f *fileStore) StoreUserSession(userID int64, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.userSessions[userID] = sessionID
	f.markDirty()
	return f.saveLocked()
}

// GetUserSession retrieves a session ID for a user
func (f *fileStore) GetUserSession(userID int64) (string, bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	sessionID, exists := f.userSessions[userID]
	return sessionID, exists, nil
}

// DeleteUserSession removes a user-to-session mapping
func (f *fileStore) DeleteUserSession(userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.userSessions, userID)
	f.markDirty()
	return f.saveLocked()
}

// StoreSessionMeta stores session metadata
func (f *fileStore) StoreSessionMeta(meta *SessionMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sessions[meta.SessionID] = meta
	f.markDirty()
	return f.saveLocked()
}

// GetSessionMeta retrieves session metadata
func (f *fileStore) GetSessionMeta(sessionID string) (*SessionMeta, bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	meta, exists := f.sessions[sessionID]
	return meta, exists, nil
}

// DeleteSessionMeta removes session metadata
func (f *fileStore) DeleteSessionMeta(sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Remove session metadata
	delete(f.sessions, sessionID)

	// Remove any user session mappings that reference this session
	for userID, userSessionID := range f.userSessions {
		if userSessionID == sessionID {
			delete(f.userSessions, userID)
		}
	}

	f.markDirty()
	return f.saveLocked()
}

// ListSessions returns all session metadata
func (f *fileStore) ListSessions() ([]*SessionMeta, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	sessions := make([]*SessionMeta, 0, len(f.sessions))
	for _, meta := range f.sessions {
		sessions = append(sessions, meta)
	}
	return sessions, nil
}

// CleanupInactiveSessions removes sessions that haven't been used for a while
func (f *fileStore) CleanupInactiveSessions(maxAge time.Duration) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()
	var removed []string

	for sessionID, meta := range f.sessions {
		if now.Sub(meta.LastUsedAt) > maxAge {
			// Remove from user mapping if this is their current session
			for userID, userSessionID := range f.userSessions {
				if userSessionID == sessionID {
					delete(f.userSessions, userID)
					break
				}
			}
			// Remove session metadata
			delete(f.sessions, sessionID)
			removed = append(removed, sessionID)
		}
	}

	if len(removed) > 0 {
		f.markDirty()
		if err := f.saveLocked(); err != nil {
			return removed, err
		}
	}

	return removed, nil
}

// StoreModel stores model metadata
func (f *fileStore) StoreModel(meta *ModelMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.models[meta.ID] = meta
	f.markDirty()
	return f.saveLocked()
}

// GetModel retrieves model metadata
func (f *fileStore) GetModel(modelID string) (*ModelMeta, bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	meta, exists := f.models[modelID]
	return meta, exists, nil
}

// ListModels returns all model metadata
func (f *fileStore) ListModels() ([]*ModelMeta, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	models := make([]*ModelMeta, 0, len(f.models))
	for _, meta := range f.models {
		models = append(models, meta)
	}
	return models, nil
}

// DeleteModel removes model metadata
func (f *fileStore) DeleteModel(modelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.models, modelID)
	f.markDirty()
	return f.saveLocked()
}

// StoreUserLastModel stores the last model used by a user
func (f *fileStore) StoreUserLastModel(userID int64, providerID, modelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.userLastModels[userID] = &modelPreference{
		ProviderID: providerID,
		ModelID:    modelID,
	}
	f.markDirty()
	return f.saveLocked()
}

// GetUserLastModel retrieves the last model used by a user
func (f *fileStore) GetUserLastModel(userID int64) (providerID, modelID string, exists bool, err error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	pref, exists := f.userLastModels[userID]
	if !exists {
		return "", "", false, nil
	}
	return pref.ProviderID, pref.ModelID, true, nil
}

// StoreUserLastSession stores the last session used by a user
func (f *fileStore) StoreUserLastSession(userID int64, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.userLastSessions[userID] = sessionID
	f.markDirty()
	return f.saveLocked()
}

// GetUserLastSession retrieves the last session used by a user
func (f *fileStore) GetUserLastSession(userID int64) (sessionID string, exists bool, err error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	sessionID, exists = f.userLastSessions[userID]
	return sessionID, exists, nil
}

// Close implements Store interface
func (f *fileStore) Close() error {
	// Save any pending changes
	f.mu.RLock()
	dirty := f.dirty
	f.mu.RUnlock()

	if dirty {
		return f.save()
	}
	return nil
}
