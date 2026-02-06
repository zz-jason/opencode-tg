package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func createTempFile(t *testing.T) string {
	tmpDir := t.TempDir()
	return filepath.Join(tmpDir, "test-storage.json")
}

func TestNewFileStore(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	if store == nil {
		t.Fatal("NewFileStore should return non-nil store")
	}
	defer store.Close()

	// File may not be created until first write
	// Store something to ensure file is created
	err = store.StoreUserSession(1, "test-session")
	if err != nil {
		t.Fatalf("StoreUserSession failed: %v", err)
	}
	// Now file should exist
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("Storage file should be created at %s", path)
	}
}

func TestNewFileStore_MigratesLegacySessionsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	legacyPath := filepath.Join(tmpDir, "sessions.json")
	newPath := filepath.Join(tmpDir, "bot-state.json")

	legacyStore, err := NewFileStore(legacyPath)
	if err != nil {
		t.Fatalf("failed to create legacy store: %v", err)
	}
	if err := legacyStore.StoreUserSession(7, "session-legacy"); err != nil {
		t.Fatalf("failed to seed legacy store: %v", err)
	}
	legacyStore.Close()

	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("expected legacy file to exist before migration: %v", err)
	}

	store, err := NewFileStore(newPath)
	if err != nil {
		t.Fatalf("failed to create store on new path: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected migrated file to exist at new path: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy file to be moved, stat error=%v", err)
	}

	sessionID, exists, err := store.GetUserSession(7)
	if err != nil {
		t.Fatalf("failed to read migrated session: %v", err)
	}
	if !exists || sessionID != "session-legacy" {
		t.Fatalf("unexpected migrated data: exists=%v sessionID=%q", exists, sessionID)
	}
}

func TestNewFileStore_LoadExisting(t *testing.T) {
	path := createTempFile(t)

	// Create initial store and add data
	store1, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	meta := &SessionMeta{
		SessionID: "session-123",
		UserID:    12345,
		Name:      "Test Session",
	}
	err = store1.StoreSessionMeta(meta)
	if err != nil {
		t.Fatalf("StoreSessionMeta failed: %v", err)
	}
	err = store1.StoreUserSession(12345, "session-123")
	if err != nil {
		t.Fatalf("StoreUserSession failed: %v", err)
	}
	store1.Close()

	// Create new store that loads existing data
	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed on reload: %v", err)
	}
	defer store2.Close()

	// Verify data persisted
	sessionID, exists, err := store2.GetUserSession(12345)
	if err != nil {
		t.Fatalf("GetUserSession failed: %v", err)
	}
	if !exists {
		t.Error("User session should exist after reload")
	}
	if sessionID != "session-123" {
		t.Errorf("Expected session ID 'session-123', got %s", sessionID)
	}

	meta2, exists, err := store2.GetSessionMeta("session-123")
	if err != nil {
		t.Fatalf("GetSessionMeta failed: %v", err)
	}
	if !exists {
		t.Error("Session meta should exist after reload")
	}
	if meta2.Name != "Test Session" {
		t.Errorf("Expected session name 'Test Session', got %s", meta2.Name)
	}
}

func TestFileStore_StoreUserSession(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	defer store.Close()

	userID := int64(12345)
	sessionID := "session-123"

	err = store.StoreUserSession(userID, sessionID)
	if err != nil {
		t.Fatalf("StoreUserSession failed: %v", err)
	}

	// Retrieve and verify
	retrievedID, exists, err := store.GetUserSession(userID)
	if err != nil {
		t.Fatalf("GetUserSession failed: %v", err)
	}
	if !exists {
		t.Error("GetUserSession should return exists = true")
	}
	if retrievedID != sessionID {
		t.Errorf("Expected session ID %s, got %s", sessionID, retrievedID)
	}
}

func TestFileStore_GetUserSession_NonExistent(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	defer store.Close()

	userID := int64(99999)

	sessionID, exists, err := store.GetUserSession(userID)
	if err != nil {
		t.Fatalf("GetUserSession failed: %v", err)
	}
	if exists {
		t.Error("GetUserSession should return exists = false for non-existent user")
	}
	if sessionID != "" {
		t.Errorf("Expected empty session ID, got %s", sessionID)
	}
}

func TestFileStore_DeleteUserSession(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	defer store.Close()

	userID := int64(12345)
	sessionID := "session-123"

	// Store then delete
	err = store.StoreUserSession(userID, sessionID)
	if err != nil {
		t.Fatalf("StoreUserSession failed: %v", err)
	}

	err = store.DeleteUserSession(userID)
	if err != nil {
		t.Fatalf("DeleteUserSession failed: %v", err)
	}

	// Verify deleted
	_, exists, err := store.GetUserSession(userID)
	if err != nil {
		t.Fatalf("GetUserSession failed: %v", err)
	}
	if exists {
		t.Error("User session should be deleted")
	}
}

func TestFileStore_StoreSessionMeta(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	defer store.Close()

	meta := &SessionMeta{
		SessionID:    "session-123",
		UserID:       12345,
		Name:         "Test Session",
		CreatedAt:    time.Now(),
		LastUsedAt:   time.Now(),
		MessageCount: 5,
		ProviderID:   "provider1",
		ModelID:      "model1",
		Status:       "owned",
	}

	err = store.StoreSessionMeta(meta)
	if err != nil {
		t.Fatalf("StoreSessionMeta failed: %v", err)
	}

	// Retrieve and verify
	retrievedMeta, exists, err := store.GetSessionMeta(meta.SessionID)
	if err != nil {
		t.Fatalf("GetSessionMeta failed: %v", err)
	}
	if !exists {
		t.Error("GetSessionMeta should return exists = true")
	}
	if retrievedMeta.SessionID != meta.SessionID {
		t.Errorf("SessionID mismatch: expected %s, got %s", meta.SessionID, retrievedMeta.SessionID)
	}
	if retrievedMeta.UserID != meta.UserID {
		t.Errorf("UserID mismatch: expected %d, got %d", meta.UserID, retrievedMeta.UserID)
	}
	if retrievedMeta.Name != meta.Name {
		t.Errorf("Name mismatch: expected %s, got %s", meta.Name, retrievedMeta.Name)
	}
}

func TestFileStore_GetSessionMeta_NonExistent(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	defer store.Close()

	meta, exists, err := store.GetSessionMeta("non-existent")
	if err != nil {
		t.Fatalf("GetSessionMeta failed: %v", err)
	}
	if exists {
		t.Error("GetSessionMeta should return exists = false for non-existent session")
	}
	if meta != nil {
		t.Errorf("Expected nil meta, got %v", meta)
	}
}

func TestFileStore_DeleteSessionMeta(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	defer store.Close()

	meta := &SessionMeta{
		SessionID: "session-123",
		UserID:    12345,
	}

	err = store.StoreSessionMeta(meta)
	if err != nil {
		t.Fatalf("StoreSessionMeta failed: %v", err)
	}

	err = store.DeleteSessionMeta(meta.SessionID)
	if err != nil {
		t.Fatalf("DeleteSessionMeta failed: %v", err)
	}

	// Verify deleted
	_, exists, err := store.GetSessionMeta(meta.SessionID)
	if err != nil {
		t.Fatalf("GetSessionMeta failed: %v", err)
	}
	if exists {
		t.Error("Session meta should be deleted")
	}
}

func TestFileStore_ListSessions(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	defer store.Close()

	// Initially empty
	sessions, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("Expected 0 sessions initially, got %d", len(sessions))
	}

	// Add two sessions
	meta1 := &SessionMeta{SessionID: "session-1", UserID: 1}
	meta2 := &SessionMeta{SessionID: "session-2", UserID: 2}
	store.StoreSessionMeta(meta1)
	store.StoreSessionMeta(meta2)

	sessions, err = store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("Expected 2 sessions, got %d", len(sessions))
	}
}

func TestFileStore_CleanupInactiveSessions(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	defer store.Close()

	// Create a session with recent last used time
	meta1 := &SessionMeta{
		SessionID:  "session-recent",
		UserID:     1,
		LastUsedAt: time.Now(),
	}
	// Create a session with old last used time
	meta2 := &SessionMeta{
		SessionID:  "session-old",
		UserID:     2,
		LastUsedAt: time.Now().Add(-2 * time.Hour),
	}
	store.StoreSessionMeta(meta1)
	store.StoreSessionMeta(meta2)

	// Cleanup with 1 hour max age
	removed, err := store.CleanupInactiveSessions(1 * time.Hour)
	if err != nil {
		t.Fatalf("CleanupInactiveSessions failed: %v", err)
	}
	if len(removed) != 1 {
		t.Errorf("Expected 1 removed session, got %d", len(removed))
	}
	if len(removed) > 0 && removed[0] != "session-old" {
		t.Errorf("Expected removed session 'session-old', got %s", removed[0])
	}

	// Verify remaining sessions
	sessions, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("Expected 1 remaining session, got %d", len(sessions))
	}
	if len(sessions) > 0 && sessions[0].SessionID != "session-recent" {
		t.Errorf("Expected remaining session 'session-recent', got %s", sessions[0].SessionID)
	}
}

func TestFileStore_Close(t *testing.T) {
	path := createTempFile(t)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	err = store.Close()
	if err != nil {
		t.Errorf("Close should not return error for file store, got %v", err)
	}
	// Closing twice should be safe
	err = store.Close()
	if err != nil {
		t.Errorf("Second Close should be safe, got %v", err)
	}
}
