package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tg-bot/internal/config"
	"tg-bot/internal/opencode"
	"tg-bot/internal/render"
	"tg-bot/internal/session"
	"tg-bot/internal/storage"

	log "github.com/sirupsen/logrus"
	"gopkg.in/telebot.v4"
)

const (
	// maxTelegramMessages is the maximum number of messages we'll send for a single response
	// to avoid flooding the chat and hitting rate limits
	maxTelegramMessages = 20
	// telegramMessageMaxLength is Telegram's hard message size limit.
	telegramMessageMaxLength = 4096
)

// Bot represents the Telegram bot with all dependencies
type Bot struct {
	config         *config.Config
	tgBot          *telebot.Bot
	opencodeClient *opencode.Client
	sessionManager *session.Manager
	runtime        *openCodeRuntime
	ctx            context.Context
	cancel         context.CancelFunc

	// Model mapping for each user (userID -> map[int]modelSelection)
	modelMappingMu sync.RWMutex
	modelMapping   map[int64]map[int]modelSelection

	// Global model mapping (number -> modelSelection) - populated at startup
	globalModelMappingMu sync.RWMutex
	globalModelMapping   map[int]modelSelection

	// Session mapping for each user (userID -> map[int]sessionID)
	sessionMappingMu sync.RWMutex
	sessionMapping   map[int64]map[int]string

	// Streaming state management
	streamingStateMu sync.RWMutex
	streamingStates  map[string]*streamingState
	renderer         *render.Renderer
}

// streamingState tracks the state of an active streaming response
type streamingState struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	telegramMsg      *telebot.Message
	telegramMessages []*telebot.Message
	lastRendered     []string
	telegramCtx      telebot.Context

	content     *strings.Builder
	lastUpdate  time.Time
	updateMutex *sync.Mutex

	// Cache of formatted message chunks by message ID
	cachedMessageChunks map[string][]string

	// Cumulative display chunks (session info + all message chunks)
	allDisplayChunks []string

	// Whether session info has been added to display chunks
	sessionInfoAdded bool

	// Session ID for this streaming state
	sessionID        string
	requestMessageID string
	requestTraceID   string
	requestText      string

	// Event-driven stream state for /event updates.
	requestStartedAt      int64
	initialMessageIDs     map[string]bool
	initialMessageDigests map[string]string
	eventMessages         map[string]*eventMessageState
	activeMessageID       string
	displayOrder          []string
	displaySet            map[string]bool
	pendingOrder          []string
	pendingSet            map[string]bool
	lastEventAt           time.Time
	hasEventUpdates       bool
	revision              uint64
	requestObserved       bool
	reconcileInFlight     bool
	lastReconcileAt       time.Time
	pendingEventParts     map[string][]pendingEventPart
	sessionStatus         string
	lastStatusAt          time.Time
	sawBusyStatus         bool
	sawIdleAfterBusy      bool

	isStreaming bool
	isComplete  bool
}

type eventMessageState struct {
	Info      opencode.MessageInfo
	PartOrder []string
	Parts     map[string]opencode.MessagePartResponse
	LastEvent time.Time
}

type pendingEventPart struct {
	Part  opencode.MessagePartResponse
	Delta string
}

// modelSelection represents a model selection with provider and model IDs
type modelSelection struct {
	ProviderID string
	ModelID    string
	ModelName  string
}

// NewBot creates a new bot instance
func NewBot(cfg *config.Config) (*Bot, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Ensure cancel is called if we return with an error
	var returnErr error
	defer func() {
		if returnErr != nil {
			cancel()
		}
	}()

	// Create OpenCode client
	client := opencode.NewClient(cfg.OpenCode.URL, cfg.OpenCode.Timeout)

	// Test OpenCode connection
	healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second)
	defer healthCancel()

	if healthErr := client.HealthCheck(healthCtx); healthErr != nil {
		returnErr = fmt.Errorf("OpenCode health check failed: %w", healthErr)
		return nil, returnErr
	} else {
		log.Info("OpenCode connection successful")
	}

	// Create storage
	store, err := storage.NewStore(storage.Options{
		Type:     cfg.Storage.Type,
		FilePath: cfg.Storage.FilePath,
	})
	if err != nil {
		returnErr = fmt.Errorf("failed to create storage: %w", err)
		return nil, returnErr
	}

	// Create session manager with storage
	sessionManager := session.NewManagerWithStore(client, store)

	bot := &Bot{
		config:             cfg,
		opencodeClient:     client,
		sessionManager:     sessionManager,
		ctx:                ctx,
		cancel:             cancel,
		modelMapping:       make(map[int64]map[int]modelSelection),
		globalModelMapping: make(map[int]modelSelection),
		sessionMapping:     make(map[int64]map[int]string),
		streamingStates:    make(map[string]*streamingState),
		renderer:           render.New(cfg.Render.Mode),
	}

	// Initialize session manager before serving requests to ensure startup is healthy.
	initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
	defer initCancel()

	if err := sessionManager.Initialize(initCtx); err != nil {
		returnErr = fmt.Errorf("failed to initialize session manager: %w", err)
		return nil, returnErr
	}
	log.Info("Session manager initialized successfully with preloaded sessions and models")

	// Build global model mapping after successful initialization.
	bot.buildGlobalModelMapping(initCtx)

	runtime, err := newOpenCodeRuntime(ctx, bot)
	if err != nil {
		returnErr = fmt.Errorf("failed to initialize OpenCode runtime: %w", err)
		return nil, returnErr
	}
	bot.runtime = runtime

	return bot, nil
}

// SetTelegramBot sets the Telegram bot instance
func (b *Bot) SetTelegramBot(tgBot *telebot.Bot) {
	b.tgBot = tgBot
}

func (b *Bot) withTelegramInterfaceLog(interfaceName string, handler func(telebot.Context) error) func(telebot.Context) error {
	return func(c telebot.Context) error {
		var userID int64
		var chatID int64
		var messageID int
		var textLen int

		if c != nil {
			if sender := c.Sender(); sender != nil {
				userID = sender.ID
			}
			if chat := c.Chat(); chat != nil {
				chatID = chat.ID
			}
			if msg := c.Message(); msg != nil {
				messageID = msg.ID
				textLen = len(msg.Text)
			}
		}

		log.Infof("TG interface triggered: interface=%s user=%d chat=%d message_id=%d text_len=%d", interfaceName, userID, chatID, messageID, textLen)
		return handler(c)
	}
}

// Start starts the bot and registers handlers
func (b *Bot) Start() {
	if b.tgBot == nil {
		log.Error("Telegram bot not set")
		return
	}

	// Register command handlers
	b.tgBot.Handle("/help", b.withTelegramInterfaceLog("/help", b.handleHelp))
	b.tgBot.Handle("/sessions", b.withTelegramInterfaceLog("/sessions", b.handleSessions))
	b.tgBot.Handle("/new", b.withTelegramInterfaceLog("/new", b.handleNew))
	b.tgBot.Handle("/switch", b.withTelegramInterfaceLog("/switch", b.handleSwitch))
	b.tgBot.Handle("/abort", b.withTelegramInterfaceLog("/abort", b.handleAbort))
	b.tgBot.Handle("/models", b.withTelegramInterfaceLog("/models", b.handleModels))
	b.tgBot.Handle("/setmodel", b.withTelegramInterfaceLog("/setmodel", b.handleSetModel))
	b.tgBot.Handle("/rename", b.withTelegramInterfaceLog("/rename", b.handleRename))
	b.tgBot.Handle("/delete", b.withTelegramInterfaceLog("/delete", b.handleDelete))

	// Handle plain text messages (non-commands)
	b.tgBot.Handle(telebot.OnText, b.withTelegramInterfaceLog("OnText", b.handleText))
}

// handleHelp handles the /help command
func (b *Bot) handleHelp(c telebot.Context) error {
	helpText := `📚 OpenCode Bot Help

Core Commands:
• /help - Show this help
• /sessions - List all sessions
• /new [name] - Create new session
• /switch <number> - Switch current session
• /rename <number> <name> - Rename a session
• /delete <number> - Delete a session
• /abort - Abort current task

Model Selection:
• /models - List available AI models (with numbers)
• /setmodel <number> - Set model for current session

Interactive Mode:
Send any non-command text and I'll send it as an instruction to OpenCode and stream back the response.

Notes:
• Each user has one default session
• Use /new to create multiple sessions for different tasks
• Use /abort to abort long-running tasks
• Sending a new message automatically aborts previous streaming response`

	return c.Send(helpText)
}

// handleSessions handles the /sessions command
func (b *Bot) handleSessions(c telebot.Context) error {
	// Synchronize sessions from OpenCode to local storage
	if err := b.sessionManager.SyncSessions(b.ctx); err != nil {
		log.Errorf("Failed to synchronize sessions: %v", err)
		return c.Send(fmt.Sprintf("Failed to get session list: %v", err))
	}

	userID := c.Sender().ID
	sessions, err := b.sessionManager.ListUserSessions(b.ctx, userID)
	if err != nil {
		log.Errorf("Failed to list sessions: %v", err)
		return c.Send(fmt.Sprintf("Failed to get session list: %v", err))
	}

	if len(sessions) == 0 {
		return c.Send("You don't have any sessions yet. Use /new to create a new session.")
	}

	// Update session mapping for this user
	b.sessionMappingMu.Lock()
	b.sessionMapping[userID] = make(map[int]string)
	for i, sess := range sessions {
		b.sessionMapping[userID][i+1] = sess.SessionID
	}
	b.sessionMappingMu.Unlock()

	var sb strings.Builder
	sb.WriteString("📋 Available Sessions\n\n")

	currentSessionID, hasCurrent := b.sessionManager.GetUserSession(userID)

	for i, sess := range sessions {
		// Determine if this is the current session
		isCurrent := hasCurrent && sess.SessionID == currentSessionID

		// Format the header line
		if isCurrent {
			fmt.Fprintf(&sb, "[✅ CURRENT] %d. %s\n", i+1, sess.Name)
		} else {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, sess.Name)
		}

		// Add separator line (fixed length)
		sb.WriteString("────────────────\n")

		// Add session details with bullet points
		fmt.Fprintf(&sb, "• Created: %s\n", sess.CreatedAt.Format("2006-01-02 15:04"))
		fmt.Fprintf(&sb, "• Last used: %s\n", sess.LastUsedAt.Format("2006-01-02 15:04"))
		fmt.Fprintf(&sb, "• Messages: %d\n", sess.MessageCount)

		// Add model information
		if sess.ProviderID != "" && sess.ModelID != "" {
			fmt.Fprintf(&sb, "• Model: %s/%s\n", sess.ProviderID, sess.ModelID)
		} else if sess.ModelID != "" {
			fmt.Fprintf(&sb, "• Model: %s\n", sess.ModelID)
		} else if sess.ProviderID != "" {
			fmt.Fprintf(&sb, "• Model: %s\n", sess.ProviderID)
		} else {
			sb.WriteString("• Model: Default\n")
		}

		// Add empty line between sessions
		sb.WriteString("\n")
	}

	sb.WriteString("Use /switch <number> to switch sessions, /rename <number> <name> to rename, or /delete <number> to delete.")

	return c.Send(sb.String())
}

// handleNew handles the /new command
func (b *Bot) handleNew(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()

	name := "New session"
	if len(args) > 0 {
		name = strings.Join(args, " ")
	}

	sessionID, err := b.sessionManager.CreateNewSession(b.ctx, userID, name)
	if err != nil {
		log.Errorf("Failed to create session: %v", err)
		return c.Send(fmt.Sprintf("Failed to create session: %v", err))
	}

	// Set as current session
	b.sessionManager.SetUserSession(userID, sessionID)

	// Check if session has a model configured
	meta, exists := b.sessionManager.GetSessionMeta(sessionID)
	var message string
	if exists && meta.ProviderID != "" && meta.ModelID != "" {
		// Session has a model (likely from user's last preference)
		message = fmt.Sprintf("✅ Created new session: %s\n\nThis session has been set as your current session.\n\n📋 Using your last model preference.", name)
	} else {
		// No model configured for this session
		message = fmt.Sprintf("✅ Created new session: %s\n\nThis session has been set as your current session.\n\n⚠️ No AI model configured for this session.\n\nPlease use `/models` to view available models, then use `/setmodel <number>` to set a model for this session before sending messages.", name)
	}

	return c.Send(message)
}

// handleSwitch handles the /switch command
func (b *Bot) handleSwitch(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()

	if len(args) == 0 {
		return c.Send("Please specify the session number to switch to.\nUsage: /switch <number>\nUse /sessions to see available sessions.")
	}

	input := args[0]
	var sessionID string

	// Check if input is a number
	if num, err := strconv.Atoi(input); err == nil {
		// Input is a number, get session ID from mapping
		b.sessionMappingMu.RLock()
		userMapping, exists := b.sessionMapping[userID]
		b.sessionMappingMu.RUnlock()

		if !exists {
			return c.Send("Session mapping not found. Please use /sessions first to see available sessions.")
		}

		mappedSessionID, found := userMapping[num]
		if !found {
			return c.Send(fmt.Sprintf("Session number %d not found. Use /sessions to see available sessions.", num))
		}
		sessionID = mappedSessionID
	} else {
		// Input is not a number, treat as session ID
		sessionID = input
	}

	// Check if session exists for this user and get session details
	sessions, err := b.sessionManager.ListUserSessions(b.ctx, userID)
	if err != nil {
		log.Errorf("Failed to get user sessions: %v", err)
		return c.Send(fmt.Sprintf("Failed to get session list: %v", err))
	}
	var foundSession *session.SessionMeta
	var sessionNumber int
	for i, sess := range sessions {
		if sess.SessionID == sessionID {
			foundSession = sess
			sessionNumber = i + 1
			break
		}
	}

	if foundSession == nil {
		return c.Send("Session not found.\nUse /sessions to see available sessions.")
	}

	if err := b.sessionManager.SetUserSession(userID, sessionID); err != nil {
		log.Errorf("Failed to switch session: %v", err)
		return c.Send(fmt.Sprintf("Failed to switch session: %v", err))
	}

	return c.Send(fmt.Sprintf("✅ Session switched to:\n\n%d. %s", sessionNumber, foundSession.Name))
}

// handleAbort handles the /abort command
func (b *Bot) handleAbort(c telebot.Context) error {
	userID := c.Sender().ID
	sessionID, exists := b.sessionManager.GetUserSession(userID)

	if !exists {
		return c.Send("You don't have a current session. Use /new to create a new session.")
	}

	// First, try to cancel any local streaming state
	b.streamingStateMu.Lock()
	if state, ok := b.streamingStates[sessionID]; ok && state.isStreaming {
		state.cancel()
		state.isStreaming = false
		log.Infof("Cancelled local streaming state for session %s", sessionID)
	}
	b.streamingStateMu.Unlock()

	// Then send abort to OpenCode
	if err := b.opencodeClient.AbortSession(b.ctx, sessionID); err != nil {
		log.Errorf("Failed to abort session: %v", err)
		return c.Send(fmt.Sprintf("Failed to abort session: %v", err))
	}

	return c.Send("🛑 Abort signal sent. Current task will be interrupted.")
}

// formatMessageParts formats message parts for display
func formatMessageParts(parts []interface{}) string {
	return formatMessagePartsWithOptions(parts, true)
}

func formatMessagePartsWithOptions(parts []interface{}, includeReplyContent bool) string {
	if len(parts) == 0 {
		return "No detailed content"
	}

	var sb strings.Builder
	hasTextContent := false
	var textContent strings.Builder

	for _, part := range parts {
		partResp, ok := part.(opencode.MessagePartResponse)
		if !ok {
			log.Warnf("Unknown message part type: %T", part)
			continue
		}

		switch partResp.Type {
		case "text":
			if partResp.Text != "" {
				hasTextContent = true
				textContent.WriteString(partResp.Text)
				if !strings.HasSuffix(partResp.Text, "\n") {
					textContent.WriteString("\n")
				}
			}
		case "reasoning":
			if partResp.Text != "" {
				reasoningText := strings.ReplaceAll(partResp.Text, "\n", "\n> ")
				fmt.Fprintf(&sb, "> Thinking: %s\n", reasoningText)
			}
		case "step-start":
		case "step-finish":
		case "tool":
			sb.WriteString(formatToolCallPart(partResp.Tool, partResp.Snapshot, partResp.State, partResp.Text))
		default:
			fmt.Fprintf(&sb, "%v\n", partResp)
		}
	}

	// Add text content at the end if we have any.
	// For realtime display where msg.Content is already shown, this can be disabled
	// to avoid duplicate content blocks.
	if hasTextContent && includeReplyContent {
		text := strings.TrimSpace(textContent.String())
		if text != "" {
			// Truncate if too long, but be generous for important content
			if len(text) > 3000 {
				text = text[:3000] + "...\n(Reply content too long, truncated)"
			}
			fmt.Fprintf(&sb, "\n• ✅ Reply content:\n%s\n", text)
		}
	}

	result := strings.TrimSpace(sb.String())
	if result == "" {
		return "No detailed content"
	}
	return result
}

func formatToolCallPart(toolName, snapshot string, state interface{}, text string) string {
	snapshotData := parseJSONMap(snapshot)
	if toolName == "" {
		toolName = extractToolName(snapshotData)
	}
	sourceData := toStringAnyMap(state)
	if sourceData == nil {
		sourceData = snapshotData
	}

	description := extractToolDescription(sourceData, text)
	command := extractToolCommand(sourceData)
	output := extractToolOutput(sourceData)
	if output == "" && text != "" && text != description {
		output = text
	}
	description = strings.TrimSpace(description)
	if strings.EqualFold(description, "executed") {
		description = ""
	}
	command = strings.TrimSpace(command)
	output = strings.TrimSpace(strings.ReplaceAll(output, "\r\n", "\n"))

	lines := make([]string, 0, 24)
	if comment := normalizeToolBlockComment(description); comment != "" {
		lines = append(lines, "# "+comment)
	}
	if command != "" {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "$ "+truncateAndInline(command, 500))
	}
	if output != "" {
		outputText := truncateMultiline(output, 2500)
		if outputText != "" {
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, strings.Split(outputText, "\n")...)
		}
	}

	if len(lines) == 0 {
		fallbackComment := "tool output"
		if name := strings.TrimSpace(toolName); name != "" {
			fallbackComment = name + " output"
		}
		lines = append(lines, "# "+fallbackComment)
	}

	var block strings.Builder
	fmt.Fprintf(&block, "```%s\n", toolBlockLanguage(toolName, command))
	block.WriteString(strings.Join(lines, "\n"))
	block.WriteString("\n```")

	return wrapInExpandableBlockquote(block.String())
}

// formatMessageWithMetadata formats a single OpenCode message with role, timestamp, parts, and content.
func formatMessageWithMetadata(msg opencode.Message) string {
	var sb strings.Builder

	// Determine role display
	var role string
	switch msg.Role {
	case "user":
		role = "👤 User"
	case "assistant":
		role = "🤖 Assistant"
	case "system":
		role = "⚙️ System"
	default:
		role = msg.Role
	}

	// Format timestamp
	timeStr := msg.CreatedAt.Format("15:04")

	// Write header
	fmt.Fprintf(&sb, "[%s] [%s]\n", role, timeStr)
	sb.WriteString("---\n")

	// Show detailed parts if available
	if len(msg.Parts) > 0 {
		// Include reply content from parts only if message content is empty
		// (to avoid duplicate content blocks)
		partsStr := formatMessagePartsWithOptions(msg.Parts, msg.Content == "")
		if partsStr != "No detailed content" {
			fmt.Fprintf(&sb, "%s\n", partsStr)
		}
	}

	// Show message content if available
	if msg.Content != "" {
		fmt.Fprintf(&sb, "%s\n", msg.Content)
	}

	return strings.TrimSpace(sb.String())
}

// buildDisplayChunksFromMessagesWithCache builds display chunks using cache to avoid re-rendering
func (b *Bot) buildDisplayChunksFromMessagesWithCache(messages []opencode.Message, sessionMeta *session.SessionMeta, state *streamingState) []string {
	if len(messages) == 0 {
		return []string{"No messages yet."}
	}

	// Ensure cache maps are initialized
	if state.cachedMessageChunks == nil {
		state.cachedMessageChunks = make(map[string][]string)
	}
	if state.allDisplayChunks == nil {
		state.allDisplayChunks = []string{}
	}

	// Add session info if needed
	if sessionMeta != nil && !state.sessionInfoAdded {
		var sb strings.Builder
		fmt.Fprintf(&sb, "## Session: %s\n", sessionMeta.Name)
		sb.WriteString("---\n")
		fmt.Fprintf(&sb, "- Messages: %d\n", sessionMeta.MessageCount)
		if sessionMeta.ProviderID != "" && sessionMeta.ModelID != "" {
			fmt.Fprintf(&sb, "- Model: %s/%s\n", sessionMeta.ProviderID, sessionMeta.ModelID)
		}
		sb.WriteString("\n")
		sessionInfo := sb.String()
		// Session info is typically short, no need to split
		state.allDisplayChunks = append(state.allDisplayChunks, sessionInfo)
		state.sessionInfoAdded = true
	}

	// Process each message in order
	for _, msg := range messages {
		if _, cached := state.cachedMessageChunks[msg.ID]; cached {
			// Already cached, skip
			continue
		}

		// Format and split this message
		formatted := formatMessageWithMetadata(msg)
		chunks := b.splitLongContentPreserveCodeBlocks(formatted)
		state.cachedMessageChunks[msg.ID] = chunks
		state.allDisplayChunks = append(state.allDisplayChunks, chunks...)
	}

	return state.allDisplayChunks
}

func parseJSONMap(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

func toStringAnyMap(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	return m
}

func extractToolName(source map[string]interface{}) string {
	if source == nil {
		return ""
	}
	for _, key := range []string{"name", "type", "tool", "function"} {
		if value, ok := source[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func extractToolDescription(source map[string]interface{}, text string) string {
	description := ""
	if source != nil {
		if inputMap, ok := source["input"].(map[string]interface{}); ok {
			if desc, ok := inputMap["description"].(string); ok && desc != "" {
				description = desc
			} else if cmd, ok := inputMap["command"].(string); ok && cmd != "" {
				description = cmd
			}
		} else if input, ok := source["input"].(string); ok && input != "" {
			description = input
		} else if content, ok := source["content"].(string); ok && content != "" {
			description = content
		}
	}
	if description == "" && text != "" {
		description = text
	}
	if description == "" {
		return "executed"
	}
	return truncateAndInline(description, 100)
}

func extractToolCommand(source map[string]interface{}) string {
	if source == nil {
		return ""
	}
	if inputMap, ok := source["input"].(map[string]interface{}); ok {
		if cmd, ok := inputMap["command"].(string); ok && cmd != "" {
			return cmd
		}
		if argsMap, ok := inputMap["args"].(map[string]interface{}); ok {
			if cmd, ok := argsMap["command"].(string); ok && cmd != "" {
				return cmd
			}
		}
	}
	if cmd, ok := source["command"].(string); ok && cmd != "" {
		return cmd
	}
	if argsMap, ok := source["args"].(map[string]interface{}); ok {
		if cmd, ok := argsMap["command"].(string); ok && cmd != "" {
			return cmd
		}
	}
	return ""
}

func extractToolOutput(source map[string]interface{}) string {
	if source == nil {
		return ""
	}
	for _, key := range []string{"output", "result", "stderr", "stdout", "error"} {
		if value, exists := source[key]; exists {
			if text := stringifyToolValue(value); strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return ""
}

func stringifyToolValue(v interface{}) string {
	switch value := v.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

func truncateAndInline(text string, maxLen int) string {
	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}
	text = strings.ReplaceAll(text, "\n", "\\n")
	return strings.TrimSpace(text)
}

func truncateMultiline(text string, maxLen int) string {
	text = strings.TrimSpace(text)
	if len(text) > maxLen {
		return text[:maxLen] + "..."
	}
	return text
}

func normalizeToolBlockComment(comment string) string {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return ""
	}
	comment = strings.ReplaceAll(comment, "\r\n", " ")
	comment = strings.ReplaceAll(comment, "\n", " ")
	comment = strings.Join(strings.Fields(comment), " ")
	if len(comment) > 200 {
		return comment[:200] + "..."
	}
	return comment
}

func toolBlockLanguage(toolName, command string) string {
	toolName = strings.ToLower(strings.TrimSpace(toolName))
	switch toolName {
	case "bash", "sh", "shell", "zsh", "fish", "terminal", "command":
		return "bash"
	case "python", "python3":
		return "python"
	case "javascript", "node":
		return "javascript"
	case "typescript", "ts-node":
		return "typescript"
	case "sql":
		return "sql"
	}
	if strings.TrimSpace(command) != "" {
		return "bash"
	}
	return "text"
}

func wrapInExpandableBlockquote(markdown string) string {
	markdown = strings.TrimSpace(strings.ReplaceAll(markdown, "\r\n", "\n"))
	if markdown == "" {
		return ""
	}

	lines := strings.Split(markdown, "\n")
	var sb strings.Builder
	for _, line := range lines {
		if line == "" {
			sb.WriteString(">\n")
			continue
		}
		sb.WriteString("> ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// handleModels lists available AI models
func (b *Bot) handleModels(c telebot.Context) error {
	// Synchronize models from OpenCode to local storage
	if err := b.sessionManager.SyncModels(b.ctx); err != nil {
		log.Errorf("Failed to synchronize models: %v", err)
		return c.Send(fmt.Sprintf("Failed to get model list: %v", err))
	}

	providersResp, err := b.opencodeClient.GetProviders(b.ctx)
	if err != nil {
		log.Errorf("Failed to get providers: %v", err)
		return c.Send(fmt.Sprintf("Failed to get model list: %v", err))
	}

	var sb strings.Builder
	sb.WriteString("📋 Connected Providers\n\n")

	// Create a set of connected provider IDs for faster lookup
	connectedSet := make(map[string]bool)
	for _, providerID := range providersResp.Connected {
		connectedSet[providerID] = true
	}

	// Collect connected providers that actually expose models.
	connectedProviders := make([]opencode.Provider, 0, len(providersResp.All))
	for _, provider := range providersResp.All {
		if connectedSet[provider.ID] && len(provider.Models) > 0 {
			connectedProviders = append(connectedProviders, provider)
		}
	}

	// Keep display order stable across calls.
	sort.Slice(connectedProviders, func(i, j int) bool {
		if strings.EqualFold(connectedProviders[i].Name, connectedProviders[j].Name) {
			return connectedProviders[i].ID < connectedProviders[j].ID
		}
		return strings.ToLower(connectedProviders[i].Name) < strings.ToLower(connectedProviders[j].Name)
	})

	modelCounter := 1 // sequential integer -> model selection
	modelMapping := make(map[int]modelSelection)

	if len(connectedProviders) == 0 {
		sb.WriteString("⚠️ No connected AI providers.\n")
		sb.WriteString("Please configure API keys for at least one AI provider first.")
		b.storeModelMapping(c.Sender().ID, modelMapping)
		return c.Send(sb.String())
	}

	for _, provider := range connectedProviders {
		fmt.Fprintf(&sb, "%s (%s)\n", provider.Name, provider.ID)
		sb.WriteString("────────────────\n")

		models := make([]opencode.Model, 0, len(provider.Models))
		for _, model := range provider.Models {
			models = append(models, model)
		}
		sort.Slice(models, func(i, j int) bool {
			if strings.EqualFold(models[i].Name, models[j].Name) {
				return models[i].ID < models[j].ID
			}
			return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name)
		})

		for _, model := range models {
			// Store mapping
			modelMapping[modelCounter] = modelSelection{
				ProviderID: provider.ID,
				ModelID:    model.ID,
				ModelName:  model.Name,
			}

			fmt.Fprintf(&sb, "%d. %s\n", modelCounter, model.Name)

			modelCounter++
		}

		sb.WriteString("\n")
	}

	sb.WriteString("Use /setmodel <number> to set model for current session.\n")
	sb.WriteString("Use /new <name> to create new session (uses your last selected model).")

	// Store the model mapping in the bot context (for this user)
	// We'll store it in a simple way for now - could be enhanced with persistence
	b.storeModelMapping(c.Sender().ID, modelMapping)

	result := sb.String()
	if len(result) > 4000 {
		result = result[:4000] + "\n...(content too long, truncated)"
	}
	return c.Send(result)
}

// handleSetModel sets the model for the current session
func (b *Bot) handleSetModel(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()
	log.Infof("User %d executing /setmodel with args: %v", userID, args)

	if len(args) != 1 {
		log.Warnf("Invalid arguments count: %d", len(args))
		return c.Send("Please specify the model number.\nUsage: /setmodel <number>\nUse /models to view available models and their numbers.")
	}

	sessionID, exists := b.sessionManager.GetUserSession(userID)
	if !exists {
		log.Warnf("User %d has no current session", userID)
		return c.Send("You don't have a current session. Use /new to create a new session.")
	}
	log.Debugf("User %d current session: %s", userID, sessionID)

	modelNum, err := strconv.Atoi(args[0])
	if err != nil {
		log.Warnf("Invalid model number: %s", args[0])
		return c.Send(fmt.Sprintf("Invalid model number: %s. Number must be an integer.\nUse /models to view available models and their numbers.", args[0]))
	}
	log.Debugf("Model number: %d", modelNum)

	// Get model selection from mapping
	selection, exists := b.getModelSelection(userID, modelNum)
	if !exists {
		log.Warnf("Model mapping not found for user %d, model %d", userID, modelNum)
		return c.Send(fmt.Sprintf("Model with number %d not found. Please use /models to view the latest model list first.", modelNum))
	}
	log.Debugf("Model selection found: %s/%s (%s)", selection.ProviderID, selection.ModelID, selection.ModelName)

	log.Debugf("Calling SetSessionModel for session %s with model %s/%s", sessionID, selection.ProviderID, selection.ModelID)
	if err := b.sessionManager.SetSessionModel(b.ctx, sessionID, selection.ProviderID, selection.ModelID); err != nil {
		log.Errorf("Failed to set session model: %v", err)
		return c.Send(fmt.Sprintf("Failed to set model: %v", err))
	}

	log.Infof("Successfully set model for user %d session %s to %s/%s", userID, sessionID, selection.ProviderID, selection.ModelID)
	return c.Send(fmt.Sprintf("✅ Current session model set to %s (%s/%s)\n\nThis model will be used as your default for new sessions.", selection.ModelName, selection.ProviderID, selection.ModelID))
}

// handleText handles plain text messages (non-commands) through attach-like runtime actors.
func (b *Bot) handleText(c telebot.Context) error {
	userID := c.Sender().ID
	text := strings.TrimSpace(c.Text())
	if text == "" {
		return nil
	}

	sessionID, err := b.sessionManager.GetOrCreateSession(b.ctx, userID)
	if err != nil {
		log.Errorf("Failed to get/create session: %v", err)
		return c.Send(fmt.Sprintf("Session error: %v", err))
	}

	var messageModel *opencode.MessageModel
	meta, exists := b.sessionManager.GetSessionMeta(sessionID)
	if exists && meta.ProviderID != "" && meta.ModelID != "" {
		messageModel = &opencode.MessageModel{
			ProviderID: meta.ProviderID,
			ModelID:    meta.ModelID,
		}
		log.Debugf("Using session model %s/%s for message", meta.ProviderID, meta.ModelID)
	} else {
		log.Warnf("No model configured for session %s", sessionID)
		return c.Send("⚠️ No AI model configured for this session.\n\nPlease use `/models` to view available models, then use `/setmodel <number>` to set a model for this session.")
	}

	requestTraceID := opencode.GenerateMessageID()

	if b.runtime == nil {
		return c.Send("Processing error: runtime is not initialized")
	}

	err = b.runtime.SubmitTextTask(runtimeTaskRequest{
		SessionID:      sessionID,
		RequestTraceID: requestTraceID,
		Text:           text,
		Model:          messageModel,
		TelegramCtx:    c,
	})
	if err != nil {
		log.Warnf("OpenCode runtime task failed for session %s request_trace_id=%s: %v", sessionID, requestTraceID, err)
		return c.Send(fmt.Sprintf("Processing error: %v", err))
	}
	return nil
}

// buildGlobalModelMapping builds the global model mapping from preloaded models
func (b *Bot) buildGlobalModelMapping(ctx context.Context) {
	// Get providers to determine connection status
	providersResp, err := b.opencodeClient.GetProviders(ctx)
	if err != nil {
		log.Warnf("Failed to get providers for global model mapping: %v", err)
		return
	}

	// Create a set of connected provider IDs for faster lookup
	connectedSet := make(map[string]bool)
	for _, providerID := range providersResp.Connected {
		connectedSet[providerID] = true
	}

	// Get preloaded models from storage
	models, err := b.sessionManager.GetAllModels()
	if err != nil {
		log.Warnf("Failed to get preloaded models for global mapping: %v", err)
		return
	}

	// Build mapping with sequential numbers
	modelCounter := 1
	globalMapping := make(map[int]modelSelection)

	for _, model := range models {
		// Only include models from connected providers
		if !connectedSet[model.ProviderID] {
			continue
		}

		globalMapping[modelCounter] = modelSelection{
			ProviderID: model.ProviderID,
			ModelID:    model.ID,
			ModelName:  model.Name,
		}
		modelCounter++
	}

	// Store global mapping
	b.globalModelMappingMu.Lock()
	b.globalModelMapping = globalMapping
	b.globalModelMappingMu.Unlock()

	log.Infof("Built global model mapping with %d models from connected providers", len(globalMapping))
}

// storeModelMapping stores the model mapping for a user
func (b *Bot) storeModelMapping(userID int64, mapping map[int]modelSelection) {
	b.modelMappingMu.Lock()
	defer b.modelMappingMu.Unlock()
	b.modelMapping[userID] = mapping
}

// getModelSelection gets a model selection by ID for a user
func (b *Bot) getModelSelection(userID int64, modelID int) (modelSelection, bool) {
	// First, try user-specific mapping
	b.modelMappingMu.RLock()
	userMapping, userExists := b.modelMapping[userID]
	b.modelMappingMu.RUnlock()

	if userExists {
		if selection, exists := userMapping[modelID]; exists {
			return selection, true
		}
	}

	// Fall back to global mapping
	b.globalModelMappingMu.RLock()
	selection, globalExists := b.globalModelMapping[modelID]
	b.globalModelMappingMu.RUnlock()

	if globalExists {
		return selection, true
	}

	return modelSelection{}, false
}

func (b *Bot) tryReconcileEventStateWithLatestMessages(state *streamingState, minInterval time.Duration, force bool, reason string) (performed bool, requestObserved bool) {
	if state == nil || state.sessionID == "" || b.opencodeClient == nil {
		return false, false
	}

	now := time.Now()
	state.updateMutex.Lock()
	requestObserved = state.requestObserved
	if state.reconcileInFlight {
		state.updateMutex.Unlock()
		log.Debugf("Skip reconcile while in-flight: session=%s reason=%s", state.sessionID, reason)
		return false, requestObserved
	}
	if !force && minInterval > 0 && !state.lastReconcileAt.IsZero() && now.Sub(state.lastReconcileAt) < minInterval {
		state.updateMutex.Unlock()
		return false, requestObserved
	}
	state.reconcileInFlight = true
	state.updateMutex.Unlock()

	defer func() {
		state.updateMutex.Lock()
		state.reconcileInFlight = false
		state.lastReconcileAt = time.Now()
		requestObserved = state.requestObserved
		state.updateMutex.Unlock()
	}()

	requestObserved = b.reconcileEventStateWithLatestMessages(state)
	return true, requestObserved
}

func (b *Bot) applySessionEventLocked(state *streamingState, sessionID string, event opencode.SessionEvent) (changed bool, forceFlush bool) {
	switch event.Type {
	case "message.updated":
		return b.applyMessageUpdatedEventLocked(state, sessionID, event.Properties)
	case "message.part.updated":
		return b.applyMessagePartUpdatedEventLocked(state, sessionID, event.Properties)
	case "session.status":
		return b.applySessionStatusEventLocked(state, sessionID, event.Properties)
	default:
		return false, false
	}
}

func (b *Bot) applySessionStatusEventLocked(state *streamingState, sessionID string, raw json.RawMessage) (bool, bool) {
	var payload struct {
		SessionID string                     `json:"sessionID"`
		Status    opencode.SessionStatusInfo `json:"status"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, false
	}
	if payload.SessionID == "" || payload.SessionID != sessionID {
		return false, false
	}

	next := strings.TrimSpace(strings.ToLower(payload.Status.Type))
	if next == "" {
		return false, false
	}

	changed := state.sessionStatus != next
	state.sessionStatus = next
	state.lastStatusAt = time.Now()
	switch next {
	case "busy":
		if !state.sawBusyStatus {
			changed = true
		}
		state.sawBusyStatus = true
		state.sawIdleAfterBusy = false
	case "idle":
		if state.sawBusyStatus && !state.sawIdleAfterBusy {
			changed = true
		}
		if state.sawBusyStatus {
			state.sawIdleAfterBusy = true
		}
	}
	if !changed {
		return false, false
	}
	return true, next == "idle"
}

func (b *Bot) applyMessageUpdatedEventLocked(state *streamingState, sessionID string, raw json.RawMessage) (bool, bool) {
	var payload opencode.MessageUpdatedProperties
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, false
	}
	info := payload.Info
	if info.ID == "" || info.SessionID == "" || info.SessionID != sessionID {
		return false, false
	}
	role := strings.ToLower(strings.TrimSpace(info.Role))
	if role == "user" {
		if b.isRequestUserMessageLocked(state, info) {
			state.requestObserved = true
			state.requestMessageID = info.ID
			return false, false
		}
		return false, false
	}
	parentID := strings.TrimSpace(info.ParentID)
	if state.requestMessageID != "" {
		if parentID != "" && parentID != state.requestMessageID {
			return false, false
		}
		if parentID == "" && !b.shouldTrackEventMessageLocked(state, info.ID, info.Time.Created) {
			return false, false
		}
	} else if !b.shouldTrackEventMessageLocked(state, info.ID, info.Time.Created) {
		return false, false
	}

	msgState := b.getOrCreateEventMessageStateLocked(state, info.ID)
	prevInfo := msgState.Info
	msgState.Info = mergeMessageInfo(prevInfo, info)

	displayChanged := false
	if role := strings.ToLower(strings.TrimSpace(msgState.Info.Role)); role != "" && role != "user" {
		displayChanged = b.enqueueEventMessageLocked(state, msgState.Info.ID)
	}

	completedNow := isEventMessageCompleted(msgState)
	infoChanged := !sameMessageInfoForRender(prevInfo, msgState.Info)
	if infoChanged || displayChanged {
		msgState.LastEvent = time.Now()
	}
	b.applyPendingPartsForMessageLocked(state, msgState.Info.ID)
	return infoChanged || displayChanged, completedNow || displayChanged
}

func (b *Bot) applyMessagePartUpdatedEventLocked(state *streamingState, sessionID string, raw json.RawMessage) (bool, bool) {
	var payload opencode.MessagePartUpdatedProperties
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, false
	}

	part := payload.Part
	if part.MessageID == "" || part.SessionID == "" || part.SessionID != sessionID {
		return false, false
	}
	if part.MessageID == state.requestMessageID {
		state.requestObserved = true
		return false, false
	}
	if state.requestMessageID == "" && !state.requestObserved {
		if state.initialMessageIDs == nil || !state.initialMessageIDs[part.MessageID] {
			if strings.TrimSpace(part.Type) == "text" && strings.TrimSpace(state.requestText) != "" {
				partText := strings.TrimSpace(part.Text)
				reqText := strings.TrimSpace(state.requestText)
				if partText == reqText {
					state.requestMessageID = part.MessageID
					state.requestObserved = true
					return false, false
				}
			}
		}
	}
	if !b.shouldTrackEventMessageLocked(state, part.MessageID, 0) {
		if state.initialMessageIDs != nil && state.initialMessageIDs[part.MessageID] {
			return false, false
		}
		if state.pendingEventParts == nil {
			state.pendingEventParts = make(map[string][]pendingEventPart)
		}
		state.pendingEventParts[part.MessageID] = append(state.pendingEventParts[part.MessageID], pendingEventPart{
			Part:  part,
			Delta: payload.Delta,
		})
		return false, false
	}

	msgState := b.getOrCreateEventMessageStateLocked(state, part.MessageID)
	if strings.TrimSpace(part.ID) == "" {
		part.ID = synthesizePartID(part, 0)
	}
	changed := upsertEventPartLocked(msgState, part, payload.Delta)
	return changed, false
}

func (b *Bot) isRequestUserMessageLocked(state *streamingState, info opencode.MessageInfo) bool {
	if state == nil || info.ID == "" {
		return false
	}
	if state.requestMessageID != "" {
		return info.ID == state.requestMessageID
	}
	if state.initialMessageIDs != nil && state.initialMessageIDs[info.ID] {
		return false
	}
	if info.Time.Created > 0 && state.requestStartedAt > 0 && info.Time.Created+1500 < state.requestStartedAt {
		return false
	}
	return true
}

func (b *Bot) applyPendingPartsForMessageLocked(state *streamingState, messageID string) {
	if state == nil || messageID == "" || len(state.pendingEventParts) == 0 {
		return
	}
	pending := state.pendingEventParts[messageID]
	if len(pending) == 0 {
		delete(state.pendingEventParts, messageID)
		return
	}

	msgState := b.getOrCreateEventMessageStateLocked(state, messageID)
	for _, item := range pending {
		part := item.Part
		if strings.TrimSpace(part.ID) == "" {
			part.ID = synthesizePartID(part, 0)
		}
		_ = upsertEventPartLocked(msgState, part, item.Delta)
	}
	delete(state.pendingEventParts, messageID)
}

func (b *Bot) shouldTrackEventMessageLocked(state *streamingState, messageID string, created int64) bool {
	if messageID == "" {
		return false
	}
	if _, tracked := state.eventMessages[messageID]; tracked {
		return true
	}
	if state.initialMessageIDs != nil && state.initialMessageIDs[messageID] {
		return false
	}
	if created > 0 && state.requestStartedAt > 0 {
		return created+1500 >= state.requestStartedAt
	}
	return state.requestObserved
}

func (b *Bot) getOrCreateEventMessageStateLocked(state *streamingState, messageID string) *eventMessageState {
	if msgState, exists := state.eventMessages[messageID]; exists {
		return msgState
	}
	msgState := &eventMessageState{
		Info: opencode.MessageInfo{
			ID: messageID,
		},
		PartOrder: make([]string, 0, 8),
		Parts:     make(map[string]opencode.MessagePartResponse),
	}
	state.eventMessages[messageID] = msgState
	return msgState
}

func mergeMessageInfo(existing, incoming opencode.MessageInfo) opencode.MessageInfo {
	merged := existing
	if incoming.ID != "" {
		merged.ID = incoming.ID
	}
	if incoming.SessionID != "" {
		merged.SessionID = incoming.SessionID
	}
	if incoming.Role != "" {
		merged.Role = incoming.Role
	}
	if incoming.Time.Created > 0 {
		merged.Time.Created = incoming.Time.Created
	}
	if incoming.Time.Completed > 0 {
		merged.Time.Completed = incoming.Time.Completed
	}
	if incoming.Error != nil {
		merged.Error = incoming.Error
	}
	if incoming.ParentID != "" {
		merged.ParentID = incoming.ParentID
	}
	if incoming.ModelID != "" {
		merged.ModelID = incoming.ModelID
	}
	if incoming.ProviderID != "" {
		merged.ProviderID = incoming.ProviderID
	}
	if incoming.Mode != "" {
		merged.Mode = incoming.Mode
	}
	if incoming.Agent != "" {
		merged.Agent = incoming.Agent
	}
	if incoming.Path != nil {
		merged.Path = incoming.Path
	}
	if incoming.Cost != 0 {
		merged.Cost = incoming.Cost
	}
	if incoming.Tokens != nil {
		merged.Tokens = incoming.Tokens
	}
	if incoming.Finish != "" {
		merged.Finish = incoming.Finish
	}
	if incoming.Summary != nil {
		merged.Summary = incoming.Summary
	}
	return merged
}

func sameMessageInfoForRender(left, right opencode.MessageInfo) bool {
	if left.ID != right.ID || left.Role != right.Role || left.SessionID != right.SessionID {
		return false
	}
	if left.Time.Created != right.Time.Created || left.Time.Completed != right.Time.Completed {
		return false
	}
	if left.Finish != right.Finish {
		return false
	}
	if (left.Error == nil) != (right.Error == nil) {
		return false
	}
	return true
}

func (b *Bot) enqueueEventMessageLocked(state *streamingState, messageID string) bool {
	if messageID == "" {
		return false
	}

	if state.activeMessageID == "" {
		state.activeMessageID = messageID
		if !state.displaySet[messageID] {
			state.displaySet[messageID] = true
			state.displayOrder = append(state.displayOrder, messageID)
		}
		return true
	}

	if state.activeMessageID == messageID || state.displaySet[messageID] || state.pendingSet[messageID] {
		return false
	}

	state.pendingSet[messageID] = true
	state.pendingOrder = append(state.pendingOrder, messageID)
	return false
}

func (b *Bot) tryPromoteNextActiveMessage(state *streamingState) bool {
	if state.activeMessageID == "" {
		return false
	}
	active := state.eventMessages[state.activeMessageID]
	if !isEventMessageCompleted(active) {
		return false
	}
	if len(state.pendingOrder) == 0 {
		return false
	}

	next := state.pendingOrder[0]
	state.pendingOrder = state.pendingOrder[1:]
	delete(state.pendingSet, next)
	state.activeMessageID = next
	if !state.displaySet[next] {
		state.displaySet[next] = true
		state.displayOrder = append(state.displayOrder, next)
	}
	return true
}

func (b *Bot) eventPipelineSettledLocked(state *streamingState, sendCompleted bool) bool {
	if state == nil || !sendCompleted {
		return false
	}
	if len(state.pendingOrder) > 0 {
		return false
	}
	if state.activeMessageID == "" {
		return false
	}
	active := state.eventMessages[state.activeMessageID]
	return isEventMessageCompleted(active)
}

func (b *Bot) eventPipelineNoOutputCandidateLocked(state *streamingState, sendCompleted bool) bool {
	if state == nil || !sendCompleted {
		return false
	}
	if len(state.pendingOrder) > 0 {
		return false
	}
	if state.activeMessageID != "" {
		return false
	}
	return len(state.displayOrder) == 0 && !state.hasEventUpdates
}

func isEventMessageCompleted(msg *eventMessageState) bool {
	if msg == nil {
		return false
	}
	if msg.Info.Time.Completed > 0 {
		return true
	}
	if msg.Info.Finish != "" {
		return true
	}
	if msg.Info.Error != nil {
		return true
	}
	return false
}

func (b *Bot) buildEventDrivenDisplaysLocked(state *streamingState) []string {
	if state == nil {
		return nil
	}

	renderedMessages := make([]string, 0, len(state.displayOrder))
	for _, messageID := range state.displayOrder {
		msgState := state.eventMessages[messageID]
		if msgState == nil {
			continue
		}
		block := formatEventMessageForDisplay(msgState)
		if strings.TrimSpace(block) == "" {
			continue
		}
		renderedMessages = append(renderedMessages, block)
	}
	if len(renderedMessages) == 0 {
		if state.isComplete {
			return nil
		}
		return []string{"🤖 Processing..."}
	}

	content := strings.Join(renderedMessages, "\n\n")
	chunks := b.splitLongContentPreserveCodeBlocks(content)
	if len(chunks) == 0 {
		return []string{content}
	}
	return chunks
}

func formatEventMessageForDisplay(msg *eventMessageState) string {
	if msg == nil {
		return ""
	}

	roleLabel := "🤖 Assistant"
	switch strings.ToLower(strings.TrimSpace(msg.Info.Role)) {
	case "assistant":
		roleLabel = "🤖 Assistant"
	case "system":
		roleLabel = "⚙️ System"
	case "user":
		roleLabel = "👤 User"
	}

	var sb strings.Builder
	if msg.Info.Time.Created > 0 {
		ts := time.UnixMilli(msg.Info.Time.Created).Format("15:04")
		fmt.Fprintf(&sb, "[%s] [%s]\n", roleLabel, ts)
		sb.WriteString("---\n")
	} else {
		fmt.Fprintf(&sb, "[%s]\n---\n", roleLabel)
	}

	partStr := formatEventMessageParts(sortedEventParts(msg))
	if partStr != "" {
		sb.WriteString(partStr)
	}
	if msg.Info.Error != nil {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("⚠️ Execution ended with an error.")
	}
	return strings.TrimSpace(sb.String())
}

func sortedEventParts(msg *eventMessageState) []opencode.MessagePartResponse {
	if msg == nil || len(msg.Parts) == 0 {
		return nil
	}

	return orderedParts(msg.PartOrder, msg.Parts)
}

func formatEventMessageParts(parts []opencode.MessagePartResponse) string {
	if len(parts) == 0 {
		return ""
	}

	var reasoningAndTools strings.Builder
	var textContent strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text":
			textContent.WriteString(part.Text)
		case "reasoning":
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			reasoningText := strings.ReplaceAll(strings.TrimSpace(part.Text), "\n", "\n> ")
			fmt.Fprintf(&reasoningAndTools, "> Thinking: %s\n", reasoningText)
		case "tool":
			reasoningAndTools.WriteString(formatToolCallPart(part.Tool, part.Snapshot, part.State, part.Text))
		case "step-start", "step-finish":
			// Step boundaries are structural markers; skip to keep stream concise.
		default:
			if strings.TrimSpace(part.Text) != "" {
				reasoningAndTools.WriteString(strings.TrimSpace(part.Text))
				reasoningAndTools.WriteString("\n")
			}
		}
	}

	sections := make([]string, 0, 2)
	if meta := strings.TrimSpace(reasoningAndTools.String()); meta != "" {
		sections = append(sections, meta)
	}
	if text := strings.TrimSpace(textContent.String()); text != "" {
		sections = append(sections, text)
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func (b *Bot) reconcileEventStateWithLatestMessages(state *streamingState) bool {
	if state == nil || state.sessionID == "" || b.opencodeClient == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(state.ctx, 4*time.Second)
	defer cancel()

	messages, err := b.opencodeClient.GetMessages(ctx, state.sessionID)
	if err != nil {
		log.Warnf("Failed to reconcile event state from message snapshots: %v", err)
		return false
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].CreatedAt.Before(messages[j].CreatedAt)
	})

	requestObserved := false
	observedRequestMessageID := ""
	if state.requestMessageID != "" {
		for _, msg := range messages {
			if msg.ID == state.requestMessageID {
				requestObserved = true
				observedRequestMessageID = msg.ID
				break
			}
		}
	} else {
		reqText := strings.TrimSpace(state.requestText)
		for _, msg := range messages {
			if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" || msg.ID == "" {
				continue
			}
			if state.initialMessageIDs != nil && state.initialMessageIDs[msg.ID] {
				continue
			}
			created := int64(0)
			if !msg.CreatedAt.IsZero() {
				created = msg.CreatedAt.UnixMilli()
			}
			if created > 0 && state.requestStartedAt > 0 && created+1500 < state.requestStartedAt {
				continue
			}
			if reqText != "" {
				msgText := strings.TrimSpace(msg.Content)
				if msgText != "" && msgText != reqText && !strings.Contains(msgText, reqText) && !strings.Contains(reqText, msgText) {
					continue
				}
			}
			requestObserved = true
			observedRequestMessageID = msg.ID
			break
		}
	}

	state.updateMutex.Lock()
	defer state.updateMutex.Unlock()
	if requestObserved {
		changed := !state.requestObserved || (observedRequestMessageID != "" && state.requestMessageID != observedRequestMessageID)
		state.requestObserved = true
		if observedRequestMessageID != "" {
			state.requestMessageID = observedRequestMessageID
		}
		if changed {
			log.Infof("Observed request message in OpenCode snapshot: session=%s request_trace_id=%s request_message_id=%s", state.sessionID, state.requestTraceID, state.requestMessageID)
		}
	}
	b.reconcileEventStateWithMessagesLocked(state, messages)
	return state.requestObserved
}

func (b *Bot) reconcileEventStateWithMessagesLocked(state *streamingState, messages []opencode.Message) {
	if state == nil {
		return
	}

	stateChanged := false
	for _, msg := range messages {
		if msg.ID == "" {
			continue
		}
		created := int64(0)
		if !msg.CreatedAt.IsZero() {
			created = msg.CreatedAt.UnixMilli()
		}
		track, fromChangedInitial := b.shouldTrackSnapshotMessageLocked(state, msg, created)
		if !track {
			continue
		}
		if fromChangedInitial {
			log.Infof("Tracking updated initial message from snapshot: session=%s message_id=%s", state.sessionID, msg.ID)
		}

		msgState := b.getOrCreateEventMessageStateLocked(state, msg.ID)
		prevInfo := msgState.Info
		msgState.Info = mergeMessageInfo(msgState.Info, opencode.MessageInfo{
			ID:        msg.ID,
			SessionID: state.sessionID,
			Role:      msg.Role,
			Time: opencode.MessageTime{
				Created: created,
			},
			Finish:     msg.Finish,
			ModelID:    msg.ModelID,
			ProviderID: msg.ProviderID,
		})
		if msg.Finish != "" && msgState.Info.Time.Completed == 0 {
			msgState.Info.Time.Completed = created
		}
		if !sameMessageInfoForRender(prevInfo, msgState.Info) {
			msgState.LastEvent = time.Now()
			stateChanged = true
		}

		nextParts, nextOrder, partsChanged := buildSnapshotMessageParts(msg.Parts, msgState)
		if partsChanged {
			msgState.Parts = nextParts
			msgState.PartOrder = nextOrder
			msgState.LastEvent = time.Now()
			stateChanged = true
		}

		if role := strings.ToLower(strings.TrimSpace(msgState.Info.Role)); role != "user" && role != "" {
			if b.enqueueEventMessageLocked(state, msg.ID) {
				stateChanged = true
			}
		}
	}

	for b.tryPromoteNextActiveMessage(state) {
		stateChanged = true
	}
	if stateChanged {
		state.hasEventUpdates = true
		state.lastEventAt = time.Now()
		state.revision++
	}
}

func (b *Bot) shouldTrackSnapshotMessageLocked(state *streamingState, msg opencode.Message, created int64) (track bool, fromChangedInitial bool) {
	if state == nil || msg.ID == "" {
		return false, false
	}
	if _, tracked := state.eventMessages[msg.ID]; tracked {
		return true, false
	}
	if state.initialMessageIDs != nil && state.initialMessageIDs[msg.ID] {
		return false, false
	}
	if created > 0 && state.requestStartedAt > 0 && created+500 < state.requestStartedAt {
		return false, false
	}
	return true, false
}

func snapshotMessageDigest(msg opencode.Message) string {
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(msg.ID))
	sb.WriteString("|")
	sb.WriteString(strings.TrimSpace(msg.Role))
	sb.WriteString("|")
	sb.WriteString(strings.TrimSpace(msg.Finish))
	sb.WriteString("|")
	sb.WriteString(strings.TrimSpace(msg.ModelID))
	sb.WriteString("|")
	sb.WriteString(strings.TrimSpace(msg.ProviderID))
	sb.WriteString("|")
	if !msg.CreatedAt.IsZero() {
		sb.WriteString(strconv.FormatInt(msg.CreatedAt.UnixMilli(), 10))
	}
	sb.WriteString("|parts=")
	sb.WriteString(strconv.Itoa(len(msg.Parts)))

	for _, rawPart := range msg.Parts {
		part, ok := rawPart.(opencode.MessagePartResponse)
		if !ok {
			continue
		}
		sb.WriteString("|")
		sb.WriteString(strings.TrimSpace(part.ID))
		sb.WriteString("|")
		sb.WriteString(strings.TrimSpace(part.Type))
		sb.WriteString("|")
		sb.WriteString(strings.TrimSpace(part.Text))
		sb.WriteString("|")
		sb.WriteString(strings.TrimSpace(part.Tool))
		sb.WriteString("|")
		sb.WriteString(strings.TrimSpace(part.Snapshot))
		sb.WriteString("|")
		sb.WriteString(strings.TrimSpace(part.Reason))
		sb.WriteString("|")
		sb.WriteString(strings.TrimSpace(part.CallID))
		sb.WriteString("|")
		sb.WriteString(strings.TrimSpace(stringifyToolValue(part.State)))
	}
	return sb.String()
}

func upsertEventPartLocked(msgState *eventMessageState, part opencode.MessagePartResponse, delta string) bool {
	if msgState == nil {
		return false
	}

	partID := strings.TrimSpace(part.ID)
	if partID == "" {
		partID = synthesizePartID(part, 0)
		part.ID = partID
	}

	part = normalizeIncomingPart(part, delta)

	existing, exists := msgState.Parts[partID]
	if exists {
		part = mergeEventPart(existing, part, delta)
	}
	if exists && sameEventPart(existing, part) {
		return false
	}

	if !exists {
		msgState.PartOrder = append(msgState.PartOrder, partID)
	}
	msgState.Parts[partID] = part
	msgState.LastEvent = time.Now()
	return true
}

func normalizeIncomingPart(part opencode.MessagePartResponse, delta string) opencode.MessagePartResponse {
	_ = delta
	return part
}

func mergeEventPart(existing, incoming opencode.MessagePartResponse, delta string) opencode.MessagePartResponse {
	merged := incoming

	if merged.Type == "" {
		merged.Type = existing.Type
	}
	if merged.SessionID == "" {
		merged.SessionID = existing.SessionID
	}
	if merged.MessageID == "" {
		merged.MessageID = existing.MessageID
	}
	if merged.Tool == "" {
		merged.Tool = existing.Tool
	}
	if merged.Snapshot == "" {
		merged.Snapshot = existing.Snapshot
	}
	if merged.Reason == "" {
		merged.Reason = existing.Reason
	}
	if merged.CallID == "" {
		merged.CallID = existing.CallID
	}
	if merged.State == nil {
		merged.State = existing.State
	}

	if isAppendOnlyPartType(merged.Type) {
		merged.Text = mergeAppendOnlyText(existing.Text, merged.Text, delta)
	}

	return merged
}

func isAppendOnlyPartType(partType string) bool {
	switch strings.ToLower(strings.TrimSpace(partType)) {
	case "text", "reasoning":
		return true
	default:
		return false
	}
}

func mergeAppendOnlyText(existingText, incomingText, delta string) string {
	if incomingText == "" && delta != "" {
		if existingText == "" {
			return delta
		}
		if strings.HasSuffix(existingText, delta) {
			return existingText
		}
		return existingText + delta
	}

	if existingText == "" {
		return incomingText
	}
	if incomingText == "" {
		return existingText
	}

	// Replayed historical snapshots should not regress rendered content.
	if strings.HasPrefix(existingText, incomingText) {
		return existingText
	}
	// Normal forward growth.
	if strings.HasPrefix(incomingText, existingText) {
		return incomingText
	}
	// Prefer the longer candidate to avoid oscillation on reconnect replay.
	if len(incomingText) < len(existingText) {
		return existingText
	}
	return incomingText
}

func buildSnapshotMessageParts(rawParts []interface{}, msgState *eventMessageState) (map[string]opencode.MessagePartResponse, []string, bool) {
	nextParts := make(map[string]opencode.MessagePartResponse, len(rawParts))
	nextOrder := make([]string, 0, len(rawParts))
	seenOrder := make(map[string]bool, len(rawParts))
	occurrences := make(map[string]int)

	for _, rawPart := range rawParts {
		part, ok := rawPart.(opencode.MessagePartResponse)
		if !ok {
			continue
		}

		partID := strings.TrimSpace(part.ID)
		if partID == "" {
			partID = fallbackSnapshotPartID(part, occurrences)
			part.ID = partID
		}
		if !seenOrder[partID] {
			nextOrder = append(nextOrder, partID)
			seenOrder[partID] = true
		}
		nextParts[partID] = part
	}

	if msgState == nil {
		return nextParts, nextOrder, len(nextOrder) > 0
	}

	currentOrdered := orderedParts(msgState.PartOrder, msgState.Parts)
	nextOrdered := orderedParts(nextOrder, nextParts)
	if sameOrderedPartContent(currentOrdered, nextOrdered) {
		return nextParts, nextOrder, false
	}
	return nextParts, nextOrder, true
}

func fallbackSnapshotPartID(part opencode.MessagePartResponse, occurrences map[string]int) string {
	signature := snapshotPartSignature(part)
	index := occurrences[signature]
	occurrences[signature] = index + 1
	return synthesizePartID(part, index)
}

func synthesizePartID(part opencode.MessagePartResponse, occurrence int) string {
	if callID := strings.TrimSpace(part.CallID); callID != "" {
		if occurrence <= 0 {
			return "call:" + callID
		}
		return fmt.Sprintf("call:%s:%d", callID, occurrence)
	}

	signature := snapshotPartSignature(part)
	if occurrence <= 0 {
		return "synth:" + signature
	}
	return fmt.Sprintf("synth:%s:%d", signature, occurrence)
}

func snapshotPartSignature(part opencode.MessagePartResponse) string {
	partType := strings.TrimSpace(part.Type)
	if partType == "" {
		partType = "part"
	}
	callID := strings.TrimSpace(part.CallID)
	tool := strings.TrimSpace(part.Tool)
	reason := strings.TrimSpace(part.Reason)
	if reason == "" {
		reason = "-"
	}
	if callID == "" {
		callID = "-"
	}
	if tool == "" {
		tool = "-"
	}
	return fmt.Sprintf("%s|tool=%s|call=%s|reason=%s", partType, tool, callID, reason)
}

func orderedParts(partOrder []string, parts map[string]opencode.MessagePartResponse) []opencode.MessagePartResponse {
	if len(parts) == 0 {
		return nil
	}

	ordered := make([]opencode.MessagePartResponse, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, partID := range partOrder {
		part, exists := parts[partID]
		if !exists {
			continue
		}
		ordered = append(ordered, part)
		seen[partID] = true
	}

	if len(ordered) == len(parts) {
		return ordered
	}

	remainingIDs := make([]string, 0, len(parts)-len(ordered))
	for partID := range parts {
		if seen[partID] {
			continue
		}
		remainingIDs = append(remainingIDs, partID)
	}
	sort.Strings(remainingIDs)
	for _, partID := range remainingIDs {
		ordered = append(ordered, parts[partID])
	}
	return ordered
}

func sameOrderedPartContent(left, right []opencode.MessagePartResponse) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !sameEventPartContent(left[i], right[i]) {
			return false
		}
	}
	return true
}

func sameEventPartContent(left, right opencode.MessagePartResponse) bool {
	if left.Type != right.Type || left.Text != right.Text {
		return false
	}
	if left.Tool != right.Tool || left.Snapshot != right.Snapshot || left.Reason != right.Reason {
		return false
	}
	if left.CallID != right.CallID {
		return false
	}
	if stringifyToolValue(left.State) != stringifyToolValue(right.State) {
		return false
	}
	return true
}

func sameEventPart(left, right opencode.MessagePartResponse) bool {
	if left.ID != right.ID || left.Type != right.Type || left.Text != right.Text {
		return false
	}
	if left.Tool != right.Tool || left.Snapshot != right.Snapshot || left.Reason != right.Reason {
		return false
	}
	if stringifyToolValue(left.State) != stringifyToolValue(right.State) {
		return false
	}
	return true
}

// formatMessageForDisplay formats a message for Telegram display
func (b *Bot) formatMessageForDisplay(msg opencode.Message, isCompleted bool) string {
	var sb strings.Builder

	// Add header only for completed tasks
	if isCompleted {
		sb.WriteString("✅ Task completed\n\n")
	}

	// Add message content if available
	if msg.Content != "" {
		content := msg.Content
		if len(content) > 3000 {
			content = content[:3000] + "...\n\n(content too long, truncated)"
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	// Add detailed parts information
	if len(msg.Parts) > 0 {
		partsStr := formatMessagePartsWithOptions(msg.Parts, msg.Content == "")
		if partsStr != "No detailed content" {
			sb.WriteString("📋 Processing Details:\n")
			sb.WriteString(partsStr)
			sb.WriteString("\n\n")
		}
	}

	// Add status
	if isCompleted {
		sb.WriteString("📊 Status: Task completed")
		if msg.Finish != "" {
			fmt.Fprintf(&sb, " (Reason: %s)", msg.Finish)
		}
		if msg.ModelID != "" {
			fmt.Fprintf(&sb, "\n🤖 Model: %s", msg.ModelID)
		}
	} else {
		// For ongoing tasks, only show the auto-update indicator at the end
		// Don't show redundant status lines
		if msg.Content == "" && len(msg.Parts) == 0 {
			// If no content yet, show minimal status
			sb.WriteString("🤖 Processing...")
		}
		sb.WriteString("\n\n⏳ Auto-updating...")
	}

	return sb.String()
}

// updateTelegramMessage updates a Telegram message with new content.
func (b *Bot) updateTelegramMessage(c telebot.Context, msg *telebot.Message, content string, streaming bool) {
	if msg == nil {
		log.Warn("updateTelegramMessage called with nil message")
		return
	}

	safeChunks := b.ensureTelegramRenderSafeDisplays([]string{content}, streaming)
	if len(safeChunks) == 0 {
		return
	}
	content = safeChunks[0]

	rendered := b.buildTelegramRenderResult(content, streaming)
	primary := rendered.primaryText
	if len(primary) > telegramMessageMaxLength {
		log.Warnf("Skipping Telegram edit because rendered content exceeds limit (%d)", len(primary))
		return
	}

	if msg.Text == primary {
		log.Debug("Skipping Telegram edit because message content is unchanged")
		return
	}

	// Try to edit with the preferred mode
	_, err := b.editTelegramWithMode(c, msg, primary, rendered.primaryMode)
	if err != nil {
		if isMessageNotModifiedError(err) {
			log.Debugf("Skipping no-op Telegram edit: %v", err)
			msg.Text = primary
			return
		}

		// If it's an HTML parse error and we're using HTML mode, try editing with plain text
		if isHTMLParseError(err) && rendered.primaryMode == telebot.ModeHTML {
			log.Warnf("HTML parse error during edit, trying plain text: %v", err)
			_, err = b.editTelegramWithMode(c, msg, primary, telebot.ModeDefault)
		}

		if err != nil {
			log.Warnf("Failed to update Telegram message: %v", err)
			// If editing fails, try to send a new message with fallback handling
			var newMsg *telebot.Message
			newMsg, err = b.sendRenderedTelegramMessage(c, content, streaming)
			if err != nil {
				log.Errorf("Failed to send new message: %v", err)
				return
			}
			// Update the message reference for future updates
			*msg = *newMsg
			log.Debugf("Sent new message due to edit failure, new message ID: %d", newMsg.ID)
		} else {
			msg.Text = primary
			log.Debugf("Successfully edited message ID %d with plain text fallback", msg.ID)
		}
	} else {
		msg.Text = primary
		log.Debugf("Successfully edited message ID %d", msg.ID)
	}
}

func isMessageNotModifiedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "message is not modified")
}

func isHTMLParseError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "can't parse entities") ||
		strings.Contains(errStr, "bad request") ||
		strings.Contains(errStr, "html") ||
		strings.Contains(errStr, "parse")
}

type telegramRenderResult struct {
	primaryText string
	primaryMode telebot.ParseMode
}

func (b *Bot) buildTelegramRenderResult(content string, streaming bool) telegramRenderResult {
	if b.renderer == nil {
		return telegramRenderResult{
			primaryText: content,
			primaryMode: telebot.ModeDefault,
		}
	}

	rendered := b.renderer.Render(content, streaming)
	return telegramRenderResult{
		primaryText: rendered.Text,
		primaryMode: telebot.ModeHTML,
	}
}

func (b *Bot) sendTelegramWithMode(c telebot.Context, text string, mode telebot.ParseMode) (*telebot.Message, error) {
	if mode == telebot.ModeDefault {
		return c.Bot().Send(c.Chat(), text)
	}
	return c.Bot().Send(c.Chat(), text, mode)
}

func (b *Bot) editTelegramWithMode(c telebot.Context, msg *telebot.Message, text string, mode telebot.ParseMode) (*telebot.Message, error) {
	if mode == telebot.ModeDefault {
		return c.Bot().Edit(msg, text)
	}
	return c.Bot().Edit(msg, text, mode)
}

func (b *Bot) sendRenderedTelegramMessage(c telebot.Context, content string, streaming bool) (*telebot.Message, error) {
	safeChunks := b.ensureTelegramRenderSafeDisplays([]string{content}, streaming)
	if len(safeChunks) == 0 {
		return nil, fmt.Errorf("empty content after render-safe pagination")
	}
	if len(safeChunks) > 1 {
		log.Warnf("sendRenderedTelegramMessage received multi-page content, sending first page only")
	}
	content = safeChunks[0]

	rendered := b.buildTelegramRenderResult(content, streaming)
	primary := rendered.primaryText
	if len(primary) > telegramMessageMaxLength {
		return nil, fmt.Errorf("rendered content exceeds telegram limit: %d", len(primary))
	}

	// Try sending with the preferred mode (usually HTML)
	msg, err := b.sendTelegramWithMode(c, primary, rendered.primaryMode)

	// If it's an HTML parse error and we're using HTML mode, fall back to plain text
	if err != nil && isHTMLParseError(err) && rendered.primaryMode == telebot.ModeHTML {
		log.Warnf("HTML parse error, falling back to plain text: %v", err)
		// Retry with plain text mode
		msg, err = b.sendTelegramWithMode(c, primary, telebot.ModeDefault)
	}

	if err == nil {
		msg.Text = primary
	}
	return msg, err
}

// handleRename handles the /rename command
func (b *Bot) handleRename(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()

	if len(args) < 2 {
		return c.Send("Usage: /rename <number> <new name>\nExample: /rename 2 \"My New Session Name\"")
	}

	sessionInput := args[0]
	newName := strings.Join(args[1:], " ")

	// Validate new name
	if strings.TrimSpace(newName) == "" {
		return c.Send("Session name cannot be empty.")
	}

	// Resolve session ID from input (number or session ID)
	var sessionID string
	if num, err := strconv.Atoi(sessionInput); err == nil {
		// Input is a number, get session ID from mapping
		b.sessionMappingMu.RLock()
		userMapping, exists := b.sessionMapping[userID]
		b.sessionMappingMu.RUnlock()

		if !exists {
			return c.Send("Session mapping not found. Please use /sessions first to see available sessions.")
		}

		mappedSessionID, found := userMapping[num]
		if !found {
			return c.Send(fmt.Sprintf("Session number %d not found. Use /sessions to see available sessions.", num))
		}
		sessionID = mappedSessionID
	} else {
		// Input is not a number, treat as session ID
		sessionID = sessionInput
	}

	// Rename session
	if err := b.sessionManager.RenameSession(b.ctx, c.Sender().ID, sessionID, newName); err != nil {
		log.Errorf("Failed to rename session: %v", err)
		return c.Send(fmt.Sprintf("Failed to rename session: %v", err))
	}

	return c.Send(fmt.Sprintf("✅ Session renamed to '%s'", newName))
}

// handleDelete handles the /delete command
func (b *Bot) handleDelete(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()

	if len(args) == 0 {
		return c.Send("Usage: /delete <number>\nExample: /delete 2")
	}

	sessionInput := args[0]

	// Resolve session ID from input (number or session ID)
	var sessionID string
	if num, err := strconv.Atoi(sessionInput); err == nil {
		// Input is a number, get session ID from mapping
		b.sessionMappingMu.RLock()
		userMapping, exists := b.sessionMapping[userID]
		b.sessionMappingMu.RUnlock()

		if !exists {
			return c.Send("Session mapping not found. Please use /sessions first to see available sessions.")
		}

		mappedSessionID, found := userMapping[num]
		if !found {
			return c.Send(fmt.Sprintf("Session number %d not found. Use /sessions to see available sessions.", num))
		}
		sessionID = mappedSessionID
	} else {
		// Input is not a number, treat as session ID
		sessionID = sessionInput
	}

	// Delete session
	if err := b.sessionManager.DeleteSession(b.ctx, sessionID); err != nil {
		log.Errorf("Failed to delete session: %v", err)
		return c.Send(fmt.Sprintf("Failed to delete session: %v", err))
	}

	// Remove from session mapping if present
	b.sessionMappingMu.Lock()
	if userMapping, exists := b.sessionMapping[userID]; exists {
		// Find and remove the mapping entry for this session
		for num, mappedID := range userMapping {
			if mappedID == sessionID {
				delete(userMapping, num)
				break
			}
		}
		// If mapping becomes empty, remove it
		if len(userMapping) == 0 {
			delete(b.sessionMapping, userID)
		}
	}
	b.sessionMappingMu.Unlock()

	return c.Send("🗑️ Session deleted successfully.")
}

// handleStreamChunk processes a chunk of text from the streaming response
func (b *Bot) handleStreamChunk(state *streamingState, textChunk string) error {
	state.updateMutex.Lock()
	defer state.updateMutex.Unlock()

	// Prefer event-driven updates when available. Keep chunk handling only as fallback.
	if state.hasEventUpdates {
		return nil
	}

	// Some stream providers emit cumulative snapshots instead of pure deltas.
	// Normalize to incremental append to avoid duplicated content growth.
	currentBefore := state.content.String()
	delta := streamChunkDelta(currentBefore, textChunk)
	if delta == "" {
		return nil
	}
	state.content.WriteString(delta)
	currentContent := state.content.String()

	// Track content length for update decisions
	currentLength := len(currentContent)

	// Check if we should update the Telegram message
	// Update logic:
	// 1. Always update if it's been more than 2 seconds since last update
	// 2. Update if content has grown significantly (300+ chars) even if less than 2 seconds
	// 3. Limit updates to at most once per 0.5 seconds to avoid rate limiting
	now := time.Now()
	timeSinceLastUpdate := now.Sub(state.lastUpdate)

	// Estimate content growth since last update
	contentGrowth := len(delta)

	shouldUpdate := false

	// Try to get formatted displays from latest messages
	var formattedDisplays []string
	var streamDisplayCount int

	// Only try to fetch messages if we have a client and session ID
	if b.opencodeClient != nil && state.sessionID != "" {
		messages, err := b.opencodeClient.GetMessages(state.ctx, state.sessionID)
		if err == nil && len(messages) > 0 {
			// Get session metadata
			var sessionMeta *session.SessionMeta
			if b.sessionManager != nil {
				if meta, exists := b.sessionManager.GetSessionMeta(state.sessionID); exists {
					sessionMeta = meta
				}
			}
			formattedDisplays = b.buildDisplayChunksFromMessagesWithCache(messages, sessionMeta, state)
			streamDisplayCount = len(formattedDisplays)
		}
	}
	// If we couldn't get formatted displays, fall back to raw content splitting
	if formattedDisplays == nil {
		formattedDisplays = b.formatStreamingDisplays(currentContent)
		streamDisplayCount = len(formattedDisplays)
	}

	if streamDisplayCount > len(state.telegramMessages) {
		// As soon as a new Telegram part is needed, update immediately so
		// part 2/3... can begin streaming before completion.
		shouldUpdate = true
	} else if timeSinceLastUpdate >= 2*time.Second {
		shouldUpdate = true
	} else if timeSinceLastUpdate >= 500*time.Millisecond && contentGrowth >= 100 {
		// Significant content growth, update more frequently
		shouldUpdate = true
	} else if len(state.telegramMessages) > 1 && timeSinceLastUpdate >= 500*time.Millisecond && contentGrowth > 0 {
		// Once we are in multi-message streaming, keep incremental updates
		// responsive for later parts.
		shouldUpdate = true
	} else if currentLength < 1000 && timeSinceLastUpdate >= 1*time.Second {
		// For short content, update more frequently to show progress
		shouldUpdate = true
	}

	if !shouldUpdate {
		return nil
	}

	state.lastUpdate = now
	b.updateStreamingTelegramMessages(state, formattedDisplays)

	return nil
}

func streamChunkDelta(existing, chunk string) string {
	if chunk == "" {
		return ""
	}
	if existing == "" {
		return chunk
	}

	// Typical cumulative snapshot: chunk starts with all existing content.
	if strings.HasPrefix(chunk, existing) {
		return chunk[len(existing):]
	}
	// Stale or repeated shorter snapshot.
	if strings.HasPrefix(existing, chunk) || strings.Contains(existing, chunk) {
		return ""
	}
	// Chunk contains existing content in the middle (rare wrapper case).
	if idx := strings.Index(chunk, existing); idx >= 0 {
		return chunk[idx+len(existing):]
	}

	// Fallback to suffix/prefix overlap.
	overlap := longestSuffixPrefixOverlap(existing, chunk)
	if overlap > 0 {
		return chunk[overlap:]
	}
	return chunk
}

func longestSuffixPrefixOverlap(left, right string) int {
	max := len(left)
	if len(right) < max {
		max = len(right)
	}
	for i := max; i > 0; i-- {
		if left[len(left)-i:] == right[:i] {
			return i
		}
	}
	return 0
}

// formatStreamingContent formats streaming content for display
func (b *Bot) formatStreamingDisplays(content string) []string {
	// Trim trailing whitespace
	content = strings.TrimSpace(content)

	if content == "" {
		return []string{"🤖 Processing..."}
	}

	// Split full content into stable streaming pages so page 1 can keep updating
	// until full, then page 2 starts streaming, and so on.
	chunks := b.splitLongContent(content)
	if len(chunks) == 0 {
		return []string{"🤖 Processing..."}
	}

	displays := make([]string, 0, len(chunks))
	// Only output content, no progress indicators or pagination headers.
	displays = append(displays, chunks...)
	return b.ensureTelegramRenderSafeDisplays(displays, true)
}

func (b *Bot) ensureTelegramRenderSafeDisplays(displays []string, streaming bool) []string {
	if len(displays) == 0 {
		return nil
	}

	normalized := make([]string, 0, len(displays))
	for _, display := range displays {
		if display == "" {
			normalized = append(normalized, display)
			continue
		}

		safeParts := b.splitDisplayToTelegramSafe(display, streaming)
		normalized = append(normalized, safeParts...)
		if len(normalized) >= maxTelegramMessages {
			normalized = normalized[:maxTelegramMessages]
			last := len(normalized) - 1
			normalized[last] += "\n\n... (response too long, truncated)"
			return normalized
		}
	}
	return normalized
}

func (b *Bot) splitDisplayToTelegramSafe(content string, streaming bool) []string {
	if content == "" {
		return []string{""}
	}

	if b.renderedLengthWithinTelegramLimit(content, streaming) {
		return []string{content}
	}

	left, right := splitContentNearMiddle(content)
	if left == "" || right == "" || left == content || right == content {
		runes := []rune(content)
		if len(runes) <= 1 {
			return []string{content}
		}
		mid := len(runes) / 2
		left = string(runes[:mid])
		right = string(runes[mid:])
		if left == "" || right == "" {
			return []string{content}
		}
	}

	parts := b.splitDisplayToTelegramSafe(left, streaming)
	parts = append(parts, b.splitDisplayToTelegramSafe(right, streaming)...)
	return parts
}

func (b *Bot) renderedLengthWithinTelegramLimit(content string, streaming bool) bool {
	rendered := b.buildTelegramRenderResult(content, streaming)
	return len(rendered.primaryText) <= telegramMessageMaxLength
}

func splitContentNearMiddle(content string) (string, string) {
	runes := []rune(content)
	total := len(runes)
	if total <= 1 {
		return content, ""
	}

	mid := total / 2
	bestSplit := -1
	bestDistance := total + 1
	searchWindow := 300
	if searchWindow > mid {
		searchWindow = mid
	}
	for i := mid; i >= mid-searchWindow; i-- {
		if i <= 0 || i >= total {
			continue
		}
		if runes[i-1] == '\n' {
			dist := mid - i
			if dist < 0 {
				dist = -dist
			}
			if dist < bestDistance {
				bestDistance = dist
				bestSplit = i
			}
			break
		}
	}
	upper := mid + searchWindow
	if upper >= total {
		upper = total - 1
	}
	for i := mid; i <= upper; i++ {
		if i <= 0 || i >= total {
			continue
		}
		if runes[i-1] == '\n' {
			dist := i - mid
			if dist < 0 {
				dist = -dist
			}
			if dist < bestDistance {
				bestSplit = i
			}
			break
		}
	}

	if bestSplit <= 0 || bestSplit >= total {
		bestSplit = mid
	}
	return string(runes[:bestSplit]), string(runes[bestSplit:])
}

func (b *Bot) updateStreamingTelegramMessages(state *streamingState, displays []string) {
	if len(displays) == 0 || state.telegramCtx == nil {
		return
	}

	displays = b.ensureTelegramRenderSafeDisplays(displays, true)
	if len(displays) == 0 {
		return
	}

	// Limit number of messages to avoid flooding
	originalCount := len(displays)
	if originalCount > maxTelegramMessages {
		log.Warnf("Too many messages (%d), truncating to %d", originalCount, maxTelegramMessages)
		displays = displays[:maxTelegramMessages]
		// Add truncation notice to last message
		if len(displays) > 0 {
			lastIdx := len(displays) - 1
			displays[lastIdx] = displays[lastIdx] + "\n\n... (response too long, truncated)"
		}
	}

	// Ensure we have enough Telegram messages.
	for len(state.telegramMessages) < len(displays) {
		idx := len(state.telegramMessages)
		newMsg, err := b.sendRenderedTelegramMessage(state.telegramCtx, displays[idx], true)
		if err != nil {
			log.Errorf("Failed to create additional streaming message #%d: %v", idx+1, err)
			// Keep updating already-existing pages; we'll retry creating missing pages
			// on the next update cycle.
			displays = displays[:len(state.telegramMessages)]
			break
		}
		state.telegramMessages = append(state.telegramMessages, newMsg)
		state.lastRendered = append(state.lastRendered, displays[idx])
	}

	for i, display := range displays {
		if i < len(state.lastRendered) && state.lastRendered[i] == display {
			continue
		}
		b.updateTelegramMessage(state.telegramCtx, state.telegramMessages[i], display, true)
		if i < len(state.lastRendered) {
			state.lastRendered[i] = display
		}
	}

	if len(state.telegramMessages) > 0 {
		state.telegramMsg = state.telegramMessages[0]
	}
}

// splitLongContent splits long content into chunks that fit in Telegram messages
func (b *Bot) splitLongContent(content string) []string {
	const maxChunkSize = 3000
	if content == "" {
		return nil
	}

	var chunks []string
	remaining := content

	for len(remaining) > maxChunkSize {
		window := remaining[:maxChunkSize]
		splitAt := strings.LastIndex(window, "\n")
		if splitAt <= 0 {
			// No natural boundary in the window (or newline at position 0):
			// hard-split to guarantee progress for long single-line content.
			splitAt = maxChunkSize
		}

		chunk := remaining[:splitAt]
		chunks = append(chunks, chunk)

		// Stop if we've reached the maximum number of messages
		if len(chunks) >= maxTelegramMessages {
			// Add truncation notice to the last chunk
			chunks[maxTelegramMessages-1] = chunks[maxTelegramMessages-1] + "\n\n... (response too long, truncated)"
			return chunks
		}

		if splitAt < len(remaining) && remaining[splitAt] == '\n' {
			splitAt++
		}
		remaining = remaining[splitAt:]
	}

	if remaining != "" {
		chunks = append(chunks, remaining)
	}

	return chunks
}

func (b *Bot) splitLongContentPreserveCodeBlocks(content string) []string {
	const maxChunkSize = 3000
	if content == "" {
		return nil
	}

	// For very simple case, fall back to original splitLongContent
	// This handles long single lines without newlines
	if !strings.Contains(content, "\n") {
		return b.splitLongContent(content)
	}

	var chunks []string
	lines := strings.Split(content, "\n")

	var currentChunk strings.Builder
	inCodeBlock := false
	codeBlockMarker := ""
	codeBlockQuotePrefix := ""
	truncated := false

	for i, line := range lines {
		// If we've already truncated, stop processing
		if truncated {
			break
		}
		// Check if this line starts or ends a code block
		trimmed := strings.TrimSpace(line)
		normalizedFenceLine, quotePrefix := normalizeFenceLineForSplit(trimmed)
		if strings.HasPrefix(normalizedFenceLine, "```") {
			if !inCodeBlock {
				// Starting a code block
				inCodeBlock = true
				codeBlockMarker = normalizedFenceLine
				codeBlockQuotePrefix = quotePrefix
			} else {
				// Check if this ends the current code block
				if strings.HasPrefix(normalizedFenceLine, codeBlockMarker) ||
					(len(normalizedFenceLine) >= 3 && normalizedFenceLine[:3] == "```") {
					// Ending the code block
					inCodeBlock = false
				}
			}
		}

		lineWithNewline := line + "\n"

		// Check if adding this line would exceed chunk size
		if currentChunk.Len()+len(lineWithNewline) > maxChunkSize && currentChunk.Len() > 0 {
			// Current chunk is full, start a new one
			// If we're in a code block, close it before ending the chunk
			if inCodeBlock {
				currentChunk.WriteString(codeBlockQuotePrefix + "```\n")
			}

			chunkStr := strings.TrimSuffix(currentChunk.String(), "\n")
			chunks = append(chunks, chunkStr)

			// Check if we've reached the maximum number of messages
			if len(chunks) >= maxTelegramMessages {
				// Add truncation notice to the last chunk
				chunks[maxTelegramMessages-1] = chunks[maxTelegramMessages-1] + "\n\n... (response too long, truncated)"
				truncated = true
				break
			}

			currentChunk.Reset()

			// If we were in a code block, reopen it in the new chunk
			if inCodeBlock {
				currentChunk.WriteString(codeBlockQuotePrefix + codeBlockMarker + "\n")
			}
		}

		currentChunk.WriteString(lineWithNewline)

		// If this is the last line, finalize the chunk
		if i == len(lines)-1 && !truncated {
			chunkStr := strings.TrimSuffix(currentChunk.String(), "\n")
			// Check if we can add another chunk without exceeding limit
			if len(chunks) < maxTelegramMessages {
				chunks = append(chunks, chunkStr)
			} else if len(chunks) == maxTelegramMessages {
				// Already at limit, replace last chunk with current content plus truncation notice
				// (should not happen due to earlier break)
				chunks[maxTelegramMessages-1] = chunkStr + "\n\n... (response too long, truncated)"
			}
		}
	}

	return chunks
}

func normalizeFenceLineForSplit(trimmedLine string) (fenceLine string, quotePrefix string) {
	trimmedLine = strings.TrimSpace(trimmedLine)
	if strings.HasPrefix(trimmedLine, "```") {
		return trimmedLine, ""
	}

	if strings.HasPrefix(trimmedLine, ">") {
		noQuote := strings.TrimSpace(strings.TrimPrefix(trimmedLine, ">"))
		if strings.HasPrefix(noQuote, "```") {
			return noQuote, "> "
		}
	}
	return trimmedLine, ""
}

// Close closes the bot and releases resources
func (b *Bot) Close() error {
	if b.runtime != nil {
		b.runtime.Close()
	}

	if b.cancel != nil {
		b.cancel()
	}

	if b.sessionManager != nil {
		return b.sessionManager.Close()
	}
	return nil
}
