package storage

import (
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

	// Maintenance
	Close() error
}

// Options contains configuration options for storage
type Options struct {
	Type     string // "file" (only file storage is supported)
	FilePath string // path to JSON file for session storage
}
