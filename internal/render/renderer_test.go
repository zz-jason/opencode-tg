package render

import (
	"strings"
	"testing"
)

func TestNormalizeMode(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: ModeMarkdownStream},
		{in: "plain", want: ModePlain},
		{in: "markdown_final", want: ModeMarkdownFinal},
		{in: "markdown_stream", want: ModeMarkdownStream},
		{in: "unknown", want: ModeMarkdownStream},
	}

	for _, tt := range tests {
		if got := NormalizeMode(tt.in); got != tt.want {
			t.Fatalf("NormalizeMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRenderer_RenderMode(t *testing.T) {
	r := New(ModePlain)
	plain := r.Render("**hello**", true)
	if plain.UseHTML {
		t.Fatalf("plain mode should not use HTML")
	}
	if plain.Text != "**hello**" {
		t.Fatalf("plain mode should keep raw text, got %q", plain.Text)
	}

	r = New(ModeMarkdownFinal)
	streaming := r.Render("**hello**", true)
	if streaming.UseHTML {
		t.Fatalf("markdown_final streaming path should not use HTML")
	}

	final := r.Render("**hello**", false)
	if !final.UseHTML {
		t.Fatalf("markdown_final final path should use HTML")
	}
	if !strings.Contains(final.Text, "<b>hello</b>") {
		t.Fatalf("expected bold conversion, got %q", final.Text)
	}

	r = New(ModeMarkdownStream)
	stream := r.Render("**hello**", true)
	if !stream.UseHTML {
		t.Fatalf("markdown_stream should use HTML while streaming")
	}
}

func TestMarkdownToTelegramHTML(t *testing.T) {
	input := "# Title\nA **bold** and ~~strike~~ text with [link](https://example.com?q=1&k=2)\n`code`\n***both***"
	got := MarkdownToTelegramHTML(input)

	expectContains := []string{
		"<b>Title</b>", // Heading should be bold without emoji prefix
		"<b>bold</b>",
		"<s>strike</s>",
		`<a href="https://example.com?q=1&amp;k=2">link</a>`,
		"<code>code</code>",
		"<b><i>both</i></b>",
	}
	for _, want := range expectContains {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
}

func TestMarkdownToTelegramHTML_FencePreserved(t *testing.T) {
	input := "```go\nfmt.Println(\"hi\")\n```"
	got := MarkdownToTelegramHTML(input)

	if strings.Contains(got, "<b>") || strings.Contains(got, "<a ") {
		t.Fatalf("fenced content should not be transformed, got %q", got)
	}
	if !strings.Contains(got, "<pre><code>fmt.Println(&#34;hi&#34;)</code></pre>") {
		t.Fatalf("expected code fence rendered as pre/code, got %q", got)
	}
}

func TestMarkdownToTelegramHTML_UnclosedFenceKeptRaw(t *testing.T) {
	input := "```go\nfmt.Println(\"hi\")"
	got := MarkdownToTelegramHTML(input)

	if !strings.Contains(got, "```go") {
		t.Fatalf("expected unclosed fence marker to stay raw, got %q", got)
	}
}
