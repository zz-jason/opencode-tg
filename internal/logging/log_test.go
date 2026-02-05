package logging

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestInitLogger(t *testing.T) {
	tests := []struct {
		name    string
		level   string
		output  string
		wantErr bool
	}{
		{
			name:    "valid info level to stdout",
			level:   "info",
			output:  "stdout",
			wantErr: false,
		},
		{
			name:    "valid debug level to stdout",
			level:   "debug",
			output:  "stdout",
			wantErr: false,
		},
		{
			name:    "invalid level defaults to info",
			level:   "invalid",
			output:  "stdout",
			wantErr: false,
		},
		{
			name:    "valid level with file output",
			level:   "warn",
			output:  "test.log",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			outputPath := tt.output
			if tt.output != "stdout" {
				outputPath = filepath.Join(tempDir, tt.output)
			}

			logger, err := Init(tt.level, outputPath)
			if tt.wantErr && err == nil {
				t.Error("Expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if err == nil && logger != nil {
				// Verify log level
				if tt.level == "invalid" {
					if logger.GetLevel() != logrus.InfoLevel {
						t.Errorf("Expected default level info for invalid input, got %v", logger.GetLevel())
					}
				} else {
					expectedLevel, _ := logrus.ParseLevel(tt.level)
					if logger.GetLevel() != expectedLevel {
						t.Errorf("Expected level %v, got %v", expectedLevel, logger.GetLevel())
					}
				}

				// Verify formatter is set
				if logger.Formatter == nil {
					t.Error("Formatter should be set")
				}
			}

			// Clean up test file
			if tt.output != "stdout" {
				os.Remove(outputPath)
			}
		})
	}
}

func TestInitLoggerWithFile(t *testing.T) {
	tempDir := t.TempDir()
	logFile := filepath.Join(tempDir, "test.log")

	logger, err := Init("info", logFile)
	if err != nil {
		t.Fatalf("Failed to initialize logger: %v", err)
	}

	if logger == nil {
		t.Fatal("Logger should not be nil")
	}

	// Test that we can write to the logger
	logger.Info("Test log message")

	// Verify file was created
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Error("Log file should have been created")
	}

	// Clean up
	os.Remove(logFile)
}

func TestInitLoggerWithNestedDirectory(t *testing.T) {
	tempDir := t.TempDir()
	nestedPath := filepath.Join(tempDir, "nested", "dir", "test.log")

	logger, err := Init("info", nestedPath)
	if err != nil {
		t.Fatalf("Failed to initialize logger with nested directory: %v", err)
	}

	if logger == nil {
		t.Fatal("Logger should not be nil")
	}

	// Verify directory was created
	dir := filepath.Dir(nestedPath)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("Nested directory should have been created")
	}

	// Clean up
	os.RemoveAll(filepath.Join(tempDir, "nested"))
}
