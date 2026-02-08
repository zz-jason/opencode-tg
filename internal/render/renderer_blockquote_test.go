package render

import (
	"testing"
)

func TestMarkdownToTelegramHTML_MultilineBlockquote(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "multiline blockquote",
			input:    "> line1\n> line2 \n> line3",
			expected: "<blockquote expandable=\"\">line1\nline2 \nline3</blockquote>",
		},
		{
			name:     "blockquote with empty lines",
			input:    "> line1\n> \n> line3",
			expected: "<blockquote expandable=\"\">line1\n\nline3</blockquote>",
		},
		{
			name:     "single line blockquote",
			input:    "> hello",
			expected: "<blockquote expandable=\"\">hello</blockquote>",
		},
		{
			name:     "blockquote with formatting",
			input:    "> **bold** and `code`",
			expected: "<blockquote expandable=\"\"><b>bold</b> and <code>code</code></blockquote>",
		},
		{
			name:     "mixed blockquote and regular lines",
			input:    "> quote line1\nregular line\n> quote line2",
			expected: "<blockquote expandable=\"\">quote line1</blockquote>\nregular line\n<blockquote expandable=\"\">quote line2</blockquote>",
		},
		{
			name:     "blockquote fenced code block",
			input:    "> ```bash\n> echo hi\n> ```",
			expected: "<blockquote expandable=\"\"><pre><code>echo hi</code></pre></blockquote>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MarkdownToTelegramHTML(tt.input)
			if result != tt.expected {
				t.Errorf("MarkdownToTelegramHTML(%q) = %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}
