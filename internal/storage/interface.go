package storage

import (
	"strings"
	"time"
)

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

// ModelMeta contains metadata about an AI model
type ModelMeta struct {
	ID          string `json:"id"`
	Number      int    `json:"number,omitempty"`
	ProviderID  string `json:"providerID"`
	Name        string `json:"name"`
	Family      string `json:"family"`
	Status      string `json:"status,omitempty"`
	ReleaseDate string `json:"release_date,omitempty"`
}

// ModelKey returns the canonical storage key for a provider/model pair.
func ModelKey(providerID, modelID string) string {
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if providerID == "" {
		return modelID
	}
	if modelID == "" {
		return providerID
	}
	return providerID + "/" + modelID
}

// Store defines the interface for persistent session storage
type Store interface {
	// UserSession operations
	StoreUserSession(userID int64, sessionID string) error
	GetUserSession(userID int64) (string, bool, error)
	DeleteUserSession(userID int64) error

	// SessionMeta operations
	StoreSessionMeta(meta *SessionMeta) error
	GetSessionMeta(sessionID string) (*SessionMeta, bool, error)
	DeleteSessionMeta(sessionID string) error

	// Batch operations
	ListSessions() ([]*SessionMeta, error)
	CleanupInactiveSessions(maxAge time.Duration) ([]string, error)

	// ModelMeta operations
	StoreModel(meta *ModelMeta) error
	GetModel(providerID, modelID string) (*ModelMeta, bool, error)
	ListModels() ([]*ModelMeta, error)
	DeleteModel(providerID, modelID string) error

	// UserPreference operations
	StoreUserLastModel(userID int64, providerID, modelID string) error
	GetUserLastModel(userID int64) (providerID, modelID string, exists bool, err error)

	// Maintenance
	Close() error
}

// Options contains configuration options for storage
type Options struct {
	Type     string // "file" (only file storage is supported)
	FilePath string // path to JSON file for session storage
}
