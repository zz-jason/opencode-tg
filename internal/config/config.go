package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	log "github.com/sirupsen/logrus"
)

// Config represents the entire configuration structure
type Config struct {
	Telegram TelegramConfig `toml:"telegram"`
	Proxy    ProxyConfig    `toml:"proxy"`
	OpenCode OpenCodeConfig `toml:"opencode"`
	Storage  StorageConfig  `toml:"storage"`
	Render   RenderConfig   `toml:"render"`
	Logging  LoggingConfig  `toml:"logging"`
}

// TelegramConfig contains Telegram Bot settings
type TelegramConfig struct {
	Token          string `toml:"token"`
	PollingTimeout int    `toml:"polling_timeout"`
	PollingLimit   int    `toml:"polling_limit"`
}

// ProxyConfig contains HTTP proxy settings
type ProxyConfig struct {
	Enabled bool   `toml:"enabled"`
	URL     string `toml:"url"`
}

// OpenCodeConfig contains OpenCode API settings
type OpenCodeConfig struct {
	URL     string `toml:"url"`
	Timeout int    `toml:"timeout"`
}

// StorageConfig contains session storage settings
type StorageConfig struct {
	Type     string `toml:"type"`
	FilePath string `toml:"file_path"` // path to JSON file for session storage
}

// RenderConfig controls Telegram rendering behavior for OpenCode output
type RenderConfig struct {
	Mode string `toml:"mode"` // plain | markdown_final | markdown_stream
}

// LoggingConfig contains logging settings
type LoggingConfig struct {
	Level  string `toml:"level"`
	Output string `toml:"output"`
}

// Load reads and parses the configuration file
func Load(configPath string) (*Config, error) {
	// If no config path provided, try default locations
	if configPath == "" {
		configPath = getDefaultConfigPath()
	}

	log.Infof("Loading configuration from: %s", configPath)

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Set default values if not specified
	setDefaults(&cfg)

	return &cfg, nil
}

// getDefaultConfigPath returns the default configuration file path
func getDefaultConfigPath() string {
	// First try current directory
	if _, err := os.Stat("config.toml"); err == nil {
		return "config.toml"
	}

	// Then try config directory
	configDir := "config"
	if _, err := os.Stat(filepath.Join(configDir, "config.toml")); err == nil {
		return filepath.Join(configDir, "config.toml")
	}

	// Default to current directory
	return "config.toml"
}

// setDefaults applies default values to configuration fields
func setDefaults(cfg *Config) {
	if cfg.Telegram.PollingTimeout == 0 {
		cfg.Telegram.PollingTimeout = 60
	}
	if cfg.Telegram.PollingLimit == 0 {
		cfg.Telegram.PollingLimit = 100
	}
	if cfg.OpenCode.Timeout == 0 {
		cfg.OpenCode.Timeout = 30
	}
	if cfg.Storage.Type == "" {
		cfg.Storage.Type = "file"
	}
	if cfg.Storage.FilePath == "" && cfg.Storage.Type == "file" {
		cfg.Storage.FilePath = "bot-state.json"
	}
	if cfg.Render.Mode == "" {
		cfg.Render.Mode = "markdown_stream"
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Output == "" {
		cfg.Logging.Output = "bot.log"
	}
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.Telegram.Token == "" {
		return &ConfigError{Field: "telegram.token", Message: "telegram token is required"}
	}
	if c.Proxy.Enabled && c.Proxy.URL == "" {
		return &ConfigError{Field: "proxy.url", Message: "proxy URL is required when proxy is enabled"}
	}
	if c.OpenCode.URL == "" {
		return &ConfigError{Field: "opencode.url", Message: "OpenCode URL is required"}
	}
	switch strings.ToLower(strings.TrimSpace(c.Render.Mode)) {
	case "", "plain", "markdown_final", "markdown_stream":
	default:
		return &ConfigError{Field: "render.mode", Message: "must be one of: plain, markdown_final, markdown_stream"}
	}
	return nil
}

// ConfigError represents a configuration validation error
type ConfigError struct {
	Field   string
	Message string
}

func (e *ConfigError) Error() string {
	return e.Field + ": " + e.Message
}
