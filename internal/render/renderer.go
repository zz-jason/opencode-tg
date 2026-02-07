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
	ModePlain          = "plain"
	ModeMarkdownFinal  = "markdown_final"
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
	plain     string
	timestamp time.Time
}

func New(mode string) *Renderer {
	return &Renderer{
		mode:  NormalizeMode(mode),
		cache: make(map[string]cachedRender),
	}
}

func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModePlain:
		return ModePlain
	case ModeMarkdownFinal:
		return ModeMarkdownFinal
	case ModeMarkdownStream:
		return ModeMarkdownStream
	default:
		return ModeMarkdownStream
	}
}

func IsValidMode(mode string) bool {
	switch NormalizeMode(mode) {
	case ModePlain, ModeMarkdownFinal, ModeMarkdownStream:
		return true
	default:
		return false
	}
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
	mode := ModeMarkdownStream
	if r != nil {
		mode = r.mode
	}

	result := Result{
		Text:         text,
		FallbackText: text,
		UseHTML:      false,
	}

	if mode == ModePlain {
		return result
	}
	if mode == ModeMarkdownFinal && streaming {
		return result
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
	// Links: match [label](url), uses custom parsing to handle parentheses
	linkRe = regexp.MustCompile(`\[([^\[\]]*)\]\(([^)]+)\)`)

	// 粗斜体：非贪婪匹配
	boldItalicRe = regexp.MustCompile(`\*\*\*([^*]+?)\*\*\*`)

	// 粗体：允许包含单个星号（用于嵌套斜体）
	boldStarRe = regexp.MustCompile(`\*\*([^*]+?(?:\*[^*]+?)*?)\*\*`)
	boldUndRe  = regexp.MustCompile(`__([^_]+?(?:_[^_]+?)*?)__`)

	// 删除线
	strikeRe = regexp.MustCompile(`~~([^~]+?)~~`)

	// 斜体：非贪婪匹配
	italicStarRe = regexp.MustCompile(`\*([^*]+?)\*`)
	italicUndRe  = regexp.MustCompile(`_([^_]+?)_`)

	// 标题
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
)

// MarkdownToTelegramHTML converts a conservative markdown subset to Telegram HTML.
// It intentionally avoids complex constructs that are fragile during streaming updates.
func MarkdownToTelegramHTML(input string) string {
	if input == "" {
		return ""
	}

	// 输入验证：限制最大长度防止 DoS 攻击
	const maxInputSize = 100000 // 100KB
	if len(input) > maxInputSize {
		// 返回截断的版本，避免处理过大的输入
		truncated := input[:maxInputSize]
		return html.EscapeString(truncated) + "... (truncated)"
	}

	input = strings.ReplaceAll(input, "\r\n", "\n")
	lines := strings.Split(input, "\n")
	rendered := make([]string, 0, len(lines))
	inFence := false
	fenceStart := ""
	fenceLines := make([]string, 0, 16)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// 检查是否是代码块开始/结束
		// 注意：单行的 ```code``` 应该被当作行内代码，而不是代码块
		if strings.HasPrefix(trimmed, "```") {
			// 计算反引号数量
			backtickCount := 0
			for i := 0; i < len(trimmed) && trimmed[i] == '`'; i++ {
				backtickCount++
			}

			// 检查是否在同一行结束
			if !inFence {
				// 查找行内是否有关闭的反引号
				if strings.HasSuffix(trimmed, strings.Repeat("`", backtickCount)) && len(trimmed) > backtickCount*2 {
					// 单行代码块，当作行内处理
					rendered = append(rendered, renderMarkdownLine(line))
					continue
				}

				// 多行代码块开始
				inFence = true
				fenceStart = line
				fenceLines = fenceLines[:0]
			} else {
				// 代码块结束
				inFence = false
				rendered = append(rendered, renderFenceBlock(fenceLines))
			}
			continue
		}

		if inFence {
			fenceLines = append(fenceLines, line)
			continue
		}

		rendered = append(rendered, renderMarkdownLine(line))
	}

	if inFence {
		// Keep unfinished fence raw while streaming.
		rendered = append(rendered, html.EscapeString(fenceStart))
		for _, line := range fenceLines {
			rendered = append(rendered, html.EscapeString(line))
		}
	}

	return strings.Join(rendered, "\n")
}

func renderMarkdownLine(line string) string {
	// Check if it's a heading first
	matches := headingRe.FindStringSubmatch(line)
	if matches != nil {
		return renderHeading(matches[1], matches[2])
	}
	return renderInline(line)
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
						end:           i + len(label) + len(url) + 4, // []() 四个字符
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

// parseLink 解析 markdown 链接，正确处理括号
func parseLink(input string) (label, url string, ok bool) {
	// 查找第一个 '['
	start := strings.Index(input, "[")
	if start == -1 {
		return "", "", false
	}

	// 查找匹配的 ']'
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

	// 查找 '('
	if end+1 >= len(input) || input[end+1] != '(' {
		return "", "", false
	}

	// 查找匹配的 ')'
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

// balanceParentheses 处理 URL 中的括号，确保括号平衡
func balanceParentheses(url string) string {
	balance := 0
	lastValidIndex := len(url)

	for i, ch := range url {
		if ch == '(' {
			balance++
		} else if ch == ')' {
			balance--
			if balance < 0 {
				// 多余的右括号，截断到这里
				return url[:i]
			} else if balance == 0 {
				// 记录最后一个平衡的位置
				lastValidIndex = i + 1
			}
		}
	}

	// 如果左括号多于右括号，使用最后一个平衡位置
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
