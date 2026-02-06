package storage

import (
	"fmt"
)

// NewStore creates a new store based on options
func NewStore(opts Options) (Store, error) {
	// Only file storage is supported
	storageType := opts.Type
	if storageType == "" || storageType == "file" {
		if opts.FilePath == "" {
			return nil, fmt.Errorf("file path is required for file storage")
		}
		return NewFileStore(opts.FilePath)
	}
	return nil, fmt.Errorf("unsupported storage type: %s, only 'file' storage is supported", storageType)
}
