package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/telebot.v4"
	"tg-bot/internal/config"
	"tg-bot/internal/handler"
	"tg-bot/internal/logging"
)

const (
	// Version is the current version of the bot
	Version = "1.0.0"
	// BuildDate is the date when the binary was built (set during build)
	BuildDate = "unknown"
)

func main() {
	// Parse command line arguments
	var configPath string
	var showHelp bool
	var showVersion bool

	flag.StringVar(&configPath, "config", "", "Path to configuration file (default: config.toml)")
	flag.BoolVar(&showHelp, "help", false, "Show this help message")
	flag.BoolVar(&showHelp, "h", false, "Show this help message (shorthand)")
	flag.BoolVar(&showVersion, "version", false, "Show version information")
	flag.BoolVar(&showVersion, "v", false, "Show version information (shorthand)")
	flag.Parse()

	if showHelp {
		printUsage()
		return
	}

	if showVersion {
		fmt.Printf("OpenCode Telegram Bot v%s (Built: %s)\n", Version, BuildDate)
		return
	}

	// Load configuration: command line argument or default
	if configPath == "" {
		configPath = "config.toml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	// Initialize logging
	logger, err := logging.Init(cfg.Logging.Level, cfg.Logging.Output)
	if err != nil {
		log.Fatalf("Failed to initialize logging: %v", err)
	}
	logger.Info("Starting Telegram Bot for OpenCode")

	// Create HTTP client for Telegram bot with proxy if enabled
	tgHTTPClient := &http.Client{
		Timeout: 60 * time.Second, // Increased timeout for Telegram API
	}

	if cfg.Proxy.Enabled && cfg.Proxy.URL != "" {
		logger.Infof("Using proxy: %s", cfg.Proxy.URL)
		proxyURL, err := url.Parse(cfg.Proxy.URL)
		if err != nil {
			logger.Fatalf("Invalid proxy URL: %v", err)
		}

		tgHTTPClient.Transport = &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			IdleConnTimeout: 90 * time.Second,
		}
	}

	// Create Telegram bot
	botSettings := telebot.Settings{
		Token:     cfg.Telegram.Token,
		Poller:    &telebot.LongPoller{Timeout: time.Duration(cfg.Telegram.PollingTimeout) * time.Second},
		Client:    tgHTTPClient,
		Verbose:   cfg.Logging.Level == "debug",
		ParseMode: telebot.ModeDefault, // Use plain text to avoid Markdown parsing errors
	}

	tgBot, err := telebot.NewBot(botSettings)
	if err != nil {
		logger.Fatalf("Failed to create Telegram bot: %v", err)
	}

	logger.Infof("Telegram bot authorized as @%s", tgBot.Me.Username)

	// Create bot handler
	botHandler, err := handler.NewBot(cfg)
	if err != nil {
		logger.Fatalf("Failed to create bot handler: %v", err)
	}
	botHandler.SetTelegramBot(tgBot)
	botHandler.Start()

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("Bot is now running. Press Ctrl+C to exit.")

	// Start the bot in a goroutine
	go func() {
		tgBot.Start()
	}()

	// Wait for shutdown signal
	sig := <-sigChan
	logger.Infof("Received signal %v, shutting down...", sig)

	// Stop the bot
	tgBot.Stop()

	logger.Info("Bot shutdown complete")
}

// printUsage displays command line usage information
func printUsage() {
	fmt.Println("OpenCode Telegram Bot")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  tg-bot [options]")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  --config <path>    Path to configuration file")
	fmt.Println("                     (default: config.toml)")
	fmt.Println("  --help, -h         Show this help message")
	fmt.Println("  --version, -v      Show version information")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  tg-bot --config /path/to/config.toml")
	fmt.Println("  tg-bot                         # Uses config.toml in current directory")
}
