package session

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"tg-bot/internal/opencode"
	"tg-bot/internal/storage"
)

// Manager handles session management for Telegram users
type Manager struct {
	mu sync.RWMutex
	// storage backend
	store storage.Store
	// OpenCode client
	client *opencode.Client
}

// SessionMeta is an alias for storage.SessionMeta
type SessionMeta = storage.SessionMeta

// NewManager creates a new session manager with default file storage
// Deprecated: Use NewManagerWithStore for better control over storage
func NewManager(client *opencode.Client) *Manager {
	store, err := storage.NewStore(storage.Options{
		Type:     "file",
		FilePath: "opencode-tg-state.json",
	})
	if err != nil {
		// Panic since this is a programming error - storage should always be available
		panic(fmt.Sprintf("Failed to create default file storage: %v", err))
	}
	return &Manager{
		store:  store,
		client: client,
	}
}

// NewManagerWithStore creates a new session manager with custom storage
func NewManagerWithStore(client *opencode.Client, store storage.Store) *Manager {
	return &Manager{
		store:  store,
		client: client,
	}
}

// Initialize preloads sessions and models from OpenCode at bot startup
func (m *Manager) Initialize(ctx context.Context) error {
	log.Info("Initializing session manager: synchronizing sessions and models from OpenCode")

	// Synchronize sessions first
	if err := m.SyncSessions(ctx); err != nil {
		return fmt.Errorf("failed to synchronize sessions: %w", err)
	}

	// Synchronize models
	if err := m.SyncModels(ctx); err != nil {
		return fmt.Errorf("failed to synchronize models: %w", err)
	}

	log.Info("Session manager initialization completed")
	return nil
}

// SyncModels synchronizes models from OpenCode to local storage
func (m *Manager) SyncModels(ctx context.Context) error {
	providersResp, err := m.client.GetProviders(ctx)
	if err != nil {
		return fmt.Errorf("failed to get providers from OpenCode: %w", err)
	}

	connectedSet := make(map[string]bool)
	for _, providerID := range providersResp.Connected {
		connectedSet[providerID] = true
	}

	models := make([]opencode.Model, 0, 64)
	for _, provider := range providersResp.All {
		if !connectedSet[provider.ID] {
			continue
		}
		for modelID, model := range provider.Models {
			if strings.TrimSpace(model.ID) == "" {
				model.ID = modelID
			}
			if strings.TrimSpace(model.ProviderID) == "" {
				model.ProviderID = provider.ID
			}
			if strings.TrimSpace(model.Name) == "" {
				model.Name = model.ID
			}
			models = append(models, model)
		}
	}

	sort.Slice(models, func(i, j int) bool {
		leftProvider := strings.ToLower(models[i].ProviderID)
		rightProvider := strings.ToLower(models[j].ProviderID)
		if leftProvider != rightProvider {
			return leftProvider < rightProvider
		}
		leftName := strings.ToLower(models[i].Name)
		rightName := strings.ToLower(models[j].Name)
		if leftName != rightName {
			return leftName < rightName
		}
		return models[i].ID < models[j].ID
	})

	// Get existing models from storage for comparison.
	existingModels, err := m.store.ListModels()
	if err != nil {
		return fmt.Errorf("failed to list existing models: %w", err)
	}

	existingByKey := make(map[string]*storage.ModelMeta, len(existingModels))
	for _, model := range existingModels {
		if model == nil {
			continue
		}
		key := storage.ModelKey(model.ProviderID, model.ID)
		if key == "" {
			continue
		}
		existingByKey[key] = model
	}

	availableByKey := make(map[string]opencode.Model, len(models))
	availableKeys := make([]string, 0, len(models))
	for _, model := range models {
		key := storage.ModelKey(model.ProviderID, model.ID)
		if key == "" {
			continue
		}
		availableByKey[key] = model
		availableKeys = append(availableKeys, key)
	}

	usedNumbers := make(map[int]string, len(models))
	assignedNumbers := make(map[string]int, len(models))

	// First pass: keep existing numbers for currently available model keys.
	for _, key := range availableKeys {
		existing := existingByKey[key]
		if existing == nil || existing.Number <= 0 {
			continue
		}
		if ownerKey, occupied := usedNumbers[existing.Number]; occupied && ownerKey != key {
			continue
		}
		usedNumbers[existing.Number] = key
		assignedNumbers[key] = existing.Number
	}

	// Collect recyclable numbers from models no longer available.
	recyclableNumbers := make([]int, 0, len(existingModels))
	recyclableSet := make(map[int]bool)
	for key, existing := range existingByKey {
		if _, stillAvailable := availableByKey[key]; stillAvailable {
			continue
		}
		if existing.Number <= 0 {
			continue
		}
		if recyclableSet[existing.Number] {
			continue
		}
		recyclableSet[existing.Number] = true
		recyclableNumbers = append(recyclableNumbers, existing.Number)
	}
	sort.Ints(recyclableNumbers)

	nextModelNumber := 1
	for number := range usedNumbers {
		if number >= nextModelNumber {
			nextModelNumber = number + 1
		}
	}

	nextRecyclable := 0
	allocateNumber := func() int {
		for nextRecyclable < len(recyclableNumbers) {
			number := recyclableNumbers[nextRecyclable]
			nextRecyclable++
			if _, occupied := usedNumbers[number]; number > 0 && !occupied {
				return number
			}
		}
		for {
			if _, occupied := usedNumbers[nextModelNumber]; !occupied {
				break
			}
			nextModelNumber++
		}
		return nextModelNumber
	}

	// Second pass: assign numbers to newly available models.
	for _, key := range availableKeys {
		if _, exists := assignedNumbers[key]; exists {
			continue
		}
		number := allocateNumber()
		usedNumbers[number] = key
		assignedNumbers[key] = number
		if number >= nextModelNumber {
			nextModelNumber = number + 1
		}
	}

	// Upsert all available models into local cache.
	for _, key := range availableKeys {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		model := availableByKey[key]

		meta := &storage.ModelMeta{
			ID:          model.ID,
			Number:      assignedNumbers[key],
			ProviderID:  model.ProviderID,
			Name:        model.Name,
			Family:      model.Family,
			Status:      model.Status,
			ReleaseDate: model.ReleaseDate,
		}
		if err := m.store.StoreModel(meta); err != nil {
			log.Warnf("Failed to store model %s/%s: %v", model.ProviderID, model.ID, err)
		}
	}

	// Remove models that are no longer available.
	for key, existing := range existingByKey {
		if _, stillAvailable := availableByKey[key]; stillAvailable {
			continue
		}
		if err := m.store.DeleteModel(existing.ProviderID, existing.ID); err != nil {
			log.Warnf("Failed to delete stale model %s/%s: %v", existing.ProviderID, existing.ID, err)
		}
	}

	log.Infof("Synchronized %d available models from OpenCode providers", len(models))
	return nil
}

// SyncSessions synchronizes sessions from OpenCode to local storage
func (m *Manager) SyncSessions(ctx context.Context) error {
	opencodeSessions, err := m.client.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("failed to get sessions from OpenCode: %w", err)
	}

	// Get existing sessions from storage for comparison
	existingSessions, err := m.store.ListSessions()
	if err != nil {
		return fmt.Errorf("failed to list existing sessions: %w", err)
	}

	// Create map of existing session IDs for fast lookup
	existingSessionMap := make(map[string]bool)
	for _, sess := range existingSessions {
		existingSessionMap[sess.SessionID] = true
	}

	// Add or update sessions from OpenCode
	for _, ocSession := range opencodeSessions {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Use getOrCreateSessionMeta to ensure session metadata is stored
		// We use 0 as userID since we don't know the owner at sync time
		// getOrCreateSessionMeta will determine ownership from metadata
		m.getOrCreateSessionMeta(ocSession.ID, 0, ocSession.Metadata, ocSession.Title)
		delete(existingSessionMap, ocSession.ID)
	}

	// Remove sessions that no longer exist in OpenCode (orphaned sessions)
	for sessionID := range existingSessionMap {
		log.Debugf("Removing orphaned session from local storage: %s", sessionID)
		if err := m.store.DeleteSessionMeta(sessionID); err != nil {
			log.Warnf("Failed to remove orphaned session %s: %v", sessionID, err)
		}
	}

	log.Infof("Synchronized %d sessions from OpenCode", len(opencodeSessions))
	return nil
}

// GetUserLastModel retrieves the current model preference of a user.
func (m *Manager) GetUserLastModel(userID int64) (providerID, modelID string, exists bool, err error) {
	return m.store.GetUserLastModel(userID)
}

// SetUserLastModel sets the current model preference of a user.
func (m *Manager) SetUserLastModel(userID int64, providerID, modelID string) error {
	return m.store.StoreUserLastModel(userID, providerID, modelID)
}

// GetAllModels returns all preloaded models from storage
func (m *Manager) GetAllModels() ([]*storage.ModelMeta, error) {
	return m.store.ListModels()
}

// GetModel retrieves a specific model by ID
func (m *Manager) GetModel(providerID, modelID string) (*storage.ModelMeta, bool, error) {
	return m.store.GetModel(providerID, modelID)
}

// GetOrCreateSession gets the current session for a user, or creates a new one
func (m *Manager) GetOrCreateSession(ctx context.Context, userID int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if user has an existing session
	sessionID, exists, err := m.store.GetUserSession(userID)
	if err != nil {
		return "", err
	}
	if exists {
		// Update last used time
		meta, exists, err := m.store.GetSessionMeta(sessionID)
		if err != nil {
			return "", err
		}
		if exists {
			meta.LastUsedAt = time.Now()
			meta.MessageCount++
			if m.applyUserDefaultModelIfMissing(meta, userID) {
				log.Infof("Applied stored default model %s/%s to active session %s for user %d", meta.ProviderID, meta.ModelID, sessionID, userID)
			}
			if err := m.store.StoreSessionMeta(meta); err != nil {
				return "", err
			}
		}
		return sessionID, nil
	}

	// First, check if user has existing sessions in OpenCode
	opencodeSessions, err := m.client.ListSessions(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get sessions from OpenCode: %w", err)
	}

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
		ocSession := userSessions[0]

		// Check if we already have metadata for this session
		meta, exists, err := m.store.GetSessionMeta(ocSession.ID)
		if err != nil {
			return "", err
		}
		if !exists {
			// Create new metadata
			meta = &storage.SessionMeta{
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
			if m.applyUserDefaultModelIfMissing(meta, userID) {
				log.Infof("Applied stored default model %s/%s to discovered session %s for user %d", meta.ProviderID, meta.ModelID, ocSession.ID, userID)
			}

			if err := m.store.StoreSessionMeta(meta); err != nil {
				return "", err
			}
		} else {
			// Update existing metadata
			meta.LastUsedAt = time.Now()
			meta.UserID = userID  // Ensure user ID is correct
			meta.Status = "owned" // Ensure status is correct
			if m.applyUserDefaultModelIfMissing(meta, userID) {
				log.Infof("Applied stored default model %s/%s to discovered session %s for user %d", meta.ProviderID, meta.ModelID, ocSession.ID, userID)
			}
			if err := m.store.StoreSessionMeta(meta); err != nil {
				return "", err
			}
		}

		meta.MessageCount++

		// Store user mapping
		if err := m.store.StoreUserSession(userID, ocSession.ID); err != nil {
			return "", err
		}

		log.Infof("Using existing OpenCode session %s for user %d", ocSession.ID, userID)
		return ocSession.ID, nil
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
	meta := &storage.SessionMeta{
		SessionID:    session.ID,
		UserID:       userID,
		Name:         "Telegram Session",
		CreatedAt:    time.Now(),
		LastUsedAt:   time.Now(),
		MessageCount: 1,
	}

	// Extract model information from session metadata if available
	if session.Metadata != nil {
		if providerID := getMetadataString(session.Metadata, "provider_id", "providerID"); providerID != "" {
			meta.ProviderID = providerID
		}
		if modelID := getMetadataString(session.Metadata, "model_id", "modelID"); modelID != "" {
			meta.ModelID = modelID
		}
	}
	if m.applyUserDefaultModelIfMissing(meta, userID) {
		log.Infof("Applied stored default model %s/%s to new session %s for user %d", meta.ProviderID, meta.ModelID, session.ID, userID)
	}

	// Store metadata
	if err := m.store.StoreSessionMeta(meta); err != nil {
		return "", err
	}

	// Store user mapping
	if err := m.store.StoreUserSession(userID, session.ID); err != nil {
		return "", err
	}

	log.Infof("Created new session %s for user %d", session.ID, userID)
	return session.ID, nil
}

// GetUserSession gets the current session ID for a user
func (m *Manager) GetUserSession(userID int64) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessionID, exists, err := m.store.GetUserSession(userID)
	if err != nil {
		log.Errorf("Failed to get user session from store: %v", err)
		return "", false
	}
	return sessionID, exists
}

// SetUserSession sets a specific session for a user
func (m *Manager) SetUserSession(userID int64, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Update user session mapping
	if err := m.store.StoreUserSession(userID, sessionID); err != nil {
		return err
	}

	// Update or create metadata
	meta, exists, err := m.store.GetSessionMeta(sessionID)
	if err != nil {
		return err
	}
	if exists {
		meta.LastUsedAt = time.Now()
		meta.UserID = userID
		meta.Status = "owned"
		if m.applyUserDefaultModelIfMissing(meta, userID) {
			log.Infof("Applied stored default model %s/%s while switching session %s for user %d", meta.ProviderID, meta.ModelID, sessionID, userID)
		}
		if err := m.store.StoreSessionMeta(meta); err != nil {
			return err
		}
	} else {
		meta = &storage.SessionMeta{
			SessionID:    sessionID,
			UserID:       userID,
			Status:       "owned",
			CreatedAt:    time.Now(),
			LastUsedAt:   time.Now(),
			MessageCount: 0,
		}
		if m.applyUserDefaultModelIfMissing(meta, userID) {
			log.Infof("Applied stored default model %s/%s while creating local metadata for session %s user %d", meta.ProviderID, meta.ModelID, sessionID, userID)
		}
		if err := m.store.StoreSessionMeta(meta); err != nil {
			return err
		}
	}

	// Current model follows the switched current session when available.
	if strings.TrimSpace(meta.ProviderID) != "" && strings.TrimSpace(meta.ModelID) != "" {
		if err := m.store.StoreUserLastModel(userID, meta.ProviderID, meta.ModelID); err != nil {
			log.Warnf("Failed to update user current model while switching session: %v", err)
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
		// Prototype mode: surface upstream failures to caller instead of masking them
		// with local fallback, so Telegram users can see the real availability issue.
		return nil, fmt.Errorf("failed to get sessions from OpenCode: %w", err)
	}

	var allSessions []*SessionMeta

	// Create a map of OpenCode session IDs for fast lookup
	opencodeSessionMap := make(map[string]bool)
	for _, ocSession := range opencodeSessions {
		opencodeSessionMap[ocSession.ID] = true
	}

	// Get local sessions for this user
	localSessions := m.listLocalUserSessions(userID)

	// Remove local sessions that don't exist in OpenCode (orphaned sessions)
	for _, localSession := range localSessions {
		if !opencodeSessionMap[localSession.SessionID] {
			log.Debugf("Removing orphaned session for user %d: %s", userID, localSession.SessionID)
			if err := m.store.DeleteSessionMeta(localSession.SessionID); err != nil {
				log.Warnf("Failed to remove orphaned session %s: %v", localSession.SessionID, err)
			}
		}
	}

	// Process all OpenCode sessions, filter out child sessions (those with parentID)
	for _, ocSession := range opencodeSessions {
		// Skip sessions with parentID (child sessions like @explore subagent)
		if ocSession.ParentID != "" {
			log.Debugf("Skipping child session %s (parent: %s)", ocSession.ID, ocSession.ParentID)
			continue
		}
		meta := m.getOrCreateSessionMeta(ocSession.ID, userID, ocSession.Metadata, ocSession.Title)
		allSessions = append(allSessions, meta)
	}

	// Sync message count and model info from OpenCode to avoid stale local counters.
	m.syncSessionRuntimeInfo(ctx, allSessions)

	return allSessions, nil
}

// CreateNewSession creates a new session for a user
func (m *Manager) CreateNewSession(ctx context.Context, userID int64, name string) (string, error) {
	// Resolve user's current model for new sessions.
	// Priority: stored current model -> current session model.
	providerID, modelID := m.resolveUserPreferredModel(userID)
	return m.CreateNewSessionWithModel(ctx, userID, name, providerID, modelID)
}

func (m *Manager) resolveUserPreferredModel(userID int64) (providerID, modelID string) {
	providerID, modelID, exists, err := m.store.GetUserLastModel(userID)
	if err != nil {
		log.Warnf("Failed to get user %d current model: %v", userID, err)
	} else if exists && strings.TrimSpace(providerID) != "" && strings.TrimSpace(modelID) != "" {
		return providerID, modelID
	}

	candidates := make([]string, 0, 1)
	currentSessionID, currentExists, currentErr := m.store.GetUserSession(userID)
	if currentErr != nil {
		log.Warnf("Failed to get user %d current session while resolving preferred model: %v", userID, currentErr)
	} else if currentExists && strings.TrimSpace(currentSessionID) != "" {
		candidates = append(candidates, currentSessionID)
	}

	for _, sessionID := range candidates {
		meta, metaExists, metaErr := m.store.GetSessionMeta(sessionID)
		if metaErr != nil {
			log.Warnf("Failed to get session meta %s while resolving preferred model for user %d: %v", sessionID, userID, metaErr)
			continue
		}
		if !metaExists || meta == nil {
			continue
		}
		if strings.TrimSpace(meta.ProviderID) == "" || strings.TrimSpace(meta.ModelID) == "" {
			continue
		}

		if err := m.store.StoreUserLastModel(userID, meta.ProviderID, meta.ModelID); err != nil {
			log.Warnf("Failed to persist recovered current model %s/%s for user %d: %v", meta.ProviderID, meta.ModelID, userID, err)
		} else {
			log.Infof("Recovered user %d current model from session %s: %s/%s", userID, sessionID, meta.ProviderID, meta.ModelID)
		}
		return meta.ProviderID, meta.ModelID
	}

	return "", ""
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
	meta := &storage.SessionMeta{
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
	if err := m.store.StoreSessionMeta(meta); err != nil {
		return "", err
	}

	// Prototype mode: keep model binding in session metadata and apply it per message.
	// Do not block on /session/{id}/init because provider init can stall for a long time.
	if providerID != "" && modelID != "" {
		log.Debugf("Prototype mode: skip synchronous session init for new session %s with model %s/%s", session.ID, providerID, modelID)
	}

	// Persist user current model preference if a model was specified.
	if providerID != "" && modelID != "" {
		if err := m.store.StoreUserLastModel(userID, providerID, modelID); err != nil {
			log.Warnf("Failed to update user current model: %v", err)
			// Continue, as this is not critical
		}
	}

	// Persist current session mapping as part of session creation.
	if err := m.store.StoreUserSession(userID, session.ID); err != nil {
		return "", err
	}

	log.Infof("Created new named session %s (%s) with model %s/%s for user %d", session.ID, name, providerID, modelID, userID)
	return session.ID, nil
}

// SetSessionModel sets or changes the model metadata for an existing session.
// In prototype mode we rely on message-level model selection and skip /session/{id}/init.
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

	meta, exists, err := m.store.GetSessionMeta(sessionID)
	if err != nil {
		return err
	}
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

	if err := m.store.StoreSessionMeta(meta); err != nil {
		return err
	}

	if providerID != "" && modelID != "" {
		log.Debugf("SetSessionModel: prototype mode skip /session/%s/init for %s/%s; model will be applied per message", sessionID, providerID, modelID)
	}

	// Update user's current model preference.
	if meta.UserID != 0 {
		if err := m.store.StoreUserSession(meta.UserID, sessionID); err != nil {
			log.Warnf("Failed to update user current session: %v", err)
		}
		if err := m.store.StoreUserLastModel(meta.UserID, providerID, modelID); err != nil {
			log.Warnf("Failed to update user current model: %v", err)
			// Continue, as this is not critical
		}
	}

	totalTime := time.Since(startTime)
	log.Infof("Updated session %s model to %s/%s in %v", sessionID, providerID, modelID, totalTime)
	return nil
}

// RenameSession renames a session (allowed for owned or orphaned sessions)
func (m *Manager) RenameSession(ctx context.Context, userID int64, sessionID string, newName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, exists, err := m.store.GetSessionMeta(sessionID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Check if session can be renamed by this user
	if meta.Status == "other" {
		return fmt.Errorf("cannot rename session: session belongs to another user")
	}

	// If session is orphaned, assign it to the current user
	if meta.Status == "orphaned" {
		meta.UserID = userID
		meta.Status = "owned"
		log.Infof("Assigning orphaned session %s to user %d", sessionID, userID)
	}

	// Check if session is a child session (has parent)
	ocSession, err := m.client.GetSession(ctx, sessionID)
	if err != nil {
		log.Warnf("Failed to fetch session %s: %v", sessionID, err)
		// Continue with rename - OpenCode API will fail if session doesn't exist
	} else if ocSession.ParentID != "" {
		return fmt.Errorf("cannot rename child session: session is a subagent (parent: %s)", ocSession.ParentID)
	}

	// Rename in OpenCode (include userID in metadata)
	if err := m.client.RenameSession(ctx, sessionID, newName, userID); err != nil {
		return fmt.Errorf("failed to rename session in OpenCode: %w", err)
	}

	// Update local metadata
	meta.Name = newName
	meta.LastUsedAt = time.Now()
	if err := m.store.StoreSessionMeta(meta); err != nil {
		return err
	}

	log.Infof("Renamed session %s to '%s' for user %d", sessionID, newName, userID)
	return nil
}

// DeleteSession deletes a session (allowed for owned or orphaned sessions)
func (m *Manager) DeleteSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, exists, err := m.store.GetSessionMeta(sessionID)
	if err != nil {
		return err
	}
	if !exists {
		// Session not in local cache, check if it's a child session before deleting
		ocSession, err := m.client.GetSession(ctx, sessionID)
		if err != nil {
			// If we can't fetch the session, still try to delete (will fail anyway)
			log.Warnf("Failed to fetch session %s: %v", sessionID, err)
		} else if ocSession.ParentID != "" {
			return fmt.Errorf("cannot delete child session: session is a subagent (parent: %s)", ocSession.ParentID)
		}
		// Try to delete from OpenCode
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

	// Check if session is a child session (has parent)
	ocSession, err := m.client.GetSession(ctx, sessionID)
	if err != nil {
		log.Warnf("Failed to fetch session %s: %v", sessionID, err)
		// Continue with delete - OpenCode API will fail if session doesn't exist
	} else if ocSession.ParentID != "" {
		return fmt.Errorf("cannot delete child session: session is a subagent (parent: %s)", ocSession.ParentID)
	}

	// Delete from OpenCode
	if err := m.client.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("failed to delete session from OpenCode: %w", err)
	}

	// Remove from user mapping if this is their current session
	// We need to find which user has this session as current
	allSessions, err := m.store.ListSessions()
	if err != nil {
		log.Warnf("Failed to list sessions during delete: %v", err)
	} else {
		for _, sess := range allSessions {
			if sess.SessionID == sessionID && sess.UserID != 0 {
				// This session belongs to a user, remove their mapping
				if err := m.store.DeleteUserSession(sess.UserID); err != nil {
					log.Warnf("Failed to delete user session mapping for user %d: %v", sess.UserID, err)
				}
				break
			}
		}
	}

	// Remove from local cache
	if err := m.store.DeleteSessionMeta(sessionID); err != nil {
		return err
	}

	log.Infof("Deleted session %s ('%s')", sessionID, meta.Name)
	return nil
}

// GetSessionMeta gets metadata for a session
func (m *Manager) GetSessionMeta(sessionID string) (*SessionMeta, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	meta, exists, err := m.store.GetSessionMeta(sessionID)
	if err != nil {
		log.Errorf("Failed to get session meta from store: %v", err)
		return nil, false
	}
	return meta, exists
}

// CleanupInactiveSessions removes sessions that haven't been used for a while
func (m *Manager) CleanupInactiveSessions(maxAge time.Duration) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	removed, err := m.store.CleanupInactiveSessions(maxAge)
	if err != nil {
		log.Errorf("Failed to cleanup inactive sessions: %v", err)
		return nil
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

	sessions, err := m.store.ListSessions()
	if err != nil {
		log.Errorf("Failed to list sessions: %v", err)
		return 0
	}
	return len(sessions)
}

// listLocalUserSessions lists sessions from local storage only
func (m *Manager) listLocalUserSessions(userID int64) []*SessionMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions, err := m.store.ListSessions()
	if err != nil {
		log.Errorf("Failed to list sessions: %v", err)
		return nil
	}

	var userSessions []*SessionMeta
	for _, meta := range sessions {
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
	meta, exists, err := m.store.GetSessionMeta(sessionID)
	if err != nil {
		// If error, create new meta
		log.Warnf("Failed to get session meta for %s: %v, creating new", sessionID, err)
		exists = false
	}
	if exists {
		updated := false

		// Update last used time and recalculate status based on current user
		meta.LastUsedAt = time.Now()

		// Keep local metadata in sync with upstream session metadata.
		if title != "" && title != meta.Name {
			meta.Name = title
			updated = true
		}

		if providerID := getMetadataString(metadata, "provider_id", "providerID"); providerID != "" && providerID != meta.ProviderID {
			meta.ProviderID = providerID
			updated = true
		}
		if modelID := getMetadataString(metadata, "model_id", "modelID"); modelID != "" && modelID != meta.ModelID {
			meta.ModelID = modelID
			updated = true
		}

		// Recalculate status based on owner ID and current user
		if meta.UserID == 0 {
			meta.Status = "orphaned"
		} else if meta.UserID == currentUserID {
			meta.Status = "owned"
		} else {
			meta.Status = "other"
		}
		updated = true // status/last used are refreshed on each list call.

		if updated {
			if err := m.store.StoreSessionMeta(meta); err != nil {
				log.Warnf("Failed to store updated session meta: %v", err)
			}
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
	meta = &storage.SessionMeta{
		SessionID:  sessionID,
		UserID:     ownerID,
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
	}

	// Extract name from provided title, metadata, or use default
	if title != "" {
		meta.Name = title
	} else if sessionName := getMetadataString(metadata, "session_name"); sessionName != "" {
		meta.Name = sessionName
	} else if metadataTitle := getMetadataString(metadata, "title"); metadataTitle != "" {
		meta.Name = metadataTitle
	} else {
		meta.Name = "Telegram Session"
	}

	// Extract model information if available
	if providerID := getMetadataString(metadata, "provider_id", "providerID"); providerID != "" {
		meta.ProviderID = providerID
	}
	if modelID := getMetadataString(metadata, "model_id", "modelID"); modelID != "" {
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
	if err := m.store.StoreSessionMeta(meta); err != nil {
		log.Warnf("Failed to store new session meta: %v", err)
	}

	// Update user session mapping only if this session belongs to the current user
	if meta.Status == "owned" {
		currentSessionID, exists, err := m.store.GetUserSession(currentUserID)
		if err != nil {
			log.Warnf("Failed to get user session for %d: %v", currentUserID, err)
		} else if !exists || currentSessionID != sessionID {
			if err := m.store.StoreUserSession(currentUserID, sessionID); err != nil {
				log.Warnf("Failed to store user session mapping: %v", err)
			}
		}
	}

	return meta
}

func getMetadataString(metadata map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		value, ok := metadata[key]
		if !ok {
			continue
		}
		str, ok := value.(string)
		if !ok {
			continue
		}
		str = strings.TrimSpace(str)
		if str != "" {
			return str
		}
	}
	return ""
}

func (m *Manager) applyUserDefaultModelIfMissing(meta *storage.SessionMeta, userID int64) bool {
	if meta == nil || userID == 0 {
		return false
	}
	if strings.TrimSpace(meta.ProviderID) != "" && strings.TrimSpace(meta.ModelID) != "" {
		return false
	}

	providerID, modelID, exists, err := m.store.GetUserLastModel(userID)
	if err != nil {
		log.Warnf("Failed to get user %d current model: %v", userID, err)
		return false
	}
	if !exists {
		return false
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" {
		return false
	}

	meta.ProviderID = providerID
	meta.ModelID = modelID
	return true
}

func (m *Manager) syncSessionRuntimeInfo(ctx context.Context, sessions []*SessionMeta) {
	for _, meta := range sessions {
		select {
		case <-ctx.Done():
			return
		default:
		}

		messages, err := m.client.GetMessages(ctx, meta.SessionID)
		if err != nil {
			log.Warnf("Failed to fetch messages for session %s: %v", meta.SessionID, err)
			continue
		}

		updated := false
		if meta.MessageCount != len(messages) {
			meta.MessageCount = len(messages)
			updated = true
		}

		// Recover model info from the latest message when metadata is missing.
		for i := len(messages) - 1; i >= 0 && (meta.ProviderID == "" || meta.ModelID == ""); i-- {
			msg := messages[i]
			if meta.ProviderID == "" && msg.ProviderID != "" {
				meta.ProviderID = msg.ProviderID
				updated = true
			}
			if meta.ModelID == "" && msg.ModelID != "" {
				meta.ModelID = msg.ModelID
				updated = true
			}
		}

		if !updated {
			continue
		}

		if err := m.store.StoreSessionMeta(meta); err != nil {
			log.Warnf("Failed to store updated session meta: %v", err)
		}
	}
}

// Close closes the storage backend
func (m *Manager) Close() error {
	return m.store.Close()
}
