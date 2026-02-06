//go:build integration

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/telebot.v4"
	"tg-bot/internal/config"
)

type telegramRecorder struct {
	t        *testing.T
	messages chan string
	mu       sync.Mutex
	nextID   int
}

func newTelegramRecorder(t *testing.T) *telegramRecorder {
	return &telegramRecorder{
		t:        t,
		messages: make(chan string, 64),
		nextID:   1,
	}
}

func (r *telegramRecorder) serveHTTP(w http.ResponseWriter, req *http.Request) {
	method := pathMethod(req.URL.Path)
	switch method {
	case "sendMessage", "editMessageText":
		r.handleSendLike(w, req)
	case "getMe":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true,
			"result": map[string]interface{}{
				"id":         1,
				"is_bot":     true,
				"first_name": "integration-bot",
				"username":   "integration_bot",
			},
		})
	default:
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     true,
			"result": true,
		})
	}
}

func (r *telegramRecorder) handleSendLike(w http.ResponseWriter, req *http.Request) {
	var payload map[string]interface{}
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		r.t.Fatalf("failed to decode telegram request body: %v", err)
	}

	text, _ := payload["text"].(string)
	chatID := parseChatID(payload["chat_id"])

	r.mu.Lock()
	msgID := r.nextID
	r.nextID++
	r.mu.Unlock()

	select {
	case r.messages <- text:
	default:
		r.t.Fatalf("telegram message channel is full")
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"message_id": msgID,
			"date":       time.Now().Unix(),
			"text":       text,
			"chat": map[string]interface{}{
				"id":   chatID,
				"type": "private",
			},
		},
	})
}

func pathMethod(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func parseChatID(raw interface{}) int64 {
	switch v := raw.(type) {
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return n
		}
	case float64:
		return int64(v)
	}
	return 0
}

func waitForTelegramMessage(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for telegram response")
		return ""
	}
}

func runCommand(t *testing.T, tgBot *telebot.Bot, out <-chan string, userID int64, updateID int, text string) string {
	t.Helper()
	tgBot.ProcessUpdate(telebot.Update{
		ID: updateID,
		Message: &telebot.Message{
			ID:       updateID,
			Text:     text,
			Unixtime: time.Now().Unix(),
			Sender: &telebot.User{
				ID:        userID,
				FirstName: "Integration",
			},
			Chat: &telebot.Chat{
				ID:   userID,
				Type: telebot.ChatPrivate,
			},
		},
	})
	return waitForTelegramMessage(t, out)
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("expected response to contain %q, got:\n%s", want, got)
	}
}

func TestIntegration_HandleCoreCommands(t *testing.T) {
	t.Helper()

	tmpDir := t.TempDir()
	opencodeHome := filepath.Join(tmpDir, "opencode-home")
	if err := os.MkdirAll(opencodeHome, 0o755); err != nil {
		t.Fatalf("failed to create opencode home: %v", err)
	}

	opencodeBin := os.Getenv("OPENCODE_BIN")
	if opencodeBin == "" {
		opencodeBin = installOpenCode(t, opencodeHome)
	}

	baseURL, stopOpenCode := startOpenCodeServer(t, opencodeBin, opencodeHome)
	defer stopOpenCode()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
		OnError: func(err error, _ telebot.Context) {
			t.Logf("telegram error: %v", err)
		},
	})
	if err != nil {
		t.Fatalf("failed to create telegram bot: %v", err)
	}

	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			Token:          "integration-token",
			PollingTimeout: 5,
			PollingLimit:   100,
		},
		Proxy: config.ProxyConfig{
			Enabled: false,
			URL:     "",
		},
		OpenCode: config.OpenCodeConfig{
			URL:     baseURL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(tmpDir, "sessions.json"),
		},
		Logging: config.LoggingConfig{
			Level:  "error",
			Output: "stdout",
		},
	}

	bot, err := NewBot(cfg)
	if err != nil {
		t.Fatalf("failed to create handler bot: %v", err)
	}
	defer func() {
		if closeErr := bot.Close(); closeErr != nil {
			t.Fatalf("failed to close handler bot: %v", closeErr)
		}
	}()

	bot.SetTelegramBot(tgBot)
	bot.Start()

	const userID int64 = 10001
	resp := runCommand(t, tgBot, rec.messages, userID, 1, "/sessions")
	assertContains(t, resp, "You don't have any sessions yet.")

	resp = runCommand(t, tgBot, rec.messages, userID, 2, "/current")
	assertContains(t, resp, "You don't have a current session.")

	resp = runCommand(t, tgBot, rec.messages, userID, 3, "/status")
	assertContains(t, resp, "You don't have a current session.")

	resp = runCommand(t, tgBot, rec.messages, userID, 4, "/new Integration Session")
	assertContains(t, resp, "✅ Created new session: Integration Session")

	resp = runCommand(t, tgBot, rec.messages, userID, 5, "/sessions")
	assertContains(t, resp, "📋 Available Sessions")
	assertContains(t, resp, "[✅ CURRENT] 1. Integration Session")

	resp = runCommand(t, tgBot, rec.messages, userID, 6, "/current")
	assertContains(t, resp, "📁 Current Session")
	assertContains(t, resp, "• Name: Integration Session")
	assertContains(t, resp, "• Current model: Default")
	assertContains(t, resp, "• Status: Waiting For Your Input")

	resp = runCommand(t, tgBot, rec.messages, userID, 7, "/status")
	assertContains(t, resp, "Current session has no messages yet.")
}

func installOpenCode(t *testing.T, opencodeHome string) string {
	t.Helper()

	scriptPath := filepath.Join(opencodeHome, "install-opencode.sh")
	if err := downloadInstallerScript(scriptPath); err != nil {
		t.Fatalf("failed to download opencode install script: %v", err)
	}

	version := strings.TrimSpace(os.Getenv("OPENCODE_VERSION"))
	output, err := runInstaller(opencodeHome, scriptPath, version)
	if err != nil && version == "" && strings.Contains(string(output), "Failed to fetch version information") {
		latestVersion, versionErr := resolveLatestOpenCodeVersion()
		if versionErr != nil {
			t.Fatalf("failed to install opencode and failed to resolve latest version: %v\ninstaller output:\n%s", versionErr, string(output))
		}

		output, err = runInstaller(opencodeHome, scriptPath, latestVersion)
	}
	if err != nil {
		t.Fatalf("failed to install opencode: %v\noutput:\n%s", err, string(output))
	}

	opencodeBin := filepath.Join(opencodeHome, ".opencode", "bin", "opencode")
	if _, statErr := os.Stat(opencodeBin); statErr != nil {
		existingBin, lookPathErr := exec.LookPath("opencode")
		if lookPathErr == nil {
			return existingBin
		}
		t.Fatalf("opencode binary not found after install: %v", statErr)
	}

	return opencodeBin
}

func runInstaller(opencodeHome, scriptPath, version string) ([]byte, error) {
	args := []string{scriptPath, "--no-modify-path"}
	if version != "" {
		args = append(args, "--version", version)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", args...)
	cmd.Env = append(os.Environ(), "HOME="+opencodeHome)
	return cmd.CombinedOutput()
}

func resolveLatestOpenCodeVersion() (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get("https://github.com/anomalyco/opencode/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	finalURL := resp.Request.URL.String()
	idx := strings.LastIndex(finalURL, "/tag/")
	if idx == -1 {
		return "", fmt.Errorf("unexpected latest release URL: %s", finalURL)
	}

	tag := finalURL[idx+len("/tag/"):]
	tag = strings.TrimPrefix(tag, "v")
	if tag == "" {
		return "", fmt.Errorf("empty version parsed from URL: %s", finalURL)
	}
	return tag, nil
}

func downloadInstallerScript(scriptPath string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get("https://opencode.ai/install")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected installer status code: %d", resp.StatusCode)
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if err := os.WriteFile(scriptPath, content, 0o755); err != nil {
		return err
	}
	return nil
}

func startOpenCodeServer(t *testing.T, opencodeBin, opencodeHome string) (string, func()) {
	t.Helper()

	port := freePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	var logs bytes.Buffer
	cmd := exec.Command(opencodeBin, "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port), "--print-logs")
	cmd.Dir = opencodeHome
	cmd.Env = append(os.Environ(), "HOME="+opencodeHome)
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start opencode server: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	waitForOpenCodeHealth(t, baseURL, waitCh, &logs)

	stopFn := func() {
		if cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)

		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitCh
		}
	}

	return baseURL, stopFn
}

func waitForOpenCodeHealth(t *testing.T, baseURL string, waitCh <-chan error, logs *bytes.Buffer) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(40 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case err := <-waitCh:
			t.Fatalf("opencode exited before becoming healthy: %v\nlogs:\n%s", err, logs.String())
		default:
		}

		resp, err := client.Get(baseURL + "/global/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for opencode health check at %s\nlogs:\n%s", baseURL, logs.String())
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate free port: %v", err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	if port == 0 {
		t.Fatal("allocated port is 0")
	}
	return port
}
