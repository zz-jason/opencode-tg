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
	t                    *testing.T
	messages             chan string
	mu                   sync.Mutex
	nextID               int
	messageTextByID      map[int]string
	sendCount            int
	editCount            int
	notModifiedEditCount int
	failOnNotModified    bool
	failOnHTMLParse      bool
	htmlParseErrorCount  int
}

func newTelegramRecorder(t *testing.T) *telegramRecorder {
	return &telegramRecorder{
		t:               t,
		messages:        make(chan string, 64),
		nextID:          1,
		messageTextByID: make(map[int]string),
	}
}

func (r *telegramRecorder) serveHTTP(w http.ResponseWriter, req *http.Request) {
	method := pathMethod(req.URL.Path)
	switch method {
	case "sendMessage", "editMessageText":
		r.handleSendLike(w, req, method)
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

func (r *telegramRecorder) handleSendLike(w http.ResponseWriter, req *http.Request, method string) {
	var payload map[string]interface{}
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		r.t.Fatalf("failed to decode telegram request body: %v", err)
	}

	text, _ := payload["text"].(string)
	parseMode, _ := payload["parse_mode"].(string)
	chatID := parseChatID(payload["chat_id"])

	r.mu.Lock()
	var msgID int
	switch method {
	case "sendMessage":
		msgID = r.nextID
		r.nextID++
		r.sendCount++
	case "editMessageText":
		msgID = parseMessageID(payload["message_id"])
		if msgID == 0 {
			msgID = r.nextID
			r.nextID++
		}
		r.editCount++
		if r.failOnNotModified {
			if prev, ok := r.messageTextByID[msgID]; ok && prev == text {
				r.notModifiedEditCount++
				r.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"ok":          false,
					"error_code":  400,
					"description": "Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message",
				})
				return
			}
		}
	default:
		msgID = r.nextID
		r.nextID++
	}
	if r.failOnHTMLParse && parseMode == "HTML" {
		r.htmlParseErrorCount++
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":          false,
			"error_code":  400,
			"description": "Bad Request: can't parse entities: unsupported start tag",
		})
		return
	}
	r.messageTextByID[msgID] = text
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

func (r *telegramRecorder) enableNotModifiedEditFailure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOnNotModified = true
}

func (r *telegramRecorder) counts() (sendCount, editCount, notModifiedEditCount int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendCount, r.editCount, r.notModifiedEditCount
}

func (r *telegramRecorder) enableHTMLParseFailure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOnHTMLParse = true
}

func (r *telegramRecorder) htmlParseFailures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.htmlParseErrorCount
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

func parseMessageID(raw interface{}) int {
	switch v := raw.(type) {
	case string:
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	case float64:
		return int(v)
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

func waitForTelegramMessageWithPrefix(t *testing.T, ch <-chan string, prefix string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		select {
		case msg := <-ch:
			if strings.HasPrefix(msg, prefix) {
				return msg
			}
		case <-time.After(minDuration(remaining, 500*time.Millisecond)):
		}
	}

	t.Fatalf("timed out waiting for telegram message with prefix %q", prefix)
	return ""
}

func waitForTelegramMessageContaining(t *testing.T, ch <-chan string, needle string, timeout time.Duration) (string, time.Time) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		select {
		case msg := <-ch:
			if strings.Contains(msg, needle) {
				return msg, time.Now()
			}
		case <-time.After(minDuration(remaining, 500*time.Millisecond)):
		}
	}

	t.Fatalf("timed out waiting for telegram message containing %q", needle)
	return "", time.Time{}
}

func assertNoTelegramMessageContaining(t *testing.T, ch <-chan string, needle string, duration time.Duration) {
	t.Helper()

	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		select {
		case msg := <-ch:
			if strings.Contains(msg, needle) {
				t.Fatalf("unexpected telegram message containing %q: %s", needle, msg)
			}
		case <-time.After(minDuration(remaining, 300*time.Millisecond)):
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
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
			FilePath: filepath.Join(tmpDir, "bot-state.json"),
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

func TestIntegration_HandleCoreCommandsTextFallback(t *testing.T) {
	t.Helper()

	var (
		mu             sync.Mutex
		sessionID      = "session-fallback-1"
		messageReadyAt time.Time
	)

	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return

		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return

		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return

		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return

		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			// Simulate stream events with no text payload.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"event\":\"start\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}

			mu.Lock()
			messageReadyAt = time.Now().Add(1200 * time.Millisecond)
			mu.Unlock()
			return

		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			mu.Lock()
			ready := !messageReadyAt.IsZero() && time.Now().After(messageReadyAt)
			mu.Unlock()

			if !ready {
				_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
				return
			}

			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"info": map[string]interface{}{
						"id":        "msg-assistant-1",
						"sessionID": sessionID,
						"role":      "assistant",
						"time": map[string]interface{}{
							"created": time.Now().UnixMilli(),
						},
						"finish": "stop",
					},
					"parts": []map[string]interface{}{
						{
							"id":        "part-1",
							"sessionID": sessionID,
							"messageID": "msg-assistant-1",
							"type":      "text",
							"text":      "Fallback assistant reply",
						},
					},
				},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 20002
	tgBot.ProcessUpdate(telebot.Update{
		ID: 101,
		Message: &telebot.Message{
			ID:       101,
			Text:     "please solve this task",
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

	finalMsg, _ := waitForTelegramMessageContaining(t, rec.messages, "Fallback assistant reply", 20*time.Second)
	if strings.Contains(finalMsg, "Response completed with no content") {
		t.Fatalf("unexpected empty response fallback: %s", finalMsg)
	}
	assertContains(t, finalMsg, "Fallback assistant reply")
}

func TestIntegration_HandleCoreCommandsRealtimeUpdate(t *testing.T) {
	t.Helper()

	var (
		mu             sync.Mutex
		sessionID      = "session-realtime-1"
		messageReadyAt time.Time
	)

	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return

		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return

		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return

		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return

		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"event\":\"start\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}

			mu.Lock()
			messageReadyAt = time.Now().Add(1200 * time.Millisecond)
			mu.Unlock()

			// Keep stream alive for a while to verify realtime polling updates
			// can update Telegram message before stream completion.
			time.Sleep(4 * time.Second)
			return

		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			mu.Lock()
			ready := !messageReadyAt.IsZero() && time.Now().After(messageReadyAt)
			mu.Unlock()

			if !ready {
				_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
				return
			}

			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"info": map[string]interface{}{
						"id":        "msg-assistant-realtime",
						"sessionID": sessionID,
						"role":      "assistant",
						"time": map[string]interface{}{
							"created": time.Now().UnixMilli(),
						},
					},
					"parts": []map[string]interface{}{
						{
							"id":        "part-realtime",
							"sessionID": sessionID,
							"messageID": "msg-assistant-realtime",
							"type":      "text",
							"text":      "Realtime assistant reply",
						},
					},
				},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 30003
	started := time.Now()
	go tgBot.ProcessUpdate(telebot.Update{
		ID: 201,
		Message: &telebot.Message{
			ID:       201,
			Text:     "run realtime task",
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

	// first message is initial processing indicator
	_ = waitForTelegramMessage(t, rec.messages)

	realtimeMsg, realtimeAt := waitForTelegramMessageContaining(t, rec.messages, "Realtime assistant reply", 6*time.Second)
	elapsed := realtimeAt.Sub(started)
	if elapsed >= 4*time.Second {
		t.Fatalf("expected realtime update before stream completion, got elapsed=%v message=%q", elapsed, realtimeMsg)
	}
	if strings.HasPrefix(realtimeMsg, "✅") {
		t.Fatalf("expected intermediate realtime update, got final-style message: %q", realtimeMsg)
	}

	// Finalization may reuse the same rendered text as the last streaming update,
	// so there may be no additional Telegram edit after completion.
	assertContains(t, realtimeMsg, "Realtime assistant reply")

	// Ensure periodic updater does not overwrite final content with stale auto-updating text.
	assertNoTelegramMessageContaining(t, rec.messages, "⏳ Auto-updating...", 3*time.Second)
}

func TestIntegration_HandleCoreCommandsCumulativeStreamNoDup(t *testing.T) {
	t.Helper()

	const sessionID = "session-cumulative-stream-1"
	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return
		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return
		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)

			events := []string{
				`data: {"parts":[{"type":"text","text":"Alpha "}]}` + "\n\n",
				`data: {"parts":[{"type":"text","text":"Alpha Beta "}]}` + "\n\n",
				`data: {"parts":[{"type":"text","text":"Alpha Beta Gamma"}]}` + "\n\n",
			}
			for _, evt := range events {
				_, _ = io.WriteString(w, evt)
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(150 * time.Millisecond)
			}
			return
		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 30005
	tgBot.ProcessUpdate(telebot.Update{
		ID: 401,
		Message: &telebot.Message{
			ID:       401,
			Text:     "trigger cumulative stream",
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

	finalMsg, _ := waitForTelegramMessageContaining(t, rec.messages, "Alpha Beta Gamma", 10*time.Second)
	assertContains(t, finalMsg, "Alpha Beta Gamma")
	if strings.Contains(finalMsg, "Alpha Alpha") {
		t.Fatalf("unexpected duplicated cumulative stream content: %q", finalMsg)
	}
}

func TestIntegration_HandleCoreCommandsMarkdownStreamRenderedHTML(t *testing.T) {
	t.Helper()

	const sessionID = "session-markdown-render-html-1"
	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return
		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return
		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			markdown := "**bold**\n```go\nfmt.Println(\"hi\")\n```"
			_, _ = io.WriteString(w, fmt.Sprintf("data: {\"parts\":[{\"type\":\"text\",\"text\":%q}]}\n\n", markdown))
			if flusher != nil {
				flusher.Flush()
			}
			return
		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 30009
	tgBot.ProcessUpdate(telebot.Update{
		ID: 801,
		Message: &telebot.Message{
			ID:       801,
			Text:     "render markdown now",
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

	finalMsg, _ := waitForTelegramMessageContaining(t, rec.messages, "<b>bold</b>", 10*time.Second)
	if !strings.Contains(finalMsg, "<b>bold</b>") {
		t.Fatalf("expected markdown bold rendered to HTML tag, got: %q", finalMsg)
	}
	if !strings.Contains(finalMsg, "<pre><code>fmt.Println(") {
		t.Fatalf("expected fenced code rendered as pre/code, got: %q", finalMsg)
	}
	if strings.Contains(finalMsg, "**bold**") || strings.Contains(finalMsg, "```go") {
		t.Fatalf("expected markdown syntax removed from final HTML-rendered message, got: %q", finalMsg)
	}
}

func TestIntegration_HandleCoreCommandsMarkdownRenderFallbackToPlainOnParseError(t *testing.T) {
	t.Helper()

	const sessionID = "session-markdown-render-fallback-1"
	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return
		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return
		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, `data: {"parts":[{"type":"text","text":"**bold**"}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	rec.enableHTMLParseFailure()
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 30008
	tgBot.ProcessUpdate(telebot.Update{
		ID: 701,
		Message: &telebot.Message{
			ID:       701,
			Text:     "show markdown fallback",
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

	finalMsg, _ := waitForTelegramMessageContaining(t, rec.messages, "**bold**", 10*time.Second)
	if !strings.Contains(finalMsg, "**bold**") {
		t.Fatalf("expected plain-text markdown fallback, got: %q", finalMsg)
	}
	if strings.Contains(finalMsg, "<b>") {
		t.Fatalf("expected fallback plain text without HTML tags, got: %q", finalMsg)
	}
	if rec.htmlParseFailures() == 0 {
		t.Fatalf("expected simulated HTML parse failure to be triggered")
	}
}

func TestIntegration_HandleCoreCommandsCodeFenceStreamingAcrossPagesRendered(t *testing.T) {
	t.Helper()

	const sessionID = "session-code-fence-paging-render-1"
	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return
		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return
		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)

			code1 := "```bash\n" + strings.Repeat("echo one\n", 140) + "```"
			code2 := "```bash\n" + strings.Repeat("echo one\n", 420) + "```"
			events := []string{
				fmt.Sprintf("data: {\"parts\":[{\"type\":\"text\",\"text\":%q}]}\n\n", code1),
				fmt.Sprintf("data: {\"parts\":[{\"type\":\"text\",\"text\":%q}]}\n\n", code2),
			}

			for i, evt := range events {
				_, _ = io.WriteString(w, evt)
				if flusher != nil {
					flusher.Flush()
				}
				if i == 0 {
					time.Sleep(700 * time.Millisecond)
				}
			}
			time.Sleep(2 * time.Second)
			return
		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 30010
	tgBot.ProcessUpdate(telebot.Update{
		ID: 901,
		Message: &telebot.Message{
			ID:       901,
			Text:     "stream long code block",
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

	_ = waitForTelegramMessage(t, rec.messages) // initial processing message

	// Wait for second message (should contain code block)
	part2Msg, _ := waitForTelegramMessageContaining(t, rec.messages, "<pre><code>", 10*time.Second)
	if strings.HasPrefix(part2Msg, "✅") {
		t.Fatalf("expected streaming-time second message, got final-style message: %q", part2Msg)
	}
	if !strings.Contains(part2Msg, "<pre><code>") {
		t.Fatalf("expected code fence to render as pre/code on second message, got: %q", part2Msg)
	}
	if strings.Contains(part2Msg, "```") {
		t.Fatalf("expected no raw markdown fence markers in rendered second message, got: %q", part2Msg)
	}
}

func TestIntegration_HandleCoreCommandsStreamingStartsSecondMessageBeforeCompletion(t *testing.T) {
	t.Helper()

	const sessionID = "session-stream-partition-1"
	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return
		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return
		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)

			// Emit cumulative snapshots without newlines to ensure long single-line
			// content still triggers multiple messages before completion.
			base := strings.Repeat("A", 2400)
			mid := base + strings.Repeat("B", 2300)
			long := mid + strings.Repeat("C", 2800)
			events := []string{
				fmt.Sprintf("data: {\"parts\":[{\"type\":\"text\",\"text\":%q}]}\n\n", base),
				fmt.Sprintf("data: {\"parts\":[{\"type\":\"text\",\"text\":%q}]}\n\n", mid),
				fmt.Sprintf("data: {\"parts\":[{\"type\":\"text\",\"text\":%q}]}\n\n", long),
			}

			for i, evt := range events {
				_, _ = io.WriteString(w, evt)
				if flusher != nil {
					flusher.Flush()
				}
				// Keep stream open after crossing first-page size to verify second message appears before completion.
				if i == len(events)-1 {
					time.Sleep(2 * time.Second)
				} else {
					time.Sleep(700 * time.Millisecond)
				}
			}
			return
		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 30006
	started := time.Now()
	go tgBot.ProcessUpdate(telebot.Update{
		ID: 501,
		Message: &telebot.Message{
			ID:       501,
			Text:     "trigger multi message stream",
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

	_ = waitForTelegramMessage(t, rec.messages) // initial processing message

	// Wait for second message (should contain content)
	part2Msg, part2At := waitForTelegramMessageContaining(t, rec.messages, "", 8*time.Second)
	if strings.HasPrefix(part2Msg, "✅") {
		t.Fatalf("expected streaming-time second message, got final-style message: %q", part2Msg)
	}
	if part2At.Sub(started) >= 4*time.Second {
		t.Fatalf("expected second message to appear before stream completion, elapsed=%v message=%q", part2At.Sub(started), part2Msg)
	}

	// Wait for third message
	part3Msg, part3At := waitForTelegramMessageContaining(t, rec.messages, "", 8*time.Second)
	if strings.HasPrefix(part3Msg, "✅") {
		t.Fatalf("expected streaming-time third message, got final-style message: %q", part3Msg)
	}
	if part3At.Sub(started) >= 4*time.Second {
		t.Fatalf("expected third message to appear before stream completion, elapsed=%v message=%q", part3At.Sub(started), part3Msg)
	}

	// Wait for final message (no completion notice expected)
	finalMsg, _ := waitForTelegramMessageContaining(t, rec.messages, "", 8*time.Second)
	// No completion notice should be added to the content
	if strings.Contains(finalMsg, "--- ✅ Task completed ---") {
		t.Fatalf("unexpected completion notice in final message, got: %q", finalMsg)
	}
}

func TestIntegration_HandleCoreCommandsPeriodicFallbackStreamsPart2BeforeCompletion(t *testing.T) {
	t.Helper()

	const sessionID = "session-periodic-fallback-paging-1"
	var streamStartedAt time.Time

	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return
		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return
		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)

			streamStartedAt = time.Now()
			base := strings.Repeat("A", 2200)
			_, _ = io.WriteString(w, fmt.Sprintf("data: {\"parts\":[{\"type\":\"text\",\"text\":%q}]}\n\n", base))
			if flusher != nil {
				flusher.Flush()
			}

			// Keep stream open long enough for periodic polling updates to kick in.
			time.Sleep(9 * time.Second)
			return
		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			if streamStartedAt.IsZero() {
				_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
				return
			}

			elapsed := time.Since(streamStartedAt)
			content := strings.Repeat("A", 2200)
			switch {
			case elapsed >= 6*time.Second:
				content += strings.Repeat("B", 2300) + strings.Repeat("C", 2200)
			case elapsed >= 3*time.Second:
				content += strings.Repeat("B", 2300)
			}

			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"content": content,
					"info": map[string]interface{}{
						"id":        "msg-assistant-periodic-paging",
						"sessionID": sessionID,
						"role":      "assistant",
						"time": map[string]interface{}{
							"created": time.Now().UnixMilli(),
						},
					},
					"parts": []map[string]interface{}{
						{
							"id":        "part-periodic-text",
							"sessionID": sessionID,
							"messageID": "msg-assistant-periodic-paging",
							"type":      "text",
							"text":      content,
						},
					},
				},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 30007
	started := time.Now()
	go tgBot.ProcessUpdate(telebot.Update{
		ID: 601,
		Message: &telebot.Message{
			ID:       601,
			Text:     "trigger periodic paging fallback",
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

	_ = waitForTelegramMessage(t, rec.messages) // initial processing message

	// Wait for second message
	part2Msg, part2At := waitForTelegramMessageContaining(t, rec.messages, "", 10*time.Second)
	if strings.HasPrefix(part2Msg, "✅") {
		t.Fatalf("expected streaming-time second message, got final-style message: %q", part2Msg)
	}
	if part2At.Sub(started) >= 8*time.Second {
		t.Fatalf("expected second message to appear before stream completion, elapsed=%v message=%q", part2At.Sub(started), part2Msg)
	}

	// Wait for final message (no completion notice expected)
	finalMsg, _ := waitForTelegramMessageContaining(t, rec.messages, "", 12*time.Second)
	// No completion notice should be added to the content
	if strings.Contains(finalMsg, "--- ✅ Task completed ---") {
		t.Fatalf("unexpected completion notice in final message, got: %q", finalMsg)
	}
}

func TestIntegration_HandleCoreCommandsRealtimeToolProgressSameMessageID(t *testing.T) {
	t.Helper()

	var (
		mu           sync.Mutex
		sessionID    = "session-tool-progress-1"
		phaseOneAt   time.Time
		phaseTwoAt   time.Time
		streamEndsAt time.Time
	)

	opencodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/global/health":
			w.WriteHeader(http.StatusOK)
			return

		case r.Method == "GET" && r.URL.Path == "/provider":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"all": []map[string]interface{}{},
			})
			return

		case r.Method == "GET" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return

		case r.Method == "POST" && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    sessionID,
				"title": "Telegram Session",
				"time": map[string]interface{}{
					"created": time.Now().UnixMilli(),
					"updated": time.Now().UnixMilli(),
				},
			})
			return

		case r.Method == "POST" && r.URL.Path == "/session/"+sessionID+"/message":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"event\":\"start\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}

			mu.Lock()
			now := time.Now()
			phaseOneAt = now.Add(400 * time.Millisecond)
			phaseTwoAt = now.Add(1400 * time.Millisecond)
			streamEndsAt = now.Add(4 * time.Second)
			mu.Unlock()

			time.Sleep(4 * time.Second)
			return

		case r.Method == "GET" && r.URL.Path == "/session/"+sessionID+"/message":
			mu.Lock()
			p1 := phaseOneAt
			p2 := phaseTwoAt
			ends := streamEndsAt
			now := time.Now()
			mu.Unlock()

			if p1.IsZero() || now.Before(p1) {
				_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
				return
			}

			assistant := map[string]interface{}{
				"info": map[string]interface{}{
					"id":        "msg-assistant-tool-progress",
					"sessionID": sessionID,
					"role":      "assistant",
					"time": map[string]interface{}{
						"created": now.UnixMilli(),
					},
				},
			}

			// Same message ID, but parts become available later while streaming is still running.
			if now.Before(p2) {
				assistant["parts"] = []map[string]interface{}{}
				_ = json.NewEncoder(w).Encode([]map[string]interface{}{assistant})
				return
			}

			toolPart := map[string]interface{}{
				"id":        "part-tool-progress-1",
				"sessionID": sessionID,
				"messageID": "msg-assistant-tool-progress",
				"type":      "tool",
				"tool":      "bash",
				"state": map[string]interface{}{
					"status": "running",
					"input": map[string]interface{}{
						"command": "git diff origin/main HEAD",
					},
				},
			}
			assistant["parts"] = []map[string]interface{}{toolPart}
			if !ends.IsZero() && now.After(ends) {
				assistantInfo := assistant["info"].(map[string]interface{})
				assistantInfo["finish"] = "stop"
			}

			_ = json.NewEncoder(w).Encode([]map[string]interface{}{assistant})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer opencodeServer.Close()

	rec := newTelegramRecorder(t)
	tgServer := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	defer tgServer.Close()

	tgBot, err := telebot.NewBot(telebot.Settings{
		Token:       "integration-token",
		URL:         tgServer.URL,
		Client:      tgServer.Client(),
		Offline:     true,
		Synchronous: true,
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
		OpenCode: config.OpenCodeConfig{
			URL:     opencodeServer.URL,
			Timeout: 30,
		},
		Storage: config.StorageConfig{
			Type:     "file",
			FilePath: filepath.Join(t.TempDir(), "bot-state.json"),
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

	const userID int64 = 30004
	started := time.Now()
	go tgBot.ProcessUpdate(telebot.Update{
		ID: 301,
		Message: &telebot.Message{
			ID:       301,
			Text:     "show me tool progress",
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

	// Consume initial "processing" message.
	_ = waitForTelegramMessage(t, rec.messages)

	toolMsg, toolAt := waitForTelegramMessageContaining(t, rec.messages, "git diff origin/main HEAD", 8*time.Second)
	if toolAt.Sub(started) >= 4*time.Second {
		t.Fatalf("expected tool progress before stream completion, got elapsed=%v message=%q", toolAt.Sub(started), toolMsg)
	}
	if strings.HasPrefix(toolMsg, "✅") {
		t.Fatalf("expected intermediate tool progress update, got final-style message: %q", toolMsg)
	}
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
