package render

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	ModeMarkdownStream = "markdown_stream"
)

// Result describes how a message should be sent to Telegram.
type Result struct {
	Text         string
	FallbackText string
	UseHTML      bool
}

// Renderer formats OpenCode output for Telegram.
type Renderer struct {
	mode string
	// Cache for rendered content to avoid re-rendering unchanged text
	cacheMu sync.RWMutex
	cache   map[string]cachedRender
}

type cachedRender struct {
	html      string
	timestamp time.Time
}

func New(mode string) *Renderer {
	return &Renderer{
		mode:  NormalizeMode(mode),
		cache: make(map[string]cachedRender),
	}
}

func NormalizeMode(mode string) string {
	return ModeMarkdownStream
}

func IsValidMode(mode string) bool {
	return true // All modes are normalized to markdown_stream anyway
}

func (r *Renderer) Mode() string {
	if r == nil {
		return ModeMarkdownStream
	}
	return r.mode
}

// ClearCache clears the render cache
func (r *Renderer) ClearCache() {
	if r == nil {
		return
	}

	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.cache = make(map[string]cachedRender)
}

// Render converts markdown text to Telegram-safe HTML depending on mode.
func (r *Renderer) Render(text string, streaming bool) Result {
	result := Result{
		Text:         text,
		FallbackText: text,
		UseHTML:      false,
	}

	// Check cache first for non-streaming or completed streaming
	if !streaming {
		if cached := r.getFromCache(text); cached != "" {
			result.Text = cached
			result.UseHTML = true
			return result
		}

	}

	// Render and cache
	rendered := MarkdownToTelegramHTML(text)
	result.Text = rendered
	result.UseHTML = true

	// Cache non-streaming results
	if !streaming {
		r.addToCache(text, rendered)
	}

	return result
}

func (r *Renderer) getFromCache(text string) string {
	if r == nil {
		return ""
	}

	r.cacheMu.RLock()
	defer r.cacheMu.RUnlock()

	if cached, ok := r.cache[text]; ok {
		// Cache valid for 5 minutes
		if time.Since(cached.timestamp) < 5*time.Minute {
			return cached.html
		}
	}
	return ""
}

func (r *Renderer) addToCache(text, html string) {
	if r == nil {
		return
	}

	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()

	// Limit cache size to prevent memory issues
	if len(r.cache) > 100 {
		// Remove oldest entries
		var oldestKey string
		var oldestTime time.Time
		for key, entry := range r.cache {
			if oldestTime.IsZero() || entry.timestamp.Before(oldestTime) {
				oldestTime = entry.timestamp
				oldestKey = key
			}
		}
		if oldestKey != "" {
			delete(r.cache, oldestKey)
		}
	}

	r.cache[text] = cachedRender{
		html:      html,
		timestamp: time.Now(),
	}
}

var (
	// Bold italic: non-greedy match
	boldItalicRe = regexp.MustCompile(`\*\*\*([^*]+?)\*\*\*`)

	// Bold: allows single asterisk (for nested italic)
	boldStarRe = regexp.MustCompile(`\*\*([^*]+?(?:\*[^*]+?)*?)\*\*`)
	boldUndRe  = regexp.MustCompile(`__([^_]+?(?:_[^_]+?)*?)__`)

	// Strikethrough
	strikeRe = regexp.MustCompile(`~~([^~]+?)~~`)

	// Italic: non-greedy match
	italicStarRe = regexp.MustCompile(`\*([^*]+?)\*`)
	italicUndRe  = regexp.MustCompile(`_([^_]+?)_`)

	// Heading
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
)

// MarkdownToTelegramHTML converts a conservative markdown subset to Telegram HTML.
// It intentionally avoids complex constructs that are fragile during streaming updates.
func MarkdownToTelegramHTML(input string) string {
	if input == "" {
		return ""
	}

	// Input validation: limit maximum length to prevent DoS attacks
	const maxInputSize = 100000 // 100KB
	if len(input) > maxInputSize {
		// Return truncated version to avoid processing oversized input
		truncated := input[:maxInputSize]
		return html.EscapeString(truncated) + "... (truncated)"
	}

	input = strings.ReplaceAll(input, "\r\n", "\n")
	lines := strings.Split(input, "\n")
	rendered := make([]string, 0, len(lines))
	inFence := false
	fenceHasQuote := false
	fenceStart := ""
	fenceLines := make([]string, 0, 16)

	// State for multi-line blockquote handling
	inBlockquote := false
	blockquoteLines := make([]string, 0, 8)

	for _, line := range lines {
		// Check for quote prefix
		strippedLine, hadQuote := stripQuotePrefix(line)
		trimmed := strings.TrimSpace(strippedLine)

		// Check if it's the start/end of a code block
		// Note: single-line ```code``` should be treated as inline code, not code block
		if strings.HasPrefix(trimmed, "```") {
			// Count number of backticks
			backtickCount := 0
			for i := 0; i < len(trimmed) && trimmed[i] == '`'; i++ {
				backtickCount++
			}

			// Check if it ends on the same line
			if !inFence {
				// Check if there are closing backticks on the same line
				if strings.HasSuffix(trimmed, strings.Repeat("`", backtickCount)) && len(trimmed) > backtickCount*2 {
					// Single-line code block, treat as inline
					// Flush any pending blockquote first
					if inBlockquote {
						rendered = append(rendered, renderBlockquote(blockquoteLines))
						inBlockquote = false
						blockquoteLines = blockquoteLines[:0]
					}
					rendered = append(rendered, renderMarkdownLine(line))
					continue
				}

				// Multi-line code block starts
				// Flush any pending blockquote first
				if inBlockquote {
					rendered = append(rendered, renderBlockquote(blockquoteLines))
					inBlockquote = false
					blockquoteLines = blockquoteLines[:0]
				}
				inFence = true
				fenceHasQuote = hadQuote
				fenceStart = line
				fenceLines = fenceLines[:0]

			} else {
				// Code block ends
				inFence = false
				if fenceHasQuote {
					rendered = append(rendered, renderQuotedFenceBlock(fenceLines))
				} else {
					rendered = append(rendered, renderFenceBlock(fenceLines))
				}

			}
			continue
		}

		if inFence {
			// If fence has quote prefix, strip it from the line
			if fenceHasQuote {
				stripped, _ := stripQuotePrefix(line)
				fenceLines = append(fenceLines, stripped)
			} else {
				fenceLines = append(fenceLines, line)
			}
			continue
		}

		// Check if this line is a blockquote (using hadQuote from stripQuotePrefix)
		if hadQuote {
			// This is a blockquote line (could be empty)
			if !inBlockquote {
				// Start a new blockquote
				inBlockquote = true
				blockquoteLines = blockquoteLines[:0]
			}
			// Add the stripped content (could be empty for lines with just >)
			blockquoteLines = append(blockquoteLines, strippedLine)
		} else {
			// Not a blockquote line
			if inBlockquote {
				// Flush the accumulated blockquote
				rendered = append(rendered, renderBlockquote(blockquoteLines))
				inBlockquote = false
				blockquoteLines = blockquoteLines[:0]
			}
			// Render the non-blockquote line
			rendered = append(rendered, renderMarkdownLine(line))
		}
	}

	// Handle any trailing blockquote
	if inBlockquote {
		rendered = append(rendered, renderBlockquote(blockquoteLines))
	}

	if inFence {
		// Keep unfinished fence raw while streaming.
		// If fence has quote prefix, strip it from fenceStart
		if fenceHasQuote {
			strippedStart, _ := stripQuotePrefix(fenceStart)
			rendered = append(rendered, html.EscapeString(strippedStart))
		} else {
			rendered = append(rendered, html.EscapeString(fenceStart))
		}
		for _, line := range fenceLines {
			rendered = append(rendered, html.EscapeString(line))
		}
	}

	result := strings.Join(rendered, "\n")

	return result
}

func renderMarkdownLine(line string) string {
	// Check if it's a heading first
	matches := headingRe.FindStringSubmatch(line)
	if matches != nil {
		return renderHeading(matches[1], matches[2])
	}

	// Check if it's a horizontal rule
	trimmed := strings.TrimSpace(line)
	if isHorizontalRule(trimmed) {
		// Telegram HTML mode doesn't support <hr> tag, use visual separator instead
		return "───────────────"
	}

	// Check if it's a blockquote
	stripped, hadQuote := stripQuotePrefix(line)
	if hadQuote {
		// Use renderBlockquote for consistency (single line blockquote)
		return renderBlockquote([]string{stripped})
	}

	return renderInline(line)
}

func isHorizontalRule(line string) bool {
	if line == "" {
		return false
	}
	// Check if line consists only of 3 or more -, *, or _
	firstChar := line[0]
	if firstChar != '-' && firstChar != '*' && firstChar != '_' {
		return false
	}
	// Ensure all characters are the same
	for i := 1; i < len(line); i++ {
		if line[i] != firstChar {
			return false
		}
	}
	// At least 3 characters
	return len(line) >= 3
}

// stripQuotePrefix removes leading > characters and optional spaces
// Returns the stripped line and whether it had a quote prefix
func stripQuotePrefix(line string) (stripped string, hadQuote bool) {
	stripped = line
	hadQuote = false

	// Remove leading > characters
	for len(stripped) > 0 && stripped[0] == '>' {
		hadQuote = true
		stripped = stripped[1:]
		// Remove optional space after >
		if len(stripped) > 0 && stripped[0] == ' ' {
			stripped = stripped[1:]
		}
	}
	return stripped, hadQuote
}

func renderHeading(levelMarkers, title string) string {
	// Simply convert heading to bold text without adding any emoji or prefixes
	// This respects the principle of not modifying OpenCode's output content

	// Apply inline formatting to the title (but don't escape HTML tags)
	formattedTitle := applyInlineFormatting(title)

	return fmt.Sprintf("<b>%s</b>", formattedTitle)
}

// applyInlineFormatting applies markdown formatting with proper HTML escaping
// Used for headings to ensure security while preserving formatting
func applyInlineFormatting(text string) string {
	if text == "" {
		return ""
	}

	// First escape HTML to prevent injection
	escaped := html.EscapeString(text)

	// Then apply markdown formatting on the escaped text
	// This is safe because the text is already escaped
	return renderInline(escaped)
}

func renderInline(line string) string {
	if line == "" {
		return ""
	}

	// Step 1: Extract links and code blocks, replace with placeholders
	type placeholder struct {
		start, end    int
		html          string
		placeholderID string
	}
	var placeholders []placeholder
	placeholderIndex := 0

	// Scan the string to identify links and code blocks
	i := 0
	for i < len(line) {
		// Check if it's the start of a link
		if line[i] == '[' {
			label, url, ok := parseLink(line[i:])
			if ok {
				// Security check: only allow http:// or https:// protocols
				if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
					// Handle parentheses in URL
					url = balanceParentheses(url)
					// Escape label and url
					escapedLabel := html.EscapeString(label)
					escapedUrl := html.EscapeString(url)
					// Generate link HTML
					linkHTML := fmt.Sprintf(`<a href="%s">%s</a>`, escapedUrl, escapedLabel)
					// Create placeholder
					placeholderID := fmt.Sprintf("{{LINK%d}}", placeholderIndex)
					placeholderIndex++
					// Record the placeholder
					placeholders = append(placeholders, placeholder{
						start:         i,
						end:           i + len(label) + len(url) + 4, // []() four characters
						html:          linkHTML,
						placeholderID: placeholderID,
					})
					// Skip the link
					i += len(label) + len(url) + 4
					continue
				} else {
					// Unsafe URL, treat as plain text
					i++
					continue
				}
			}
		}

		// Check if it's the start of a code block
		if line[i] == '`' {
			// Find the start of code block
			backtickCount := 1
			for i+backtickCount < len(line) && line[i+backtickCount] == '`' {
				backtickCount++
			}

			// Find matching closing backticks
			end := -1
			for j := i + backtickCount; j < len(line); j++ {
				if line[j] == '`' {
					endCount := 1
					for j+endCount < len(line) && line[j+endCount] == '`' {
						endCount++
					}

					if endCount == backtickCount {
						end = j
						break
					}

					j += endCount - 1
				}
			}

			if end != -1 {
				// Extract code content
				code := line[i+backtickCount : end]
				// Escape HTML special characters in code content
				escapedCode := html.EscapeString(code)
				// Generate code HTML
				codeHTML := "<code>" + escapedCode + "</code>"
				// Create placeholder
				placeholderID := fmt.Sprintf("{{CODE%d}}", placeholderIndex)
				placeholderIndex++
				// Record the placeholder
				placeholders = append(placeholders, placeholder{
					start:         i,
					end:           end + backtickCount,
					html:          codeHTML,
					placeholderID: placeholderID,
				})
				// Skip the code block
				i = end + backtickCount
				continue
			} else {
				// No matching closing backticks found, treat as plain text
				i++
				continue
			}
		}

		i++
	}

	// Step 2: Build string with placeholders
	// For simplicity, we build the new string from left to right
	var processed strings.Builder
	lastPos := 0
	for _, ph := range placeholders {
		// Add text before the placeholder
		if ph.start > lastPos {
			processed.WriteString(line[lastPos:ph.start])
		}
		// Add the placeholder
		processed.WriteString(ph.placeholderID)
		lastPos = ph.end
	}
	// Add remaining text
	if lastPos < len(line) {
		processed.WriteString(line[lastPos:])
	}
	processedStr := processed.String()

	// Step 3: HTML escape the entire string (placeholders are unaffected as they contain no special characters)
	escapedStr := html.EscapeString(processedStr)

	// Step 4: Apply markdown formatting
	formattedStr := applyFormatting(escapedStr)

	// Step 5: Replace placeholders with actual HTML
	result := formattedStr
	for _, ph := range placeholders {
		result = strings.Replace(result, ph.placeholderID, ph.html, 1)
	}

	return result
}

// applyFormatting applies markdown formatting to escaped text
func applyFormatting(text string) string {
	if text == "" {
		return ""
	}

	// Apply formatting
	text = boldItalicRe.ReplaceAllString(text, "<b><i>$1</i></b>")
	text = boldStarRe.ReplaceAllString(text, "<b>$1</b>")
	text = boldUndRe.ReplaceAllString(text, "<b>$1</b>")
	text = strikeRe.ReplaceAllString(text, "<s>$1</s>")
	text = italicStarRe.ReplaceAllString(text, "<i>$1</i>")
	text = italicUndRe.ReplaceAllString(text, "<i>$1</i>")

	return text
}

// parseLink parses markdown link, correctly handles parentheses
func parseLink(input string) (label, url string, ok bool) {
	// Find first '['
	start := strings.Index(input, "[")
	if start == -1 {
		return "", "", false
	}

	// Find matching ']'
	balance := 0
	end := -1
	for i := start; i < len(input); i++ {
		if input[i] == '[' {
			balance++
		} else if input[i] == ']' {
			balance--
			if balance == 0 {
				end = i
				break
			}
		}
	}
	if end == -1 {
		return "", "", false
	}

	label = input[start+1 : end]

	// Find '('
	if end+1 >= len(input) || input[end+1] != '(' {
		return "", "", false
	}

	// Find matching ')'
	balance = 0
	urlStart := end + 2
	urlEnd := -1
	for i := urlStart - 1; i < len(input); i++ {
		if input[i] == '(' {
			balance++
		} else if input[i] == ')' {
			balance--
			if balance == 0 {
				urlEnd = i
				break
			}
		}
	}
	if urlEnd == -1 {
		return "", "", false
	}

	url = input[urlStart:urlEnd]
	return label, url, true
}

// balanceParentheses handles parentheses in URL, ensures balanced parentheses
func balanceParentheses(url string) string {
	balance := 0
	lastValidIndex := len(url)

	for i, ch := range url {
		if ch == '(' {
			balance++
		} else if ch == ')' {
			balance--
			if balance < 0 {
				// Extra closing parenthesis, truncate here
				return url[:i]
			} else if balance == 0 {
				// Record last balanced position
				lastValidIndex = i + 1
			}
		}
	}

	// If more opening than closing parentheses, use last balanced position
	if balance > 0 {
		return url[:lastValidIndex]
	}

	return url
}

func renderFenceBlock(lines []string) string {
	if len(lines) == 0 {
		return "<pre><code></code></pre>"
	}
	return "<pre><code>" + html.EscapeString(strings.Join(lines, "\n")) + "</code></pre>"
}

func renderBlockquote(lines []string) string {
	if len(lines) == 0 {
		return ""
	}

	// Apply inline formatting to each line and join with <br>
	var formattedLines []string
	for _, line := range lines {
		formatted := renderInline(line)
		formattedLines = append(formattedLines, formatted)
	}

	if len(formattedLines) == 0 {
		return ""
	}

	// Join lines with newline characters
	// Telegram HTML parser doesn't support <br> tag, so use \n instead
	content := strings.Join(formattedLines, "\n")
	// Use quoted attribute value for better compatibility
	return "<blockquote expandable=\"\">" + content + "</blockquote>"
}

func renderQuotedFenceBlock(lines []string) string {
	return "<blockquote expandable=\"\">" + renderFenceBlock(lines) + "</blockquote>"
}
