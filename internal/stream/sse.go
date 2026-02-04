package stream

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SSEEvent represents a Server-Sent Event
type SSEEvent struct {
	ID    string
	Event string
	Data  string
	Retry int
}

// SSEClient represents an SSE client
type SSEClient struct {
	client *http.Client
}

// NewSSEClient creates a new SSE client
func NewSSEClient(timeout time.Duration) *SSEClient {
	return &SSEClient{
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// Connect establishes an SSE connection to the given URL
func (c *SSEClient) Connect(ctx context.Context, url string) (*SSEConnection, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("SSE connection failed with status %d", resp.StatusCode)
	}

	conn := &SSEConnection{
		resp:   resp,
		reader: bufio.NewReader(resp.Body),
		events: make(chan SSEEvent),
		errors: make(chan error),
		done:   make(chan struct{}),
	}

	go conn.readEvents()

	return conn, nil
}

// SSEConnection represents an active SSE connection
type SSEConnection struct {
	resp   *http.Response
	reader *bufio.Reader
	events chan SSEEvent
	errors chan error
	done   chan struct{}
}

// Events returns a channel for receiving SSE events
func (c *SSEConnection) Events() <-chan SSEEvent {
	return c.events
}

// Errors returns a channel for receiving errors
func (c *SSEClient) Errors() <-chan error {
	return nil
}

// Close closes the SSE connection
func (c *SSEConnection) Close() error {
	close(c.done)
	return c.resp.Body.Close()
}

// readEvents reads events from the SSE stream
func (c *SSEConnection) readEvents() {
	defer close(c.events)
	defer close(c.errors)

	var event SSEEvent

	for {
		select {
		case <-c.done:
			return
		default:
			line, err := c.reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					c.errors <- err
				}
				return
			}

			line = strings.TrimSuffix(line, "\n")
			if strings.HasSuffix(line, "\r") {
				line = strings.TrimSuffix(line, "\r")
			}

			// Empty line indicates end of event
			if line == "" {
				if event.Data != "" {
					select {
					case c.events <- event:
						event = SSEEvent{}
					case <-c.done:
						return
					}
				}
				continue
			}

			// Parse SSE field
			if strings.HasPrefix(line, ":") {
				continue // Comment line
			}

			colonIndex := strings.Index(line, ":")
			if colonIndex == -1 {
				// Malformed line
				continue
			}

			field := line[:colonIndex]
			value := ""
			if colonIndex+1 < len(line) {
				if line[colonIndex+1] == ' ' {
					value = line[colonIndex+2:]
				} else {
					value = line[colonIndex+1:]
				}
			}

			switch field {
			case "id":
				event.ID = value
			case "event":
				event.Event = value
			case "data":
				if event.Data == "" {
					event.Data = value
				} else {
					event.Data += "\n" + value
				}
			case "retry":
				// Parse retry interval (not used currently)
			}
		}
	}
}

// StreamCallback is a function type for handling streamed data
type StreamCallback func(data string) error

// StreamMessage streams a message to OpenCode with SSE
func StreamMessage(ctx context.Context, client *http.Client, baseURL, sessionID, content string, callback StreamCallback) error {
	// Prepare request body
	reqBody := fmt.Sprintf(`{"content": %q}`, content)
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/session/"+sessionID+"/message", strings.NewReader(reqBody))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("stream request failed with status %d", resp.StatusCode)
	}

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
					if err := callback(eventData.String()); err != nil {
						return err
					}
					eventData.Reset()
				}
				continue
			}

			if strings.HasPrefix(line, "data: ") {
				eventData.WriteString(line[6:])
				eventData.WriteString("\n")
			}
		}
	}
}
