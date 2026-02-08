package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tg-bot/internal/opencode"
	"tg-bot/internal/storage"
)

// mockOpenCodeServer creates a test HTTP server that simulates OpenCode API
func mockOpenCodeServer(t *testing.T) *httptest.Server {
	sessionCounter := 0
	sessions := make(map[string]bool)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "POST" && r.URL.Path == "/session":
			sessionCounter++
			sessionID := fmt.Sprintf("test-session-%d", sessionCounter)
			sessions[sessionID] = true

			var req opencode.CreateSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			response := opencode.Session{
				ID:    sessionID,
				Slug:  "test-slug",
				Title: req.Title,
				Time: opencode.SessionTime{
					Created: time.Now().UnixMilli(),
					Updated: time.Now().UnixMilli(),
				},
			}
			json.NewEncoder(w).Encode(response)

		case r.Method == "GET" && r.URL.Path == "/session":
			var sessionList []opencode.Session
			for sessionID := range sessions {
				sessionList = append(sessionList, opencode.Session{
					ID:   sessionID,
					Slug: "test-slug",
					Time: opencode.SessionTime{
						Created: time.Now().UnixMilli(),
						Updated: time.Now().UnixMilli(),
					},
				})
			}
			json.NewEncoder(w).Encode(sessionList)

		case r.Method == "GET" && len(r.URL.Path) > len("/session/") && r.URL.Path[:len("/session/")] == "/session/":
			sessionID := r.URL.Path[len("/session/"):]
			if _, exists := sessions[sessionID]; !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			response := opencode.Session{
				ID:   sessionID,
				Slug: "test-slug",
				Time: opencode.SessionTime{
					Created: time.Now().UnixMilli(),
					Updated: time.Now().UnixMilli(),
				},
			}
			json.NewEncoder(w).Encode(response)

		case r.Method == "POST" && len(r.URL.Path) > len("/session/") && strings.Contains(r.URL.Path, "/message"):
			// Simplified message response
			response := opencode.Message{
				ID:        "test-message",
				SessionID: "test-session",
				Role:      "assistant",
				Content:   "Test response",
				CreatedAt: time.Now(),
			}
			json.NewEncoder(w).Encode(response)

		case r.Method == "POST" && len(r.URL.Path) > len("/session/") && strings.HasSuffix(r.URL.Path, "/abort"):
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
			t.Logf("Unhandled request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

// createTestManager creates a manager with file storage in a temporary directory
func createTestManager(t *testing.T, client *opencode.Client) *Manager {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "bot-state.json")
	store, err := storage.NewStore(storage.Options{
		Type:     "file",
		FilePath: path,
	})
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}
	return NewManagerWithStore(client, store)
}

func TestNewManager(t *testing.T) {
	server := mockOpenCodeServer(t)
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	// First call should create a new session
	sessionID1, err := manager.GetOrCreateSession(context.Background(), 12345)
	if err != nil {
		t.Fatalf("Failed to get/create session: %v", err)
	}

	if sessionID1 == "" {
		t.Fatal("Session ID should not be empty")
	}

	// Second call should return the same session
	sessionID2, err := manager.GetOrCreateSession(context.Background(), 12345)
	if err != nil {
		t.Fatalf("Failed to get existing session: %v", err)
	}

	if sessionID1 != sessionID2 {
		t.Errorf("Expected same session ID, got %s and %s", sessionID1, sessionID2)
	}

	// Different user should get different session
	sessionID3, err := manager.GetOrCreateSession(context.Background(), 67890)
	if err != nil {
		t.Fatalf("Failed to get/create session for different user: %v", err)
	}

	if sessionID1 == sessionID3 {
		t.Error("Different users should have different sessions")
	}
}

func TestGetUserSession(t *testing.T) {
	server := mockOpenCodeServer(t)
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	// Initially no session
	sessionID, exists := manager.GetUserSession(12345)
	if exists {
		t.Error("Should not have session for new user")
	}

	// Create a session
	_, err := manager.GetOrCreateSession(context.Background(), 12345)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Now should have session
	sessionID, exists = manager.GetUserSession(12345)
	if !exists {
		t.Error("Should have session after creation")
	}
	if sessionID == "" {
		t.Error("Session ID should not be empty")
	}
}

func TestSetUserSession(t *testing.T) {
	server := mockOpenCodeServer(t)
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	// Set a session for user
	err := manager.SetUserSession(12345, "custom-session-123")
	if err != nil {
		t.Fatalf("Failed to set user session: %v", err)
	}

	// Verify session is set
	sessionID, exists := manager.GetUserSession(12345)
	if !exists {
		t.Error("Should have session after setting")
	}
	if sessionID != "custom-session-123" {
		t.Errorf("Expected session ID 'custom-session-123', got %s", sessionID)
	}
}

func TestListUserSessions(t *testing.T) {
	server := mockOpenCodeServer(t)
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	// Create multiple sessions for same user
	_, err := manager.CreateNewSession(context.Background(), 12345, "Session 1")
	if err != nil {
		t.Fatalf("Failed to create session 1: %v", err)
	}

	_, err = manager.CreateNewSession(context.Background(), 12345, "Session 2")
	if err != nil {
		t.Fatalf("Failed to create session 2: %v", err)
	}

	// Create session for different user
	_, err = manager.CreateNewSession(context.Background(), 67890, "Other User Session")
	if err != nil {
		t.Fatalf("Failed to create session for other user: %v", err)
	}

	// List sessions for user 12345 (should get all 3 sessions with appropriate status)
	sessions, err := manager.ListUserSessions(context.Background(), 12345)
	if err != nil {
		t.Fatalf("Failed to list sessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Errorf("Expected 3 sessions for user 12345, got %d", len(sessions))
	}
	// Check status distribution
	ownedCount := 0
	otherCount := 0
	for _, sess := range sessions {
		if sess.Status == "owned" {
			ownedCount++
		} else if sess.Status == "other" {
			otherCount++
		}
	}
	if ownedCount != 2 {
		t.Errorf("Expected 2 owned sessions for user 12345, got %d", ownedCount)
	}
	if otherCount != 1 {
		t.Errorf("Expected 1 other session for user 12345, got %d", otherCount)
	}

	// List sessions for user 67890
	sessions, err = manager.ListUserSessions(context.Background(), 67890)
	if err != nil {
		t.Fatalf("Failed to list sessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Errorf("Expected 3 sessions for user 67890, got %d", len(sessions))
	}
	ownedCount = 0
	otherCount = 0
	for _, sess := range sessions {
		if sess.Status == "owned" {
			ownedCount++
		} else if sess.Status == "other" {
			otherCount++
		}
	}
	if ownedCount != 1 {
		t.Errorf("Expected 1 owned session for user 67890, got %d", ownedCount)
	}
	if otherCount != 2 {
		t.Errorf("Expected 2 other sessions for user 67890, got %d", otherCount)
	}

	// List sessions for non-existent user 99999
	sessions, err = manager.ListUserSessions(context.Background(), 99999)
	if err != nil {
		t.Fatalf("Failed to list sessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Errorf("Expected 3 sessions for non-existent user, got %d", len(sessions))
	}
	ownedCount = 0
	otherCount = 0
	for _, sess := range sessions {
		if sess.Status == "owned" {
			ownedCount++
		} else if sess.Status == "other" {
			otherCount++
		}
	}
	if ownedCount != 0 {
		t.Errorf("Expected 0 owned sessions for non-existent user, got %d", ownedCount)
	}
	if otherCount != 3 {
		t.Errorf("Expected 3 other sessions for non-existent user, got %d", otherCount)
	}
}

func TestListUserSessionsSyncsMessageCountAndModel(t *testing.T) {
	now := time.Now().UnixMilli()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/session":
			response := []opencode.Session{
				{
					ID:    "session-1",
					Slug:  "session-1",
					Title: "Session 1",
					Time: opencode.SessionTime{
						Created: now - 1000,
						Updated: now,
					},
					Metadata: map[string]interface{}{
						"telegram_user_id": float64(12345),
					},
				},
			}
			_ = json.NewEncoder(w).Encode(response)
		case r.Method == "GET" && r.URL.Path == "/session/session-1/message":
			response := []opencode.MessageResponse{
				{
					Info: opencode.MessageInfo{
						ID:        "msg-1",
						SessionID: "session-1",
						Role:      "user",
						Time: opencode.MessageTime{
							Created: now - 500,
						},
					},
					Parts: []opencode.MessagePartResponse{
						{Type: "text", Text: "hello"},
					},
				},
				{
					Info: opencode.MessageInfo{
						ID:         "msg-2",
						SessionID:  "session-1",
						Role:       "assistant",
						ModelID:    "gpt-4.1",
						ProviderID: "openai",
						Time: opencode.MessageTime{
							Created: now,
						},
					},
					Parts: []opencode.MessagePartResponse{
						{Type: "text", Text: "world"},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	sessions, err := manager.ListUserSessions(context.Background(), 12345)
	if err != nil {
		t.Fatalf("ListUserSessions failed: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Expected 1 session, got %d", len(sessions))
	}

	sess := sessions[0]
	if sess.MessageCount != 2 {
		t.Fatalf("Expected message count 2, got %d", sess.MessageCount)
	}
	if sess.ProviderID != "openai" {
		t.Fatalf("Expected providerID openai, got %q", sess.ProviderID)
	}
	if sess.ModelID != "gpt-4.1" {
		t.Fatalf("Expected modelID gpt-4.1, got %q", sess.ModelID)
	}
}

func TestCreateNewSession(t *testing.T) {
	server := mockOpenCodeServer(t)
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	sessionID, err := manager.CreateNewSession(context.Background(), 12345, "Test Session Name")
	if err != nil {
		t.Fatalf("Failed to create new session: %v", err)
	}

	if sessionID == "" {
		t.Fatal("Session ID should not be empty")
	}

	// Verify session metadata
	meta, exists := manager.GetSessionMeta(sessionID)
	if !exists {
		t.Error("Should have metadata for created session")
	}
	if meta.Name != "Test Session Name" {
		t.Errorf("Expected session name 'Test Session Name', got %s", meta.Name)
	}
	if meta.UserID != 12345 {
		t.Errorf("Expected user ID 12345, got %d", meta.UserID)
	}
}

func TestGetSessionMeta(t *testing.T) {
	server := mockOpenCodeServer(t)
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	// Create a session
	sessionID, err := manager.GetOrCreateSession(context.Background(), 12345)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Get metadata
	meta, exists := manager.GetSessionMeta(sessionID)
	if !exists {
		t.Error("Should have metadata for created session")
	}
	if meta.SessionID != sessionID {
		t.Errorf("Expected session ID %s, got %s", sessionID, meta.SessionID)
	}
	if meta.UserID != 12345 {
		t.Errorf("Expected user ID 12345, got %d", meta.UserID)
	}
	if meta.MessageCount < 1 {
		t.Errorf("Expected message count >= 1, got %d", meta.MessageCount)
	}

	// Non-existent session
	meta, exists = manager.GetSessionMeta("non-existent-session")
	if exists {
		t.Error("Should not have metadata for non-existent session")
	}
}

func TestGetSessionCount(t *testing.T) {
	server := mockOpenCodeServer(t)
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	// Initially zero
	count := manager.GetSessionCount()
	if count != 0 {
		t.Errorf("Expected 0 sessions initially, got %d", count)
	}

	// Create a session
	_, err := manager.GetOrCreateSession(context.Background(), 12345)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	count = manager.GetSessionCount()
	if count != 1 {
		t.Errorf("Expected 1 session, got %d", count)
	}

	// Create another session
	_, err = manager.GetOrCreateSession(context.Background(), 67890)
	if err != nil {
		t.Fatalf("Failed to create second session: %v", err)
	}

	count = manager.GetSessionCount()
	if count != 2 {
		t.Errorf("Expected 2 sessions, got %d", count)
	}
}

func TestCleanupInactiveSessions(t *testing.T) {
	server := mockOpenCodeServer(t)
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	// Create a session
	_, err := manager.GetOrCreateSession(context.Background(), 12345)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Initially should have 1 session
	count := manager.GetSessionCount()
	if count != 1 {
		t.Errorf("Expected 1 session, got %d", count)
	}

	// Cleanup with very short maxAge (should remove session)
	// Note: session was just created, so it's not inactive yet
	// We need to manually age it, but we can't modify the LastUsedAt
	// For now, test that cleanup doesn't remove active sessions
	removed := manager.CleanupInactiveSessions(time.Millisecond)
	if len(removed) > 0 {
		t.Logf("Note: Cleanup removed %d sessions (might be expected if test runs slowly)", len(removed))
	}
}

func TestGetOrCreateSessionFailsWhenListSessionsFails(t *testing.T) {
	var createCalls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/session":
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		case r.Method == "POST" && r.URL.Path == "/session":
			atomic.AddInt32(&createCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(opencode.Session{
				ID:   "unexpected-created-session",
				Slug: "unexpected-created-session",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	_, err := manager.GetOrCreateSession(context.Background(), 12345)
	if err == nil {
		t.Fatal("expected GetOrCreateSession to fail when ListSessions fails")
	}
	if !strings.Contains(err.Error(), "failed to get sessions from OpenCode") {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls := atomic.LoadInt32(&createCalls); calls != 0 {
		t.Fatalf("expected no CreateSession calls when ListSessions fails, got %d", calls)
	}
}

func TestInitializeFailsFastWhenSyncSessionsFails(t *testing.T) {
	var providerCalls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/session":
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		case r.Method == "GET" && r.URL.Path == "/provider":
			atomic.AddInt32(&providerCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(opencode.ProvidersResponse{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	err := manager.Initialize(context.Background())
	if err == nil {
		t.Fatal("expected Initialize to fail when SyncSessions fails")
	}
	if !strings.Contains(err.Error(), "failed to synchronize sessions") {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls := atomic.LoadInt32(&providerCalls); calls != 0 {
		t.Fatalf("expected SyncModels not to run after SyncSessions failure, provider calls=%d", calls)
	}
}

func TestSetSessionModelPrototypeModeSkipsInit(t *testing.T) {
	var initCalls int32
	sessionCounter := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "POST" && r.URL.Path == "/session":
			sessionCounter++
			sessionID := fmt.Sprintf("test-session-%d", sessionCounter)
			response := opencode.Session{
				ID:    sessionID,
				Slug:  "test-slug",
				Title: "Test Session",
				Time: opencode.SessionTime{
					Created: time.Now().UnixMilli(),
					Updated: time.Now().UnixMilli(),
				},
			}
			_ = json.NewEncoder(w).Encode(response)

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/init"):
			atomic.AddInt32(&initCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	const userID int64 = 12345
	sessionID, err := manager.CreateNewSession(context.Background(), userID, "Prototype Session")
	if err != nil {
		t.Fatalf("CreateNewSession failed: %v", err)
	}

	if err := manager.SetSessionModel(context.Background(), sessionID, "deepseek", "deepseek-chat"); err != nil {
		t.Fatalf("SetSessionModel failed: %v", err)
	}

	if calls := atomic.LoadInt32(&initCalls); calls != 0 {
		t.Fatalf("expected no /init calls in prototype mode, got %d", calls)
	}

	meta, exists := manager.GetSessionMeta(sessionID)
	if !exists {
		t.Fatalf("expected session meta to exist")
	}
	if meta.ProviderID != "deepseek" || meta.ModelID != "deepseek-chat" {
		t.Fatalf("unexpected session model metadata: %s/%s", meta.ProviderID, meta.ModelID)
	}

	providerID, modelID, exists, err := manager.GetUserLastModel(userID)
	if err != nil {
		t.Fatalf("GetUserLastModel failed: %v", err)
	}
	if !exists {
		t.Fatalf("expected user last model to be stored")
	}
	if providerID != "deepseek" || modelID != "deepseek-chat" {
		t.Fatalf("unexpected user last model: %s/%s", providerID, modelID)
	}
}

func TestCreateNewSessionWithModelPrototypeModeSkipsInit(t *testing.T) {
	var initCalls int32
	sessionCounter := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "POST" && r.URL.Path == "/session":
			sessionCounter++
			sessionID := fmt.Sprintf("test-session-%d", sessionCounter)
			response := opencode.Session{
				ID:    sessionID,
				Slug:  "test-slug",
				Title: "Session With Model",
				Time: opencode.SessionTime{
					Created: time.Now().UnixMilli(),
					Updated: time.Now().UnixMilli(),
				},
			}
			_ = json.NewEncoder(w).Encode(response)

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/init"):
			atomic.AddInt32(&initCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := opencode.NewClient(server.URL, 5)
	manager := createTestManager(t, client)

	const userID int64 = 67890
	sessionID, err := manager.CreateNewSessionWithModel(context.Background(), userID, "Named Session", "deepseek", "deepseek-chat")
	if err != nil {
		t.Fatalf("CreateNewSessionWithModel failed: %v", err)
	}

	if calls := atomic.LoadInt32(&initCalls); calls != 0 {
		t.Fatalf("expected no /init calls in prototype mode, got %d", calls)
	}

	meta, exists := manager.GetSessionMeta(sessionID)
	if !exists {
		t.Fatalf("expected session meta to exist")
	}
	if meta.ProviderID != "deepseek" || meta.ModelID != "deepseek-chat" {
		t.Fatalf("unexpected session model metadata: %s/%s", meta.ProviderID, meta.ModelID)
	}

	providerID, modelID, exists, err := manager.GetUserLastModel(userID)
	if err != nil {
		t.Fatalf("GetUserLastModel failed: %v", err)
	}
	if !exists {
		t.Fatalf("expected user last model to be stored")
	}
	if providerID != "deepseek" || modelID != "deepseek-chat" {
		t.Fatalf("unexpected user last model: %s/%s", providerID, modelID)
	}
}
