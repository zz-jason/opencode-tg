package render

import (
	"strings"
	"testing"
)

func TestCodeSpanProtection(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Simple code", "`code`", "<code>code</code>"},
		{"Code with backticks", "`` `code` ``", "<code> `code` </code>"},
		{"Multiple backticks", "```code```", "<code>code</code>"},
		{"Code in text", "text `code` text", "text <code>code</code> text"},
		{"Multiple code spans", "`a` `b` `c`", "<code>a</code> <code>b</code> <code>c</code>"},
		{"Code with formatting", "`**bold**`", "<code>**bold**</code>"},
		{"Unclosed code", "`code", "`code"},
		{"Empty code", "``", "``"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MarkdownToTelegramHTML(tt.input)
			if !strings.Contains(result, tt.expected) {
				t.Errorf("MarkdownToTelegramHTML(%q) = %q, expected to contain %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestInputValidation(t *testing.T) {
	// Test large input handling
	largeInput := strings.Repeat("a", 200000)
	result := MarkdownToTelegramHTML(largeInput)

	// Should be truncated
	if len(result) <= 100000 {
		t.Errorf("Large input should be truncated, got length %d", len(result))
	}

	if !strings.Contains(result, "(truncated)") {
		t.Errorf("Truncated input should contain '(truncated)' marker")
	}
}

func TestSecurityFeatures(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		shouldContain    string
		shouldNotContain string
	}{
		{
			"XSS prevention",
			"<script>alert('xss')</script>",
			"&lt;script&gt;",
			"<script>",
		},
		{
			"HTML entity in code",
			"`<script>`",
			"&lt;script&gt;",
			"<script>",
		},
		{
			"Unsafe URL protocol",
			"[link](javascript:alert(1))",
			"[link](javascript:alert(1))",
			"<a href=\"javascript:",
		},
		{
			"Data URL prevention",
			"[link](data:text/html,<script>alert(1)</script>)",
			"[link](data:text/html,",
			"<a href=\"data:",
		},
		{
			"Tel protocol",
			"[call](tel:1234567890)",
			"[call](tel:1234567890)",
			"<a href=\"tel:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MarkdownToTelegramHTML(tt.input)

			if tt.shouldContain != "" && !strings.Contains(result, tt.shouldContain) {
				t.Errorf("MarkdownToTelegramHTML(%q) = %q, expected to contain %q", tt.input, result, tt.shouldContain)
			}

			if tt.shouldNotContain != "" && strings.Contains(result, tt.shouldNotContain) {
				t.Errorf("MarkdownToTelegramHTML(%q) = %q, should not contain %q", tt.input, result, tt.shouldNotContain)
			}
		})
	}
}

func TestEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// Nested formatting tests
		{"Bold in italic", "*italic **bold** italic*", "<i>italic <b>bold</b> italic</i>"},
		{"Italic in bold", "**bold *italic* bold**", "<b>bold <i>italic</i> bold</b>"},
		// Edge case tests
		{"Adjacent formatting", "**bold****bold**", "<b>bold</b><b>bold</b>"},
		{"Mixed formatting", "***bold italic*** text", "<b><i>bold italic</i></b> text"},
		// Special characters
		{"Special chars in code", "`a < b && c > d`", "<code>a &lt; b &amp;&amp; c &gt; d</code>"},
		// Newline handling
		{"Newlines in text", "line1\nline2\nline3", "line1\nline2\nline3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MarkdownToTelegramHTML(tt.input)
			if !strings.Contains(result, tt.expected) {
				t.Errorf("MarkdownToTelegramHTML(%q) = %q, expected to contain %q", tt.input, result, tt.expected)
				t.Logf("Full result: %q", result)
			}
		})
	}
}
