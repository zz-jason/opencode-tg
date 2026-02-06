package handler

import (
	"strings"
	"sync"
	"testing"
	"tg-bot/internal/opencode"
	"time"
)

func TestFormatMessageParts(t *testing.T) {
	tests := []struct {
		name        string
		parts       []interface{}
		contains    []string // substrings that should be present
		notContains []string // substrings that should NOT be present
	}{
		{
			name:     "empty parts",
			parts:    []interface{}{},
			contains: []string{"No detailed content"},
		},
		{
			name: "text part only",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "text",
					Text: "Hello, world!",
				},
			},
			contains: []string{"• ✅ Reply content:", "Hello, world!"},
		},
		{
			name: "reasoning part",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "reasoning",
					Text: "I need to think about this carefully.",
				},
			},
			contains: []string{"• Thinking:", "I need to think about this carefully."},
		},
		{
			name: "reasoning part truncated",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "reasoning",
					Text: strings.Repeat("a", 2500),
				},
			},
			contains: []string{"• Thinking:", strings.Repeat("a", 2000) + "..."},
		},
		{
			name: "step-start part (should be skipped)",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "step-start",
					Text: "Task start",
				},
			},
			contains:    []string{"No detailed content"},
			notContains: []string{"Task start"},
		},
		{
			name: "step-finish part with reason",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:   "step-finish",
					Reason: "completed successfully",
					Cost:   0.1234,
				},
			},
			contains: []string{"No detailed content"},
		},
		{
			name: "tool call with snapshot containing name",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{"name": "bash", "status": "completed", "args": {"command": "ls -la"}}`,
				},
			},
			contains: []string{"• ✅ bash:", "executed"},
		},
		{
			name: "tool call with snapshot containing type",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{"type": "read", "result": "file content"}`,
				},
			},
			contains: []string{"• 🛠️ read:", "executed"},
		},
		{
			name: "tool call with snapshot containing tool field",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{"tool": "write", "output": "File written successfully"}`,
				},
			},
			contains: []string{"• 🛠️ write:", "executed"},
		},
		{
			name: "tool call with snapshot containing function field",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{"function": "edit", "error": "permission denied"}`,
				},
			},
			contains: []string{"• 🛠️ edit:", "executed"},
		},
		{
			name: "tool call with text content",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Text:     "Command executed successfully",
					Snapshot: `{"name": "bash", "args": {"command": "ls"}}`,
				},
			},
			contains: []string{"• 🛠️ bash:", "Command executed successfully"},
		},
		{
			name: "tool call with multiple arguments truncated",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{"name": "edit", "args": {"filePath": "/path/to/file", "oldString": "` + strings.Repeat("x", 200) + `", "newString": "updated", "another": "value", "more": "data", "extra": "field"}}`,
				},
			},
			contains: []string{"• 🛠️ edit:", "executed"},
		},
		{
			name: "tool call with malformed JSON snapshot",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{invalid json`,
					Text:     "Raw snapshot data",
				},
			},
			contains: []string{"• 🛠️ tool:", "Raw snapshot data"},
		},
		{
			name: "tool call with ID",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					ID:       "tool-call-12345678901234567890",
					Snapshot: `{"name": "read", "result": "content"}`,
				},
			},
			contains: []string{"• 🛠️ read:", "executed"},
		},
		{
			name: "tool call with reason",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Reason:   "execution completed",
					Snapshot: `{"name": "bash", "status": "success"}`,
				},
			},
			contains: []string{"• 🛠️ bash:", "executed"},
		},
		{
			name: "tool call with long result truncated",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{"name": "read", "result": "` + strings.Repeat("x", 400) + `"}`,
				},
			},
			contains: []string{"• 🛠️ read:", "executed"},
		},
		{
			name: "multiple parts",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "reasoning",
					Text: "First, I need to analyze.",
				},
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{"name": "read", "result": "file content"}`,
				},
				opencode.MessagePartResponse{
					Type: "text",
					Text: "Done!",
				},
			},
			contains: []string{"• Thinking:", "First, I need to analyze.", "• 🛠️ read:", "executed", "• ✅ Reply content:", "Done!"},
		},
		{
			name: "unknown part type",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "unknown-type",
					Text: "some data",
				},
			},
			contains: []string{"🔹 unknown-type"},
		},
		{
			name: "map representation fallback",
			parts: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": "Fallback text",
				},
			},
			contains: []string{"• ✅ Reply content:", "Fallback text"},
		},
		{
			name: "map representation reasoning",
			parts: []interface{}{
				map[string]interface{}{
					"type": "reasoning",
					"text": "Thinking...",
				},
			},
			contains: []string{"• Thinking:", "Thinking..."},
		},
		{
			name: "map representation tool",
			parts: []interface{}{
				map[string]interface{}{
					"type":     "tool",
					"text":     "Tool output",
					"snapshot": `{"name": "bash", "status": "done"}`,
				},
			},
			contains:    []string{"• 🛠️ bash:", "Tool output"},
			notContains: []string{"status: done"}, // Map fallback doesn't parse snapshot
		},
		{
			name: "map representation tool with command and output",
			parts: []interface{}{
				map[string]interface{}{
					"type": "tool",
					"tool": "bash",
					"state": map[string]interface{}{
						"status": "running",
						"input": map[string]interface{}{
							"command": "git diff origin/main HEAD",
						},
						"output": "# Compare HEAD and main\n$ git diff origin/main HEAD",
					},
				},
			},
			contains: []string{"• 🛠️ bash:", "$ git diff origin/main HEAD", "output:", "# Compare HEAD and main"},
		},
		{
			name: "tool call with empty snapshot",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: "",
					Text:     "Tool executed",
				},
			},
			contains: []string{"• 🛠️ tool:", "Tool executed"},
		},
		{
			name: "tool call with empty text",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type:     "tool",
					Snapshot: `{"name": "read"}`,
					Text:     "",
				},
			},
			contains: []string{"• 🛠️ read:", "executed"},
		},
		{
			name: "very long reasoning text",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "reasoning",
					Text: strings.Repeat("reasoning ", 500), // 5000 characters
				},
			},
			contains: []string{"• Thinking:", "reasoning", "..."},
		},
		{
			name: "mixed parts with step start and finish",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "step-start",
				},
				opencode.MessagePartResponse{
					Type: "reasoning",
					Text: "Thinking",
				},
				opencode.MessagePartResponse{
					Type:   "step-finish",
					Reason: "done",
				},
			},
			contains:    []string{"• Thinking:", "Thinking"},
			notContains: []string{"step-start"},
		},
		{
			name: "tool call with new Tool and State fields",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "tool",
					Tool: "bash",
					State: map[string]interface{}{
						"status": "completed",
						"input": map[string]interface{}{
							"command":     "ls -la",
							"description": "List files",
						},
						"output": "total 0\ndrwxr-xr-x ...",
					},
				},
			},
			contains: []string{"• ✅ bash:", "List files", "$ ls -la", "output:", "total 0"},
		},
		{
			name: "tool call with only Tool field",
			parts: []interface{}{
				opencode.MessagePartResponse{
					Type: "tool",
					Tool: "read",
				},
			},
			contains: []string{"• 🛠️ read:", "executed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatMessageParts(tt.parts)

			// Check for required substrings
			for _, substr := range tt.contains {
				if !strings.Contains(result, substr) {
					t.Errorf("formatMessageParts() missing substring %q\nGot: %s", substr, result)
				}
			}

			// Check for excluded substrings
			for _, substr := range tt.notContains {
				if strings.Contains(result, substr) {
					t.Errorf("formatMessageParts() contains unwanted substring %q\nGot: %s", substr, result)
				}
			}
		})
	}
}

func TestFormatMessageForDisplay_NoDuplicateReplyContentWhenMessageContentExists(t *testing.T) {
	b := &Bot{}
	msg := opencode.Message{
		Content: "Main assistant content",
		Parts: []interface{}{
			opencode.MessagePartResponse{
				Type: "text",
				Text: "Main assistant content",
			},
		},
	}

	got := b.formatMessageForDisplay(msg, false)
	if !strings.Contains(got, "Main assistant content") {
		t.Fatalf("expected main content to be displayed, got: %s", got)
	}
	if strings.Contains(got, "Reply content") {
		t.Fatalf("expected no duplicated reply-content block when content already exists, got: %s", got)
	}
}

func TestFormatMessageForDisplay_StillShowsToolDetailsWhenMessageContentExists(t *testing.T) {
	b := &Bot{}
	msg := opencode.Message{
		Content: "Main assistant content",
		Parts: []interface{}{
			opencode.MessagePartResponse{
				Type: "tool",
				Tool: "bash",
				State: map[string]interface{}{
					"status": "running",
					"input": map[string]interface{}{
						"command": "git diff origin/main HEAD",
					},
					"output": "diff --git a/file b/file",
				},
			},
		},
	}

	got := b.formatMessageForDisplay(msg, false)
	if !strings.Contains(got, "📋 Processing Details:") {
		t.Fatalf("expected processing details block, got: %s", got)
	}
	if !strings.Contains(got, "$ git diff origin/main HEAD") {
		t.Fatalf("expected tool command in details, got: %s", got)
	}
	if !strings.Contains(got, "output:") {
		t.Fatalf("expected tool output in details, got: %s", got)
	}
}

func TestStreamChunkDelta(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		chunk    string
		want     string
	}{
		{
			name:     "append when empty",
			existing: "",
			chunk:    "hello",
			want:     "hello",
		},
		{
			name:     "cumulative snapshot",
			existing: "hello",
			chunk:    "hello world",
			want:     " world",
		},
		{
			name:     "stale shorter snapshot",
			existing: "hello world",
			chunk:    "hello",
			want:     "",
		},
		{
			name:     "overlap suffix prefix",
			existing: "abc123",
			chunk:    "123XYZ",
			want:     "XYZ",
		},
		{
			name:     "repeated content",
			existing: "hello world",
			chunk:    "world",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := streamChunkDelta(tt.existing, tt.chunk)
			if got != tt.want {
				t.Fatalf("streamChunkDelta() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleStreamChunk_DeduplicatesCumulativeSnapshots(t *testing.T) {
	b := &Bot{}
	state := &streamingState{
		content:     &strings.Builder{},
		lastUpdate:  time.Now(),
		updateMutex: &sync.Mutex{},
	}

	if err := b.handleStreamChunk(state, "Integration tests validate "); err != nil {
		t.Fatalf("handleStreamChunk first call failed: %v", err)
	}
	if err := b.handleStreamChunk(state, "Integration tests validate tool progress "); err != nil {
		t.Fatalf("handleStreamChunk second call failed: %v", err)
	}
	if err := b.handleStreamChunk(state, "tool progress visibility."); err != nil {
		t.Fatalf("handleStreamChunk third call failed: %v", err)
	}

	got := state.content.String()
	want := "Integration tests validate tool progress visibility."
	if got != want {
		t.Fatalf("unexpected accumulated stream content\n got: %q\nwant: %q", got, want)
	}
}

func TestSplitLongContent_SplitsLongSingleLine(t *testing.T) {
	b := &Bot{}
	input := strings.Repeat("x", 7500)

	chunks := b.splitLongContent(input)
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(chunks))
	}

	for i, chunk := range chunks {
		if len(chunk) > 3500 {
			t.Fatalf("chunk %d exceeds limit: %d", i, len(chunk))
		}
	}

	if strings.Join(chunks, "") != input {
		t.Fatalf("split/join roundtrip mismatch")
	}
}

func TestSplitLongContent_LeadingNewlineNoDataLoss(t *testing.T) {
	b := &Bot{}
	input := "\n" + strings.Repeat("x", 5000)

	chunks := b.splitLongContent(input)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	if strings.Join(chunks, "") != input {
		t.Fatalf("split/join roundtrip mismatch for leading newline input")
	}
}

func TestFormatStreamingDisplays_LongSingleLineCreatesMultipleParts(t *testing.T) {
	b := &Bot{}
	content := strings.Repeat("a", 7600)

	displays := b.formatStreamingDisplays(content)
	if len(displays) < 3 {
		t.Fatalf("expected multiple streaming displays, got %d", len(displays))
	}

	if !strings.Contains(displays[1], "Part 2/") {
		t.Fatalf("expected second display to contain Part 2 header, got: %q", displays[1])
	}
	if !strings.Contains(displays[len(displays)-1], "▌") {
		t.Fatalf("expected last display to include streaming cursor, got: %q", displays[len(displays)-1])
	}
}
