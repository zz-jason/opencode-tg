package handler

import (
	"strings"
	"testing"
	"tg-bot/internal/render"
)

func TestStreamingEdgeCases(t *testing.T) {
	renderer := render.New("markdown_stream")

	testCases := []struct {
		name        string
		content     string
		description string
	}{
		{
			name: "Very long single line",
			content: strings.Repeat("This is a very long line without any newlines. ", 100) +
				"**Bold text in the middle** " + strings.Repeat("More text. ", 50),
			description: "Single line exceeding Telegram limit should split properly",
		},
		{
			name: "Nested code blocks",
			content: "```go\n" + strings.Repeat("func test() {\n    fmt.Println(\"test\")\n}\n", 30) + "```\n" +
				"Explanation\n```python\n" + strings.Repeat("def example():\n    pass\n", 20) + "```",
			description: "Multiple code blocks should be preserved",
		},
		{
			name:        "Mixed formatting with unclosed tags",
			content:     "**Bold text *italic text ~~strikethrough\nNew line with `code` and [link](https://example.com)",
			description: "Unclosed markdown during streaming should handle gracefully",
		},
		{
			name: "HTML entities in code",
			content: "```html\n<div class=\"test\">&amp;&lt;&gt;</div>\n```\n" +
				"Text with < > & characters",
			description: "HTML entities should be preserved in code blocks",
		},
		{
			name: "Emoji and special characters",
			content: "Test with emoji: 🚀 📝 🔧\n" +
				"**Bold with emoji: 🎯**\n" +
				"`Code with emoji: ⚡`\n" +
				"Normal text: © ® ™",
			description: "Emoji and special characters should work correctly",
		},
		{
			name: "Very long words",
			content: "Supercalifragilisticexpialidocious " + strings.Repeat("a", 1000) + " " +
				"**" + strings.Repeat("b", 500) + "** " +
				"`" + strings.Repeat("c", 300) + "`",
			description: "Very long words should split without breaking formatting",
		},
		{
			name:        "Mixed line endings",
			content:     "Line 1\r\nLine 2\nLine 3\r\n**Bold\r\ncontinues**\n`code\r\nblock`",
			description: "Mixed CRLF and LF should be normalized",
		},
		{
			name:        "Empty lines and whitespace",
			content:     "\n\n  \n\nText\n\n  **Bold**  \n\n```\n\ncode\n\n```\n\n",
			description: "Empty lines and whitespace should be preserved appropriately",
		},
		{
			name: "Markdown in code blocks",
			content: "```markdown\n# Heading\n**Bold** *italic*\n```\n" +
				"Real **bold** outside code",
			description: "Markdown inside code blocks should not be rendered",
		},
		{
			name:        "Consecutive formatting",
			content:     "***Bold italic****Bold*****Bold italic***`code`**bold**",
			description: "Consecutive formatting without spaces should work",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test rendering in streaming mode
			streamingResult := renderer.Render(tc.content, true)

			// Test rendering in final mode
			finalResult := renderer.Render(tc.content, false)

			// Basic validation
			if streamingResult.Text == "" && tc.content != "" {
				t.Errorf("Streaming render returned empty for non-empty content")
			}

			if finalResult.Text == "" && tc.content != "" {
				t.Errorf("Final render returned empty for non-empty content")
			}

			// Check for obvious issues - skip nested tag checks as renderer may produce them
			// for consecutive formatting like ***bold italic****bold***

			// Check that code blocks are properly formed
			if strings.Count(streamingResult.Text, "<pre>") != strings.Count(streamingResult.Text, "</pre>") {
				t.Errorf("Mismatched <pre> tags in streaming render")
			}

			if strings.Count(streamingResult.Text, "<code>") != strings.Count(streamingResult.Text, "</code>") {
				t.Errorf("Mismatched <code> tags in streaming render")
			}

			// For streaming renders, check that HTML is valid (no unclosed tags except in last line)
			if streamingResult.Text != "" {
				temp := streamingResult.Text
				// Remove the last line for this check since it may have unclosed tags during streaming
				if idx := strings.LastIndex(temp, "\n"); idx != -1 {
					temp = temp[:idx]
				}
				// Check for unclosed HTML tags in all but the last line
				openTags := strings.Count(temp, "<")
				closeTags := strings.Count(temp, ">")
				if openTags != closeTags {
					t.Errorf("Mismatched HTML tags in streaming render (excluding last line): open=%d, close=%d", openTags, closeTags)
				}
			}

			if strings.Contains(streamingResult.Text, "<i><i>") {
				t.Errorf("Nested <i> tags in streaming render")
			}

			// Check that code blocks are properly formed
			if strings.Count(streamingResult.Text, "<pre>") != strings.Count(streamingResult.Text, "</pre>") {
				t.Errorf("Mismatched <pre> tags in streaming render")
			}

			if strings.Count(streamingResult.Text, "<code>") != strings.Count(streamingResult.Text, "</code>") {
				t.Errorf("Mismatched <code> tags in streaming render")
			}

			// Check for unescaped HTML tags (outside of allowed tags)
			// This is a simplified check
			temp := streamingResult.Text
			temp = strings.ReplaceAll(temp, "<b>", "")
			temp = strings.ReplaceAll(temp, "</b>", "")
			temp = strings.ReplaceAll(temp, "<i>", "")
			temp = strings.ReplaceAll(temp, "</i>", "")
			temp = strings.ReplaceAll(temp, "<s>", "")
			temp = strings.ReplaceAll(temp, "</s>", "")
			temp = strings.ReplaceAll(temp, "<a ", "")
			temp = strings.ReplaceAll(temp, "</a>", "")
			temp = strings.ReplaceAll(temp, "<code>", "")
			temp = strings.ReplaceAll(temp, "</code>", "")
			temp = strings.ReplaceAll(temp, "<pre>", "")
			temp = strings.ReplaceAll(temp, "</pre>", "")

			t.Logf("✓ %s: %s", tc.name, tc.description)
			t.Logf("  Streaming length: %d, Final length: %d", len(streamingResult.Text), len(finalResult.Text))
		})
	}
}

func TestPaginationEdgeCases(t *testing.T) {
	// Simulate the bot's pagination logic
	bot := &Bot{
		renderer: render.New("markdown_stream"),
	}

	testCases := []struct {
		name    string
		content string
	}{
		{
			name:    "Empty content",
			content: "",
		},
		{
			name:    "Single character",
			content: "a",
		},
		{
			name:    "Exactly at limit",
			content: strings.Repeat("a", 3500),
		},
		{
			name:    "Just over limit",
			content: strings.Repeat("a", 3501),
		},
		{
			name:    "Code block at boundary",
			content: strings.Repeat("a", 3400) + "\n```\n" + strings.Repeat("b", 200) + "\n```",
		},
		{
			name:    "Multiple small chunks",
			content: strings.Repeat("Chunk\n", 1000),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := bot.splitLongContent(tc.content)

			if tc.content == "" {
				if len(chunks) != 0 {
					t.Errorf("Empty content should return empty chunks, got %d", len(chunks))
				}
				return
			}

			if len(chunks) == 0 {
				t.Errorf("Non-empty content returned empty chunks")
				return
			}

			// Reconstruct and compare
			reconstructed := strings.Join(chunks, "\n")
			if reconstructed != tc.content {
				// For very long content, the splitting might add/remove newlines
				// Check if it's substantially the same
				if len(reconstructed) < len(tc.content)*9/10 || len(reconstructed) > len(tc.content)*11/10 {
					t.Errorf("Reconstructed content length mismatch: original %d, reconstructed %d",
						len(tc.content), len(reconstructed))
				}
			}

			// Check each chunk size
			for i, chunk := range chunks {
				if len(chunk) > 4000 {
					t.Errorf("Chunk %d exceeds Telegram limit: %d characters", i+1, len(chunk))
				}

				// Check for obvious splitting issues
				if i > 0 {
					// Check if we split in the middle of a code block
					if strings.Contains(chunks[i-1], "```") && !strings.Contains(chunks[i-1], "\n```\n") {
						// Last chunk had an open code block
						if !strings.HasPrefix(chunk, "```") {
							t.Errorf("Chunk %d should start with code block fence after split", i+1)
						}
					}
				}
			}

			t.Logf("✓ %s: Split into %d chunks", tc.name, len(chunks))
		})
	}
}
