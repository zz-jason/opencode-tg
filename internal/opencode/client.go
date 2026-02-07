package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"tg-bot/internal/stream"
)

// Client represents an OpenCode API client
type Client struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
	stream  *stream.SSEClient
}

// NewClient creates a new OpenCode client
func NewClient(baseURL string, timeout int) *Client {
	// Always create a custom transport to bypass proxy for all requests
	// This ensures consistent behavior regardless of environment variables
	transport := &http.Transport{
		// Explicitly set Proxy to nil to disable proxy
		Proxy: nil,
		// Configure timeouts
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		// Increase MaxIdleConns for better performance
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
	}

	client := &http.Client{
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: transport,
	}

	return &Client{
		baseURL: baseURL,
		timeout: time.Duration(timeout) * time.Second,
		client:  client,
		stream:  stream.NewSSEClient(time.Duration(timeout) * time.Second),
	}
}

// Session represents an OpenCode session
type Session struct {
	ID        string                 `json:"id"`
	Slug      string                 `json:"slug"`
	Version   string                 `json:"version"`
	ProjectID string                 `json:"projectID"`
	Directory string                 `json:"directory"`
	Title     string                 `json:"title"`
	Time      SessionTime            `json:"time"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// SessionTime represents the time fields in a session
type SessionTime struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

// FileInfo represents a file or directory
type FileInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Absolute string `json:"absolute"`
	Type     string `json:"type"` // "file" or "directory"
	Ignored  bool   `json:"ignored"`
}

// Message represents a message in a session
type Message struct {
	ID         string                 `json:"id"`
	SessionID  string                 `json:"session_id"`
	Role       string                 `json:"role"` // "user", "assistant", "system"
	Content    string                 `json:"content"`
	Parts      []interface{}          `json:"parts,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
	Finish     string                 `json:"finish,omitempty"`
	ModelID    string                 `json:"model_id,omitempty"`
	ProviderID string                 `json:"provider_id,omitempty"`
}

// MessageResponse represents the actual API response for a message
type MessageResponse struct {
	Info  MessageInfo           `json:"info"`
	Parts []MessagePartResponse `json:"parts"`
}

// MessageInfo represents the info field in a message response
type MessageInfo struct {
	ID         string      `json:"id"`
	SessionID  string      `json:"sessionID"`
	Role       string      `json:"role"`
	Time       MessageTime `json:"time"`
	ParentID   string      `json:"parentID,omitempty"`
	ModelID    string      `json:"modelID,omitempty"`
	ProviderID string      `json:"providerID,omitempty"`
	Mode       string      `json:"mode,omitempty"`
	Agent      string      `json:"agent,omitempty"`
	Path       interface{} `json:"path,omitempty"`
	Cost       float64     `json:"cost,omitempty"`
	Tokens     interface{} `json:"tokens,omitempty"`
	Finish     string      `json:"finish,omitempty"`
	Summary    interface{} `json:"summary,omitempty"`
}

// MessageTime represents time fields in message info
type MessageTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed,omitempty"`
}

// MessagePartResponse represents a part in the message response
type MessagePartResponse struct {
	ID        string      `json:"id"`
	SessionID string      `json:"sessionID"`
	MessageID string      `json:"messageID"`
	Type      string      `json:"type"`
	Text      string      `json:"text,omitempty"`
	Time      interface{} `json:"time,omitempty"`
	Snapshot  string      `json:"snapshot,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	Cost      float64     `json:"cost,omitempty"`
	Tokens    interface{} `json:"tokens,omitempty"`
	CallID    string      `json:"callID,omitempty"`
	Tool      string      `json:"tool,omitempty"`
	State     interface{} `json:"state,omitempty"`
}

// CreateSessionRequest represents a request to create a session
type CreateSessionRequest struct {
	Title    string                 `json:"title,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// MessagePart represents a part of a message
type MessagePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// SendMessageRequest represents a request to send a message
type SendMessageRequest struct {
	Parts []MessagePart `json:"parts"`
}

// ErrorResponse represents an error response from OpenCode API
type ErrorResponse struct {
	Error string `json:"error"`
}

// Provider represents an AI provider
type Provider struct {
	ID      string           `json:"id"`
	Name    string           `json:"name"`
	Source  string           `json:"source"`
	Env     []string         `json:"env,omitempty"`
	Key     string           `json:"key,omitempty"`
	Options interface{}      `json:"options,omitempty"`
	Models  map[string]Model `json:"models"`
}

// Model represents an AI model
type Model struct {
	ID           string      `json:"id"`
	ProviderID   string      `json:"providerID"`
	Name         string      `json:"name"`
	Family       string      `json:"family"`
	Capabilities interface{} `json:"capabilities,omitempty"`
	Cost         interface{} `json:"cost,omitempty"`
	Limit        interface{} `json:"limit,omitempty"`
	Status       string      `json:"status,omitempty"`
	Options      interface{} `json:"options,omitempty"`
	ReleaseDate  string      `json:"release_date,omitempty"`
}

// ProvidersResponse represents the response from /provider endpoint
type ProvidersResponse struct {
	All       []Provider        `json:"all"`
	Default   map[string]string `json:"default"`
	Connected []string          `json:"connected"`
}

// ModelSelection represents a model selection
type ModelSelection struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// request makes an HTTP request to the OpenCode API
func (c *Client) request(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(jsonData)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	log.Debugf("Making %s request to %s", method, url)
	return c.client.Do(req)
}

// decodeResponse decodes the JSON response
func decodeResponse(resp *http.Response, v interface{}) error {
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil && errResp.Error != "" {
			return fmt.Errorf("API error (status %d): %s", resp.StatusCode, errResp.Error)
		}
		return fmt.Errorf("API request failed with status %d", resp.StatusCode)
	}

	if v != nil {
		return json.NewDecoder(resp.Body).Decode(v)
	}
	return nil
}

// CreateSession creates a new session
func (c *Client) CreateSession(ctx context.Context, req *CreateSessionRequest) (*Session, error) {
	resp, err := c.request(ctx, "POST", "/session", req)
	if err != nil {
		return nil, err
	}

	var session Session
	if err := decodeResponse(resp, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// ListSessions lists all sessions
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	resp, err := c.request(ctx, "GET", "/session", nil)
	if err != nil {
		return nil, err
	}

	var sessions []Session
	if err := decodeResponse(resp, &sessions); err != nil {
		return nil, err
	}

	return sessions, nil
}

// GetSession gets a session by ID
func (c *Client) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	resp, err := c.request(ctx, "GET", fmt.Sprintf("/session/%s", sessionID), nil)
	if err != nil {
		return nil, err
	}

	var session Session
	if err := decodeResponse(resp, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// SendMessage sends a message to a session and returns the response
func (c *Client) SendMessage(ctx context.Context, sessionID string, req *SendMessageRequest) (*Message, error) {
	resp, err := c.request(ctx, "POST", fmt.Sprintf("/session/%s/message", sessionID), req)
	if err != nil {
		return nil, err
	}

	var msgResp MessageResponse
	if err := decodeResponse(resp, &msgResp); err != nil {
		return nil, err
	}

	// Convert to Message
	message := &Message{
		ID:         msgResp.Info.ID,
		SessionID:  msgResp.Info.SessionID,
		Role:       msgResp.Info.Role,
		Parts:      make([]interface{}, len(msgResp.Parts)),
		Finish:     msgResp.Info.Finish,
		ModelID:    msgResp.Info.ModelID,
		ProviderID: msgResp.Info.ProviderID,
	}

	// Extract text content from parts and store all parts
	var content strings.Builder
	for j, part := range msgResp.Parts {
		// Store the part
		message.Parts[j] = part

		// Extract text content
		if part.Type == "text" && part.Text != "" {
			content.WriteString(part.Text)
			content.WriteString("\n")
		}
	}
	message.Content = strings.TrimSpace(content.String())

	// Set CreatedAt from time
	if msgResp.Info.Time.Created > 0 {
		message.CreatedAt = time.UnixMilli(msgResp.Info.Time.Created)
	}

	return message, nil
}

// GetMessages gets all messages in a session
func (c *Client) GetMessages(ctx context.Context, sessionID string) ([]Message, error) {
	resp, err := c.request(ctx, "GET", fmt.Sprintf("/session/%s/message", sessionID), nil)
	if err != nil {
		return nil, err
	}

	var msgResponses []MessageResponse
	if err := decodeResponse(resp, &msgResponses); err != nil {
		return nil, err
	}

	// Convert to Message slice
	messages := make([]Message, len(msgResponses))
	for i, msgResp := range msgResponses {
		msg := Message{
			ID:         msgResp.Info.ID,
			SessionID:  msgResp.Info.SessionID,
			Role:       msgResp.Info.Role,
			Parts:      make([]interface{}, len(msgResp.Parts)),
			Finish:     msgResp.Info.Finish,
			ModelID:    msgResp.Info.ModelID,
			ProviderID: msgResp.Info.ProviderID,
		}

		// Extract text content from parts and store all parts
		var content strings.Builder
		for j, part := range msgResp.Parts {
			// Store the part
			msg.Parts[j] = part

			// Extract text content
			if part.Type == "text" && part.Text != "" {
				content.WriteString(part.Text)
				content.WriteString("\n")
			}
		}
		msg.Content = strings.TrimSpace(content.String())

		// Set CreatedAt from time
		if msgResp.Info.Time.Created > 0 {
			msg.CreatedAt = time.UnixMilli(msgResp.Info.Time.Created)
		}

		messages[i] = msg
	}

	return messages, nil
}

// AbortSession aborts the current execution in a session
func (c *Client) AbortSession(ctx context.Context, sessionID string) error {
	resp, err := c.request(ctx, "POST", fmt.Sprintf("/session/%s/abort", sessionID), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to abort session: status %d", resp.StatusCode)
	}
	return nil
}

// HealthCheck checks if the OpenCode server is healthy
func (c *Client) HealthCheck(ctx context.Context) error {
	resp, err := c.request(ctx, "GET", "/global/health", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}
	return nil
}

// GetProviders gets all available AI providers and their models
func (c *Client) GetProviders(ctx context.Context) (*ProvidersResponse, error) {
	resp, err := c.request(ctx, "GET", "/provider", nil)
	if err != nil {
		return nil, err
	}

	var providersResp ProvidersResponse
	if err := decodeResponse(resp, &providersResp); err != nil {
		return nil, err
	}

	return &providersResp, nil
}

// GetModels gets all available models across all providers
func (c *Client) GetModels(ctx context.Context) ([]Model, error) {
	providersResp, err := c.GetProviders(ctx)
	if err != nil {
		return nil, err
	}

	var models []Model
	for _, provider := range providersResp.All {
		for _, model := range provider.Models {
			// Set provider ID if not already set
			if model.ProviderID == "" {
				model.ProviderID = provider.ID
			}
			models = append(models, model)
		}
	}

	return models, nil
}

// generateMessageID generates a unique message ID starting with "msg"
func generateMessageID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return fmt.Sprintf("msg-%s", hex.EncodeToString(bytes))
}

// InitSessionWithModel initializes a session with a specific model
func (c *Client) InitSessionWithModel(ctx context.Context, sessionID string, providerID, modelID string) error {
	log.Debugf("InitSessionWithModel called for session %s with provider %s model %s", sessionID, providerID, modelID)
	messageID := generateMessageID()
	reqBody := map[string]interface{}{
		"modelID":    modelID,
		"providerID": providerID,
		"messageID":  messageID,
	}
	log.Debugf("Initializing session %s with model %s/%s (messageID: %s)", sessionID, providerID, modelID, messageID)

	// Log the full URL being called
	url := c.baseURL + fmt.Sprintf("/session/%s/init", sessionID)
	log.Debugf("Making POST request to: %s", url)
	log.Debugf("Request body: %+v", reqBody)

	startTime := time.Now()
	resp, err := c.request(ctx, "POST", fmt.Sprintf("/session/%s/init", sessionID), reqBody)
	elapsed := time.Since(startTime)

	if err != nil {
		log.Errorf("Failed to send init request for session %s after %v: %v", sessionID, elapsed, err)
		return err
	}
	defer resp.Body.Close()

	log.Debugf("Init request for session %s completed with status %d after %v", sessionID, resp.StatusCode, elapsed)

	if resp.StatusCode >= 400 {
		// Try to read error body for more information
		body, _ := io.ReadAll(resp.Body)
		if len(body) > 0 {
			log.Errorf("Init session failed with status %d after %v: %s", resp.StatusCode, elapsed, string(body))
			return fmt.Errorf("failed to initialize session with model: status %d: %s", resp.StatusCode, string(body))
		}
		log.Errorf("Init session failed with status %d after %v (no body)", resp.StatusCode, elapsed)
		return fmt.Errorf("failed to initialize session with model: status %d", resp.StatusCode)
	}

	// Read successful response body for debugging
	body, _ := io.ReadAll(resp.Body)
	if len(body) > 0 {
		log.Debugf("Init session successful response body: %s", string(body))
	}

	log.Infof("Successfully initialized session %s with model %s/%s after %v", sessionID, providerID, modelID, elapsed)
	return nil
}

// StreamMessage sends a message and streams the response
func (c *Client) StreamMessage(ctx context.Context, sessionID string, content string, callback func(string) error) error {
	log.Debugf("Starting stream message for session %s, content length: %d", sessionID, len(content))

	reqBody := SendMessageRequest{
		Parts: []MessagePart{
			{
				Type: "text",
				Text: content,
			},
		},
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		log.Errorf("Failed to marshal request body: %v", err)
		return err
	}

	url := c.baseURL + "/session/" + sessionID + "/message"
	log.Debugf("Streaming to URL: %s", url)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonData))
	if err != nil {
		log.Errorf("Failed to create request: %v", err)
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// Create a new client with the same transport but longer timeout for streaming
	// We need to clone the transport to adjust timeouts for streaming
	baseTransport, ok := c.client.Transport.(*http.Transport)
	var transport *http.Transport
	if ok && baseTransport != nil {
		// Clone the transport and adjust timeouts for streaming
		transport = baseTransport.Clone()
		transport.TLSHandshakeTimeout = 30 * time.Second
		// Increased from 60s to 300s to accommodate slow OpenCode server header responses
		transport.ResponseHeaderTimeout = 300 * time.Second
		transport.ExpectContinueTimeout = 10 * time.Second
	} else {
		// Fallback to default transport
		transport = &http.Transport{
			Proxy:               nil, // Disable proxy
			TLSHandshakeTimeout: 30 * time.Second,
			// Increased from 60s to 300s to accommodate slow OpenCode server header responses
			ResponseHeaderTimeout: 300 * time.Second,
			ExpectContinueTimeout: 10 * time.Second,
			IdleConnTimeout:       300 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
		}
	}

	streamClient := &http.Client{
		Transport: transport,
		Timeout:   1800 * time.Second, // 30 minutes for long-running streaming tasks
	}

	// Try the request with retries for temporary network errors
	var resp *http.Response
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Debugf("Making streaming request to OpenCode (attempt %d/%d, timeout: %v)", attempt, maxRetries, streamClient.Timeout)
		startTime := time.Now()
		resp, err = streamClient.Do(req)
		elapsed := time.Since(startTime)

		if err != nil {
			log.Errorf("Stream request failed after %v (attempt %d/%d): %v", elapsed, attempt, maxRetries, err)

			// Log warning for ContentLength mismatch errors (potential OpenCode server bug)
			errStr := err.Error()
			if strings.Contains(errStr, "ContentLength=") && strings.Contains(errStr, "Body length 0") {
				log.Warnf("OpenCode server returned mismatched Content-Length header (possible server bug)")
			}

			// Check if we should retry
			if attempt < maxRetries && isRetryableError(err) {
				log.Debugf("Error is retryable: %v", err)
				waitTime := time.Duration(attempt) * 2 * time.Second
				log.Debugf("Retrying in %v...", waitTime)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(waitTime):
					continue
				}
			}
			return err
		}

		log.Debugf("Stream request completed with status %d after %v (attempt %d/%d)", resp.StatusCode, elapsed, attempt, maxRetries)
		break
	}

	if resp == nil {
		return fmt.Errorf("failed to get response after %d attempts", maxRetries)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Try to read error body for more information
		body, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("stream request failed with status %d: %s", resp.StatusCode, string(body))
		log.Error(errMsg)
		return errors.New(errMsg)
	}

	// Check content type to determine response format
	contentType := resp.Header.Get("Content-Type")

	if strings.Contains(contentType, "text/event-stream") {
		// Read SSE stream
		reader := bufio.NewReader(resp.Body)
		var eventData strings.Builder

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				line, err := reader.ReadString('\n')
				if err != nil {
					if err == io.EOF {
						return nil
					}
					return err
				}

				line = strings.TrimSuffix(line, "\n")
				if strings.HasSuffix(line, "\r") {
					line = strings.TrimSuffix(line, "\r")
				}

				// Empty line indicates end of event
				if line == "" {
					if eventData.Len() > 0 {
						textChunks := extractTextChunksFromStreamEvent(eventData.String())
						for _, text := range textChunks {
							if err := callback(text); err != nil {
								return err
							}
						}
						eventData.Reset()
					}
					continue
				}

				if strings.HasPrefix(line, "data: ") {
					eventData.WriteString(line[6:])
				}
			}
		}
	} else {
		// Handle JSON response
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}

		var msgResp MessageResponse
		if err := json.Unmarshal(body, &msgResp); err != nil {
			// If we can't parse as JSON, send the raw response
			if err := callback(string(body)); err != nil {
				return err
			}
			return nil
		}

		// Extract text from parts and send to callback
		for _, part := range msgResp.Parts {
			if part.Type == "text" && part.Text != "" {
				if err := callback(part.Text); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func extractTextChunksFromStreamEvent(data string) []string {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil
	}

	var payload interface{}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return []string{data}
	}

	chunks := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)

	var visit func(interface{})
	visit = func(v interface{}) {
		switch value := v.(type) {
		case map[string]interface{}:
			// Common stream payload shape: {"parts":[...]}
			if parts, ok := value["parts"]; ok {
				visit(parts)
			}

			// Common delta payloads: {"type":"text","text":"..."} etc.
			if text, ok := value["text"].(string); ok {
				text = strings.TrimSpace(text)
				if text != "" {
					if _, exists := seen[text]; !exists {
						seen[text] = struct{}{}
						chunks = append(chunks, text)
					}
				}
			}
			if text, ok := value["delta"].(string); ok {
				text = strings.TrimSpace(text)
				if text != "" {
					if _, exists := seen[text]; !exists {
						seen[text] = struct{}{}
						chunks = append(chunks, text)
					}
				}
			}
			if text, ok := value["content"].(string); ok {
				text = strings.TrimSpace(text)
				if text != "" {
					if _, exists := seen[text]; !exists {
						seen[text] = struct{}{}
						chunks = append(chunks, text)
					}
				}
			}

			// Traverse nested objects/arrays to handle wrapped payloads.
			for _, nested := range value {
				visit(nested)
			}
		case []interface{}:
			for _, item := range value {
				visit(item)
			}
		}
	}

	visit(payload)
	if len(chunks) == 0 {
		return nil
	}
	return chunks
}

// isRetryableError checks if an error is retryable (e.g., network timeout, temporary network issue)
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	// Check for timeout errors
	if strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "Timeout") ||
		strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "network is unreachable") ||
		strings.Contains(errStr, "EOF") ||
		// ContentLength mismatch indicates OpenCode server bug (sends header but empty body)
		(strings.Contains(errStr, "ContentLength=") && strings.Contains(errStr, "Body length 0")) {
		return true
	}

	// Check for temporary network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Temporary() || netErr.Timeout()
	}

	return false
}

// SearchResult represents a search result
type SearchResult struct {
	Path    string  `json:"path"`
	Line    int     `json:"line"`
	Content string  `json:"content"`
	Score   float64 `json:"score,omitempty"`
}

// SymbolResult represents a symbol search result
type SymbolResult struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // function, class, variable, etc.
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Signature string `json:"signature,omitempty"`
}

// AgentInfo represents an AI agent
type AgentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CommandInfo represents a command
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Usage       string `json:"usage,omitempty"`
}

// SearchFiles searches for text in files
func (c *Client) SearchFiles(ctx context.Context, query string) ([]SearchResult, error) {
	return nil, errors.New("search not implemented: API endpoint not available")
}

// FindFile finds files by name pattern
func (c *Client) FindFile(ctx context.Context, pattern string) ([]FileInfo, error) {
	return nil, errors.New("find file not implemented: API endpoint not available")
}

// SearchSymbol searches for symbols in code
func (c *Client) SearchSymbol(ctx context.Context, symbol string) ([]SymbolResult, error) {
	return nil, errors.New("symbol search not implemented: API endpoint not available")
}

// ListAgents lists available AI agents
func (c *Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	return nil, errors.New("list agents not implemented: API endpoint not available")
}

// ListCommands lists available commands
func (c *Client) ListCommands(ctx context.Context) ([]CommandInfo, error) {
	return nil, errors.New("list commands not implemented: API endpoint not available")
}

// ListFiles lists files in a directory
func (c *Client) ListFiles(ctx context.Context, path string) ([]FileInfo, error) {
	urlStr := c.baseURL + "/file"
	if path != "" {
		urlStr += "?path=" + url.QueryEscape(path)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to list files: status %d", resp.StatusCode)
	}

	var files []FileInfo
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return nil, err
	}
	return files, nil
}

// RenameSession renames a session by updating its title and metadata
func (c *Client) RenameSession(ctx context.Context, sessionID string, newName string) error {
	// Prepare update request
	reqBody := map[string]interface{}{
		"title": newName,
		"metadata": map[string]interface{}{
			"session_name": newName,
		},
	}

	resp, err := c.request(ctx, "PUT", fmt.Sprintf("/session/%s", sessionID), reqBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to rename session: status %d", resp.StatusCode)
	}

	return nil
}

// DeleteSession deletes a session
func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	resp, err := c.request(ctx, "DELETE", fmt.Sprintf("/session/%s", sessionID), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 404 {
		// 404 means session not found, which is acceptable for delete operation
		return nil
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to delete session: status %d", resp.StatusCode)
	}

	return nil
}
