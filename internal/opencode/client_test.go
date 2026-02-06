package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	client := NewClient("http://localhost:8080", 30)
	if client == nil {
		t.Fatal("NewClient should return a non-nil client")
	}
}

func TestCreateSession(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/session" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var req CreateSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Failed to decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		response := Session{
			ID:    "test-session-123",
			Slug:  "test-slug",
			Title: req.Title,
			Time: SessionTime{
				Created: time.Now().UnixMilli(),
				Updated: time.Now().UnixMilli(),
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create client with test server URL
	client := NewClient(server.URL, 5)

	// Test creating a session
	session, err := client.CreateSession(context.Background(), &CreateSessionRequest{
		Title: "Test Session",
	})

	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	if session.ID != "test-session-123" {
		t.Errorf("Expected session ID 'test-session-123', got %s", session.ID)
	}
	if session.Title != "Test Session" {
		t.Errorf("Expected session title 'Test Session', got %s", session.Title)
	}
}

func TestSendMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasPrefix(r.URL.Path, "/session/test-session/message") {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Failed to decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Extract text from parts
		text := "Test response"
		if len(req.Parts) > 0 {
			text = "Test response to: " + req.Parts[0].Text
		}

		response := MessageResponse{
			Info: MessageInfo{
				ID:        "test-msg-456",
				SessionID: "test-session",
				Role:      "assistant",
				Time: MessageTime{
					Created: time.Now().UnixMilli(),
				},
			},
			Parts: []MessagePartResponse{
				{
					ID:        "test-part-123",
					SessionID: "test-session",
					MessageID: "test-msg-456",
					Type:      "text",
					Text:      text,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(server.URL, 5)

	message, err := client.SendMessage(context.Background(), "test-session", &SendMessageRequest{
		Parts: []MessagePart{
			{
				Type: "text",
				Text: "Hello, world!",
			},
		},
	})

	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	if message.ID != "test-msg-456" {
		t.Errorf("Expected message ID 'test-msg-456', got %s", message.ID)
	}
	if !strings.Contains(message.Content, "Hello, world!") {
		t.Errorf("Expected response to contain 'Hello, world!', got %s", message.Content)
	}
}

func TestGetSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/session/test-session-123" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		response := Session{
			ID:    "test-session-123",
			Slug:  "test-slug",
			Title: "Test Session",
			Time: SessionTime{
				Created: time.Now().UnixMilli(),
				Updated: time.Now().UnixMilli(),
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(server.URL, 5)

	session, err := client.GetSession(context.Background(), "test-session-123")

	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}

	if session.ID != "test-session-123" {
		t.Errorf("Expected session ID 'test-session-123', got %s", session.ID)
	}
	if session.Title != "Test Session" {
		t.Errorf("Expected session title 'Test Session', got %s", session.Title)
	}
}

func TestHealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/global/health" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL, 5)

	err := client.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("Health check should pass, got error: %v", err)
	}
}

func TestHealthCheckFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(server.URL, 5)

	err := client.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("Health check should fail with 500 status")
	}

	expectedError := "health check failed: status 500"
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("Expected error to contain %q, got %v", expectedError, err)
	}
}

func TestAbortSession(t *testing.T) {
	aborted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/session/test-session/abort" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		aborted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL, 5)

	err := client.AbortSession(context.Background(), "test-session")
	if err != nil {
		t.Fatalf("Failed to abort session: %v", err)
	}

	if !aborted {
		t.Error("Session should have been aborted")
	}
}

func TestErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errorResp := ErrorResponse{
			Error: "Invalid request: missing parameters",
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResp)
	}))
	defer server.Close()

	client := NewClient(server.URL, 5)

	_, err := client.CreateSession(context.Background(), &CreateSessionRequest{})
	if err == nil {
		t.Fatal("Expected error for bad request")
	}

	expectedError := "API error (status 400): Invalid request: missing parameters"
	if err.Error() != expectedError {
		t.Errorf("Expected error %q, got %v", expectedError, err)
	}
}

func TestExtractTextChunksFromStreamEvent(t *testing.T) {
	tests := []struct {
		name string
		data string
		want []string
	}{
		{
			name: "parts with text",
			data: `{"parts":[{"type":"text","text":"hello"},{"type":"tool","text":"ignored"}]}`,
			want: []string{"hello", "ignored"},
		},
		{
			name: "delta payload",
			data: `{"type":"delta","delta":"partial chunk"}`,
			want: []string{"partial chunk"},
		},
		{
			name: "nested content",
			data: `{"event":{"content":"final content"}}`,
			want: []string{"final content"},
		},
		{
			name: "raw text fallback",
			data: `not-json-payload`,
			want: []string{"not-json-payload"},
		},
		{
			name: "empty payload",
			data: `   `,
			want: nil,
		},
		{
			name: "json event without text should be ignored",
			data: `{"event":"start"}`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTextChunksFromStreamEvent(tt.data)
			if len(got) != len(tt.want) {
				t.Fatalf("expected %d chunks, got %d (%v)", len(tt.want), len(got), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("expected chunk %d to be %q, got %q", i, tt.want[i], got[i])
				}
			}
		})
	}
}
