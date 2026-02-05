package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// fileStore implements Store interface using JSON file storage
type fileStore struct {
	mu sync.RWMutex

	// file path for storage
	filePath string

	// in-memory data
	userSessions map[int64]string
	sessions     map[string]*SessionMeta

	// dirty flag to track changes
	dirty bool
}

// NewFileStore creates a new file-based store
func NewFileStore(filePath string) (Store, error) {
	store := &fileStore{
		filePath:     filePath,
		userSessions: make(map[int64]string),
		sessions:     make(map[string]*SessionMeta),
		dirty:        false,
	}

	// Try to load existing data
	if err := store.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load storage file: %w", err)
	}

	return store, nil
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
		UserSessions map[int64]string        `json:"user_sessions"`
		Sessions     map[string]*SessionMeta `json:"sessions"`
	}

	if err := json.Unmarshal(data, &storedData); err != nil {
		return fmt.Errorf("failed to unmarshal storage data: %w", err)
	}

	f.userSessions = storedData.UserSessions
	f.sessions = storedData.Sessions
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
		UserSessions map[int64]string        `json:"user_sessions"`
		Sessions     map[string]*SessionMeta `json:"sessions"`
	}{
		UserSessions: f.userSessions,
		Sessions:     f.sessions,
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

	delete(f.sessions, sessionID)
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
