package main

import (
	"context"
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

var version = "dev"

func main() {
	var (
		configPath  string
		showVersion bool
		showHelp    bool
	)

	flag.StringVar(&configPath, "config", "", "Path to config file (default: config.toml)")
	flag.BoolVar(&showVersion, "version", false, "Show version information")
	flag.BoolVar(&showHelp, "help", false, "Show help information")
	flag.Parse()

	if showHelp {
		printUsage()
		return
	}

	if showVersion {
		fmt.Printf("tg-bot version %s\n", version)
		return
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(1)
	}

	logger, err := logging.Init(cfg.Logging.Level, cfg.Logging.Output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	log.SetOutput(logger.Out)
	log.SetFormatter(logger.Formatter)
	log.SetLevel(logger.GetLevel())
	log.SetReportCaller(logger.ReportCaller)

	tgClient, err := newTelegramHTTPClient(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize Telegram HTTP client: %v", err)
	}

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token: cfg.Telegram.Token,
		Poller: &telebot.LongPoller{
			Timeout: time.Duration(cfg.Telegram.PollingTimeout) * time.Second,
			Limit:   cfg.Telegram.PollingLimit,
		},
		Client: tgClient,
		OnError: func(err error, _ telebot.Context) {
			log.Errorf("Telegram bot error: %v", err)
		},
	})
	if err != nil {
		log.Fatalf("Failed to create Telegram bot: %v", err)
	}

	appBot, err := handler.NewBot(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize app bot: %v", err)
	}

	appBot.SetTelegramBot(tgBot)
	appBot.Start()

	done := make(chan struct{})
	go func() {
		log.Info("Telegram bot started")
		tgBot.Start()
		close(done)
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-done:
		log.Info("Telegram bot stopped")
	case <-sigCtx.Done():
		log.Info("Shutdown signal received")
		tgBot.Stop()
		<-done
	}

	if err := appBot.Close(); err != nil {
		log.Errorf("Failed to close app bot: %v", err)
	}
}

func printUsage() {
	fmt.Printf(`OpenCode Telegram Bot

Usage:
  tg-bot [--config <path>] [--version] [--help]

Options:
  --config <path>   Path to configuration file (default: config.toml)
  --version         Show version information
  --help            Show this help message
`)
}

func newTelegramHTTPClient(cfg *config.Config) (*http.Client, error) {
	transport := &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
	}

	if cfg.Proxy.Enabled {
		proxyURL, err := url.Parse(cfg.Proxy.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", cfg.Proxy.URL, err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
		log.Infof("Telegram API requests will use proxy: %s", cfg.Proxy.URL)
	}

	timeout := time.Duration(cfg.Telegram.PollingTimeout+20) * time.Second
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}
