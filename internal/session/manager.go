package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"tg-bot/internal/opencode"
)

// Manager handles session management for Telegram users
type Manager struct {
	mu sync.RWMutex
	// userID -> sessionID mapping
	userSessions map[int64]string
	// sessionID -> session metadata
	sessions map[string]*SessionMeta
	// OpenCode client
	client *opencode.Client
}

// SessionMeta contains metadata about a session
type SessionMeta struct {
	SessionID    string
	UserID       int64
	Name         string
	CreatedAt    time.Time
	LastUsedAt   time.Time
	MessageCount int
	ProviderID   string
	ModelID      string
	Status       string // "owned", "orphaned", "other"
}

// NewManager creates a new session manager
func NewManager(client *opencode.Client) *Manager {
	return &Manager{
		userSessions: make(map[int64]string),
		sessions:     make(map[string]*SessionMeta),
		client:       client,
	}
}

// GetOrCreateSession gets the current session for a user, or creates a new one
func (m *Manager) GetOrCreateSession(ctx context.Context, userID int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if user has an existing session
	if sessionID, exists := m.userSessions[userID]; exists {
		// Update last used time
		if meta, exists := m.sessions[sessionID]; exists {
			meta.LastUsedAt = time.Now()
			meta.MessageCount++
		}
		return sessionID, nil
	}

	// First, check if user has existing sessions in OpenCode
	opencodeSessions, err := m.client.ListSessions(ctx)
	if err != nil {
		log.Warnf("Failed to get sessions from OpenCode: %v, creating new session", err)
	} else {
		// Look for sessions belonging to this user
		var userSessions []opencode.Session
		for _, ocSession := range opencodeSessions {
			if ocSession.Metadata != nil {
				if tgUserID, ok := ocSession.Metadata["telegram_user_id"].(float64); ok && int64(tgUserID) == userID {
					userSessions = append(userSessions, ocSession)
				}
			}
		}

		if len(userSessions) > 0 {
			// Use the most recent session (assuming later in list is newer)
			// Could sort by creation time if needed
			ocSession := userSessions[0]

			// Check if we already have metadata for this session
			meta, exists := m.sessions[ocSession.ID]
			if !exists {
				// Create new metadata
				meta = &SessionMeta{
					SessionID:  ocSession.ID,
					UserID:     userID,
					Status:     "owned",
					CreatedAt:  time.Now(),
					LastUsedAt: time.Now(),
				}

				// Extract name from OpenCode session title, metadata, or use default
				if ocSession.Title != "" {
					meta.Name = ocSession.Title
				} else if ocSession.Metadata != nil {
					if sessionName, ok := ocSession.Metadata["session_name"].(string); ok && sessionName != "" {
						meta.Name = sessionName
					} else if metadataTitle, ok := ocSession.Metadata["title"].(string); ok && metadataTitle != "" {
						meta.Name = metadataTitle
					}
				}
				if meta.Name == "" {
					meta.Name = "Telegram Session"
				}

				// Extract model information if available
				if ocSession.Metadata != nil {
					if providerID, ok := ocSession.Metadata["provider_id"].(string); ok {
						meta.ProviderID = providerID
					}
					if modelID, ok := ocSession.Metadata["model_id"].(string); ok {
						meta.ModelID = modelID
					}
				}

				m.sessions[ocSession.ID] = meta
			} else {
				// Update existing metadata
				meta.LastUsedAt = time.Now()
				meta.UserID = userID  // Ensure user ID is correct
				meta.Status = "owned" // Ensure status is correct
			}

			meta.MessageCount++

			// Store user mapping
			m.userSessions[userID] = ocSession.ID

			log.Infof("Using existing OpenCode session %s for user %d", ocSession.ID, userID)
			return ocSession.ID, nil
		}
	}

	// No existing sessions found, create new session in OpenCode
	log.Infof("Creating new OpenCode session for user %d", userID)
	session, err := m.client.CreateSession(ctx, &opencode.CreateSessionRequest{
		Title: "Telegram User",
		Metadata: map[string]interface{}{
			"telegram_user_id": userID,
			"created_via":      "telegram_bot",
		},
	})
	if err != nil {
		return "", err
	}

	// Create session metadata
	meta := &SessionMeta{
		SessionID:    session.ID,
		UserID:       userID,
		Name:         "Telegram Session",
		CreatedAt:    time.Now(),
		LastUsedAt:   time.Now(),
		MessageCount: 1,
	}

	// Store mappings
	m.userSessions[userID] = session.ID
	m.sessions[session.ID] = meta

	log.Infof("Created new session %s for user %d", session.ID, userID)
	return session.ID, nil
}

// GetUserSession gets the current session ID for a user
func (m *Manager) GetUserSession(userID int64) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessionID, exists := m.userSessions[userID]
	return sessionID, exists
}

// SetUserSession sets a specific session for a user
func (m *Manager) SetUserSession(userID int64, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Update user session mapping
	m.userSessions[userID] = sessionID

	// Update or create metadata
	if meta, exists := m.sessions[sessionID]; exists {
		meta.LastUsedAt = time.Now()
		meta.UserID = userID
		meta.Status = "owned"
	} else {
		m.sessions[sessionID] = &SessionMeta{
			SessionID:    sessionID,
			UserID:       userID,
			Status:       "owned",
			CreatedAt:    time.Now(),
			LastUsedAt:   time.Now(),
			MessageCount: 0,
		}
	}

	log.Infof("User %d switched to session %s", userID, sessionID)
	return nil
}

// ListUserSessions lists all sessions from OpenCode, categorized by ownership
func (m *Manager) ListUserSessions(ctx context.Context, userID int64) ([]*SessionMeta, error) {
	// Get all sessions from OpenCode
	opencodeSessions, err := m.client.ListSessions(ctx)
	if err != nil {
		// Fall back to local sessions if OpenCode API fails
		log.Warnf("Failed to get sessions from OpenCode: %v, falling back to local sessions", err)
		return m.listLocalUserSessions(userID), nil
	}

	var allSessions []*SessionMeta

	// Process all OpenCode sessions
	for _, ocSession := range opencodeSessions {
		meta := m.getOrCreateSessionMeta(ocSession.ID, userID, ocSession.Metadata, ocSession.Title)
		allSessions = append(allSessions, meta)
	}

	// Also include any local sessions that might not be in OpenCode (for backward compatibility)
	localSessions := m.listLocalUserSessions(userID)
	for _, local := range localSessions {
		// Check if already included from OpenCode
		found := false
		for _, opencodeSession := range allSessions {
			if opencodeSession.SessionID == local.SessionID {
				found = true
				break
			}
		}
		if !found {
			// Ensure local session has correct status
			if local.UserID == userID {
				local.Status = "owned"
			} else if local.UserID == 0 {
				local.Status = "orphaned"
			} else {
				local.Status = "other"
			}
			allSessions = append(allSessions, local)
		}
	}

	return allSessions, nil
}

// CreateNewSession creates a new session for a user
func (m *Manager) CreateNewSession(ctx context.Context, userID int64, name string) (string, error) {
	return m.CreateNewSessionWithModel(ctx, userID, name, "", "")
}

// CreateNewSessionWithModel creates a new session for a user with specific model
func (m *Manager) CreateNewSessionWithModel(ctx context.Context, userID int64, name, providerID, modelID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Create new session in OpenCode
	session, err := m.client.CreateSession(ctx, &opencode.CreateSessionRequest{
		Title: name,
		Metadata: map[string]interface{}{
			"telegram_user_id": userID,
			"created_via":      "telegram_bot",
			"session_name":     name,
			"provider_id":      providerID,
			"model_id":         modelID,
		},
	})
	if err != nil {
		return "", err
	}

	// Create session metadata
	meta := &SessionMeta{
		SessionID:    session.ID,
		UserID:       userID,
		Name:         name,
		Status:       "owned",
		CreatedAt:    time.Now(),
		LastUsedAt:   time.Now(),
		MessageCount: 0,
		ProviderID:   providerID,
		ModelID:      modelID,
	}

	// Store metadata
	m.sessions[session.ID] = meta

	// Initialize session with the selected model if specified
	if providerID != "" && modelID != "" {
		if err := m.client.InitSessionWithModel(ctx, session.ID, providerID, modelID); err != nil {
			log.Warnf("Failed to initialize session with model %s/%s: %v", providerID, modelID, err)
			// Continue anyway, session will use default model
		}
	}

	log.Infof("Created new named session %s (%s) with model %s/%s for user %d", session.ID, name, providerID, modelID, userID)
	return session.ID, nil
}

// SetSessionModel sets or changes the model for an existing session
func (m *Manager) SetSessionModel(ctx context.Context, sessionID, providerID, modelID string) error {
	log.Debugf("SetSessionModel: START - acquiring lock for session %s with model %s/%s", sessionID, providerID, modelID)
	startTime := time.Now()
	m.mu.Lock()
	lockTime := time.Since(startTime)
	log.Debugf("SetSessionModel: lock acquired for session %s after %v", sessionID, lockTime)
	defer func() {
		m.mu.Unlock()
		totalTime := time.Since(startTime)
		log.Debugf("SetSessionModel: lock released for session %s, total function time: %v", sessionID, totalTime)
	}()

	meta, exists := m.sessions[sessionID]
	if !exists {
		log.Errorf("SetSessionModel: session %s not found in session map", sessionID)
		return fmt.Errorf("session not found: %s", sessionID)
	}
	log.Debugf("SetSessionModel: found metadata for session %s (current model: %s/%s, user: %d)", sessionID, meta.ProviderID, meta.ModelID, meta.UserID)

	// Update metadata
	meta.ProviderID = providerID
	meta.ModelID = modelID
	meta.LastUsedAt = time.Now()
	log.Debugf("SetSessionModel: updated metadata for session %s to %s/%s", sessionID, providerID, modelID)

	// Initialize session with the selected model
	if providerID != "" && modelID != "" {
		log.Debugf("SetSessionModel: calling InitSessionWithModel for session %s with %s/%s", sessionID, providerID, modelID)
		initStart := time.Now()
		if err := m.client.InitSessionWithModel(ctx, sessionID, providerID, modelID); err != nil {
			initTime := time.Since(initStart)
			log.Warnf("Failed to initialize session with model %s/%s after %v: %v", providerID, modelID, initTime, err)
			// Continue anyway, session will use default model
			// Return a wrapped error that handlers can check if needed
			return fmt.Errorf("model metadata updated but initialization failed: %w", err)
		}
		initTime := time.Since(initStart)
		log.Debugf("SetSessionModel: successfully initialized session %s with model %s/%s after %v", sessionID, providerID, modelID, initTime)
	}

	totalTime := time.Since(startTime)
	log.Infof("Updated session %s model to %s/%s in %v", sessionID, providerID, modelID, totalTime)
	return nil
}

// RenameSession renames a session (only allowed for owned sessions)
func (m *Manager) RenameSession(ctx context.Context, sessionID string, newName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, exists := m.sessions[sessionID]
	if !exists {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Check if session belongs to current user (owned)
	if meta.Status != "owned" {
		return fmt.Errorf("cannot rename session: session status is %s (must be owned)", meta.Status)
	}

	// Rename in OpenCode
	if err := m.client.RenameSession(ctx, sessionID, newName); err != nil {
		return fmt.Errorf("failed to rename session in OpenCode: %w", err)
	}

	// Update local metadata
	meta.Name = newName
	meta.LastUsedAt = time.Now()

	log.Infof("Renamed session %s to '%s'", sessionID, newName)
	return nil
}

// DeleteSession deletes a session (allowed for owned or orphaned sessions)
func (m *Manager) DeleteSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, exists := m.sessions[sessionID]
	if !exists {
		// Session not in local cache, still try to delete from OpenCode
		if err := m.client.DeleteSession(ctx, sessionID); err != nil {
			return fmt.Errorf("failed to delete session from OpenCode: %w", err)
		}
		log.Infof("Deleted session %s (not in local cache)", sessionID)
		return nil
	}

	// Check if session can be deleted (owned or orphaned, not other)
	if meta.Status == "other" {
		return fmt.Errorf("cannot delete session: session belongs to another user")
	}

	// Delete from OpenCode
	if err := m.client.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("failed to delete session from OpenCode: %w", err)
	}

	// Remove from user mapping if this is their current session
	for userID, userSessionID := range m.userSessions {
		if userSessionID == sessionID {
			delete(m.userSessions, userID)
			break
		}
	}

	// Remove from local cache
	delete(m.sessions, sessionID)

	log.Infof("Deleted session %s ('%s')", sessionID, meta.Name)
	return nil
}

// GetSessionMeta gets metadata for a session
func (m *Manager) GetSessionMeta(sessionID string) (*SessionMeta, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	meta, exists := m.sessions[sessionID]
	return meta, exists
}

// CleanupInactiveSessions removes sessions that haven't been used for a while
func (m *Manager) CleanupInactiveSessions(maxAge time.Duration) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	var removed []string

	for sessionID, meta := range m.sessions {
		if now.Sub(meta.LastUsedAt) > maxAge {
			// Remove from user mapping if this is their current session
			for userID, userSessionID := range m.userSessions {
				if userSessionID == sessionID {
					delete(m.userSessions, userID)
					break
				}
			}
			// Remove session metadata
			delete(m.sessions, sessionID)
			removed = append(removed, sessionID)
		}
	}

	if len(removed) > 0 {
		log.Infof("Cleaned up %d inactive sessions", len(removed))
	}
	return removed
}

// GetSessionCount returns the total number of sessions
func (m *Manager) GetSessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// listLocalUserSessions lists sessions from local storage only
func (m *Manager) listLocalUserSessions(userID int64) []*SessionMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var userSessions []*SessionMeta
	for _, meta := range m.sessions {
		if meta.UserID == userID {
			userSessions = append(userSessions, meta)
		}
	}
	return userSessions
}

// getOrCreateSessionMeta gets or creates session metadata for an OpenCode session
func (m *Manager) getOrCreateSessionMeta(sessionID string, currentUserID int64, metadata map[string]interface{}, title string) *SessionMeta {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if we already have metadata for this session
	if meta, exists := m.sessions[sessionID]; exists {
		// Update last used time and recalculate status based on current user
		meta.LastUsedAt = time.Now()
		// Recalculate status based on owner ID and current user
		if meta.UserID == 0 {
			meta.Status = "orphaned"
		} else if meta.UserID == currentUserID {
			meta.Status = "owned"
		} else {
			meta.Status = "other"
		}
		return meta
	}

	// Determine owner ID from metadata
	ownerID := int64(0) // 0 means orphaned
	if metadata != nil {
		if tgUserID, ok := metadata["telegram_user_id"].(float64); ok {
			ownerID = int64(tgUserID)
		}
	}

	// Create new metadata
	meta := &SessionMeta{
		SessionID:  sessionID,
		UserID:     ownerID,
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
	}

	// Extract name from provided title, metadata, or use default
	if title != "" {
		meta.Name = title
	} else if sessionName, ok := metadata["session_name"].(string); ok && sessionName != "" {
		meta.Name = sessionName
	} else if metadataTitle, ok := metadata["title"].(string); ok && metadataTitle != "" {
		meta.Name = metadataTitle
	} else {
		meta.Name = "Telegram Session"
	}

	// Extract model information if available
	if providerID, ok := metadata["provider_id"].(string); ok {
		meta.ProviderID = providerID
	}
	if modelID, ok := metadata["model_id"].(string); ok {
		meta.ModelID = modelID
	}

	// Determine session status
	if ownerID == 0 {
		meta.Status = "orphaned"
	} else if ownerID == currentUserID {
		meta.Status = "owned"
	} else {
		meta.Status = "other"
	}

	// Store in local cache
	m.sessions[sessionID] = meta

	// Update user session mapping only if this session belongs to the current user
	if meta.Status == "owned" {
		currentSessionID, exists := m.userSessions[currentUserID]
		if !exists || currentSessionID != sessionID {
			m.userSessions[currentUserID] = sessionID
		}
	}

	return meta
}
