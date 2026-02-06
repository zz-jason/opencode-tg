package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Create a temporary config file
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.toml")

	configContent := `
[telegram]
token = "test_token"
polling_timeout = 60
polling_limit = 100

[proxy]
enabled = true
url = "http://proxy:7890"

[opencode]
url = "http://192.168.50.100:8080"
timeout = 30

[storage]
type = "file"
sqlite_path = "sessions.db"

[logging]
level = "info"
output = "bot.log"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Test loading config
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify values
	if cfg.Telegram.Token != "test_token" {
		t.Errorf("Expected token 'test_token', got %s", cfg.Telegram.Token)
	}
	if cfg.Telegram.PollingTimeout != 60 {
		t.Errorf("Expected polling_timeout 60, got %d", cfg.Telegram.PollingTimeout)
	}
	if !cfg.Proxy.Enabled {
		t.Error("Expected proxy enabled")
	}
	if cfg.Proxy.URL != "http://proxy:7890" {
		t.Errorf("Expected proxy URL 'http://proxy:7890', got %s", cfg.Proxy.URL)
	}
	if cfg.OpenCode.URL != "http://192.168.50.100:8080" {
		t.Errorf("Expected OpenCode URL 'http://192.168.50.100:8080', got %s", cfg.OpenCode.URL)
	}
	if cfg.OpenCode.Timeout != 30 {
		t.Errorf("Expected OpenCode timeout 30, got %d", cfg.OpenCode.Timeout)
	}
	if cfg.Storage.Type != "file" {
		t.Errorf("Expected storage type 'file', got %s", cfg.Storage.Type)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Expected log level 'info', got %s", cfg.Logging.Level)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.toml")

	// Minimal config
	configContent := `
[telegram]
token = "test_token"

[opencode]
url = "http://192.168.50.100:8080"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify defaults are applied
	if cfg.Telegram.PollingTimeout != 60 {
		t.Errorf("Expected default polling_timeout 60, got %d", cfg.Telegram.PollingTimeout)
	}
	if cfg.Telegram.PollingLimit != 100 {
		t.Errorf("Expected default polling_limit 100, got %d", cfg.Telegram.PollingLimit)
	}
	if cfg.OpenCode.Timeout != 30 {
		t.Errorf("Expected default OpenCode timeout 30, got %d", cfg.OpenCode.Timeout)
	}
	if cfg.Storage.Type != "file" {
		t.Errorf("Expected default storage type 'file', got %s", cfg.Storage.Type)
	}
	if cfg.Storage.FilePath != "sessions.json" {
		t.Errorf("Expected default file path 'sessions.json', got %s", cfg.Storage.FilePath)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Expected default log level 'info', got %s", cfg.Logging.Level)
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
	}{
		{
			name: "valid config",
			config: &Config{
				Telegram: TelegramConfig{Token: "valid_token"},
				OpenCode: OpenCodeConfig{URL: "http://localhost:8080"},
				Proxy:    ProxyConfig{Enabled: false},
			},
			wantErr: false,
		},
		{
			name: "missing telegram token",
			config: &Config{
				Telegram: TelegramConfig{Token: ""},
				OpenCode: OpenCodeConfig{URL: "http://localhost:8080"},
			},
			wantErr: true,
		},
		{
			name: "missing OpenCode URL",
			config: &Config{
				Telegram: TelegramConfig{Token: "valid_token"},
				OpenCode: OpenCodeConfig{URL: ""},
			},
			wantErr: true,
		},
		{
			name: "proxy enabled but no URL",
			config: &Config{
				Telegram: TelegramConfig{Token: "valid_token"},
				OpenCode: OpenCodeConfig{URL: "http://localhost:8080"},
				Proxy:    ProxyConfig{Enabled: true, URL: ""},
			},
			wantErr: true,
		},
		{
			name: "proxy enabled with URL",
			config: &Config{
				Telegram: TelegramConfig{Token: "valid_token"},
				OpenCode: OpenCodeConfig{URL: "http://localhost:8080"},
				Proxy:    ProxyConfig{Enabled: true, URL: "http://proxy:7890"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr && err == nil {
				t.Error("Expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Unexpected validation error: %v", err)
			}
		})
	}
}

func TestConfigError(t *testing.T) {
	err := &ConfigError{
		Field:   "telegram.token",
		Message: "token is required",
	}

	expected := "telegram.token: token is required"
	if err.Error() != expected {
		t.Errorf("Expected error message %q, got %q", expected, err.Error())
	}
}
