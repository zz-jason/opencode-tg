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
	ctx         context.Context
	cancel      context.CancelFunc
	stopUpdates chan struct{}

	telegramMsg      *telebot.Message
	telegramMessages []*telebot.Message
	lastRendered     []string
	telegramCtx      telebot.Context

	content     *strings.Builder
	lastUpdate  time.Time
	updateMutex *sync.Mutex

	// Track which OpenCode message IDs have been rendered
	renderedMessageIDs map[string]bool

	// Cache of formatted message chunks by message ID
	cachedMessageChunks map[string][]string

	// Cumulative display chunks (session info + all message chunks)
	allDisplayChunks []string

	// Whether session info has been added to display chunks
	sessionInfoAdded bool

	// Session ID for this streaming state
	sessionID        string
	requestMessageID string

	// Event-driven stream state for /event updates.
	requestStartedAt  int64
	initialMessageIDs map[string]bool
	eventMessages     map[string]*eventMessageState
	activeMessageID   string
	displayOrder      []string
	displaySet        map[string]bool
	pendingOrder      []string
	pendingSet        map[string]bool
	lastEventAt       time.Time
	hasEventUpdates   bool

	isStreaming bool
	isComplete  bool
}

type eventMessageState struct {
	Info      opencode.MessageInfo
	PartOrder []string
	Parts     map[string]opencode.MessagePartResponse
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
		log.Warnf("OpenCode health check failed: %v", healthErr)
		// Continue anyway, as the server might become available later
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

	// Initialize session manager asynchronously to preload sessions and models
	go func() {
		initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
		defer initCancel()

		if err := sessionManager.Initialize(initCtx); err != nil {
			log.Warnf("Failed to initialize session manager (preloading sessions/models): %v", err)
			log.Warn("Bot will start without preloaded sessions and models. Users will need to run /sessions and /models manually.")
		} else {
			log.Info("Session manager initialized successfully with preloaded sessions and models")

			// Build global model mapping after successful initialization
			bot.buildGlobalModelMapping(initCtx)
		}
	}()

	return bot, nil
}

// SetTelegramBot sets the Telegram bot instance
func (b *Bot) SetTelegramBot(tgBot *telebot.Bot) {
	b.tgBot = tgBot
}

// Start starts the bot and registers handlers
func (b *Bot) Start() {
	if b.tgBot == nil {
		log.Error("Telegram bot not set")
		return
	}

	// Register command handlers
	b.tgBot.Handle("/start", b.handleStart)
	b.tgBot.Handle("/help", b.handleHelp)
	b.tgBot.Handle("/sessions", b.handleSessions)
	b.tgBot.Handle("/new", b.handleNew)
	b.tgBot.Handle("/switch", b.handleSwitch)
	b.tgBot.Handle("/current", b.handleCurrent)
	b.tgBot.Handle("/abort", b.handleAbort)
	b.tgBot.Handle("/files", b.handleFiles)
	b.tgBot.Handle("/search", b.handleSearch)
	b.tgBot.Handle("/findfile", b.handleFindFile)
	b.tgBot.Handle("/symbol", b.handleSymbol)
	b.tgBot.Handle("/agent", b.handleAgent)
	b.tgBot.Handle("/command", b.handleCommand)
	b.tgBot.Handle("/status", b.handleStatus)
	b.tgBot.Handle("/models", b.handleModels)
	b.tgBot.Handle("/providers", b.handleProviders)
	b.tgBot.Handle("/setmodel", b.handleSetModel)
	b.tgBot.Handle("/rename", b.handleRename)
	b.tgBot.Handle("/delete", b.handleDelete)

	// Handle plain text messages (non-commands)
	b.tgBot.Handle(telebot.OnText, b.handleText)
}

// handleStart handles the /start command
func (b *Bot) handleStart(c telebot.Context) error {
	user := c.Sender()
	message := fmt.Sprintf(`👋 Hello %s!

Welcome to OpenCode Telegram Bot.

I am an AI programming assistant that can help you:
• Write and refactor code
• Answer programming questions
• Browse project files
• Search code and symbols

Basic commands:
/start - Show this help message
/help - Show detailed help
/sessions - List your sessions
/new [name] - Create a new session
/switch <sessionID> - Switch session
/current - Show current session
/status - Check current task status

Send any non-command text and I'll send it as an instruction to OpenCode.

Use /help to see all available commands.`, user.FirstName)

	return c.Send(message)
}

// handleHelp handles the /help command
func (b *Bot) handleHelp(c telebot.Context) error {
	helpText := `📚 OpenCode Bot Help

Core Commands:
• /start - Show welcome message
• /help - Show this help
• /sessions - List all sessions
• /new [name] - Create new session
• /switch <number> - Switch current session
• /rename <number> <name> - Rename a session
• /delete <number> - Delete a session
• /current - Show current session information
• /abort - Abort current task
• /status - Check current task status

File Operations:
• /files [path] - Browse project files (default: current directory)
• /search <pattern> - Search code text
• /findfile <pattern> - Search for files
• /symbol <symbol> - Search symbols (functions, classes, etc.)

System Information:
• /agent - List available AI agents
• /command - List available commands

Model Selection:
• /models - List available AI models (with numbers)
• /providers - List AI providers
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
		log.Warnf("Failed to synchronize sessions: %v", err)
		// Continue to show sessions anyway
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

// handleCurrent handles the /current command
func (b *Bot) handleCurrent(c telebot.Context) error {
	userID := c.Sender().ID
	sessionID, exists := b.sessionManager.GetUserSession(userID)

	if !exists {
		_, err := b.sendRenderedTelegramMessage(c, "You don't have a current session. Use /new to create a new session.", false)
		return err
	}

	meta, exists := b.sessionManager.GetSessionMeta(sessionID)
	if !exists {
		_, err := b.sendRenderedTelegramMessage(c, "Session information lost. Use /new to create a new session.", false)
		return err
	}

	// Get recent messages
	messages, err := b.opencodeClient.GetMessages(b.ctx, sessionID)
	if err != nil {
		log.Errorf("Failed to get messages: %v", err)
		_, sendErr := b.sendRenderedTelegramMessage(c, fmt.Sprintf("Failed to get messages: %v", err), false)
		return sendErr
	}

	// Get session details from OpenCode
	session, err := b.opencodeClient.GetSession(b.ctx, sessionID)
	if err != nil || session == nil {
		log.Errorf("Failed to get session details: %v", err)
		_, sendErr := b.sendRenderedTelegramMessage(c, fmt.Sprintf("Failed to get session details: %v", err), false)
		return sendErr
	}

	// Determine current status
	statusStr := "Porcessing finished."
	if len(messages) > 0 {
		lastMsg := messages[len(messages)-1]
		if !(lastMsg.Role == "assistant" && lastMsg.Finish != "") {
			statusStr = "Processing..."
		}
	}

	// Determine model info
	modelInfo := "Default"
	if meta.ProviderID != "" && meta.ModelID != "" {
		modelInfo = fmt.Sprintf("%s/%s", meta.ProviderID, meta.ModelID)
	}

	var sb strings.Builder

	// Show session info in bullet points (same format as /status)
	fmt.Fprintf(&sb, "## Session: %s\n", meta.Name)
	fmt.Fprintf(&sb, "---\n")
	fmt.Fprintf(&sb, "- Created: %s\n", time.UnixMilli(session.Time.Created).Format("2006-01-02 15:04"))
	fmt.Fprintf(&sb, "- Updated: %s\n", time.UnixMilli(session.Time.Updated).Format("2006-01-02 15:04"))
	fmt.Fprintf(&sb, "- Messages: %d\n", meta.MessageCount)
	fmt.Fprintf(&sb, "- Model: %s\n", modelInfo)
	fmt.Fprintf(&sb, "- Status: %s\n", statusStr)

	// Show latest message if available
	if len(messages) > 0 {
		msg := messages[len(messages)-1]
		var role string
		switch msg.Role {
		case "user":
			role = "User"
		case "assistant":
			role = "Assistant"
		case "system":
			role = "System"
		}
		messageMeta := fmt.Sprintf("[%s] [%s]", role, msg.CreatedAt.Format("15:04"))

		fmt.Fprintf(&sb, "\n")
		fmt.Fprintf(&sb, "## Latest Message %s\n", messageMeta)
		fmt.Fprintf(&sb, "---\n")

		// Show detailed parts if available
		if len(msg.Parts) > 0 {
			// Only include reply content in parts if message content is empty
			partsStr := formatMessagePartsWithOptions(msg.Parts, msg.Content == "")
			if partsStr != "No detailed content" {
				fmt.Fprintf(&sb, "%s\n", partsStr)
			}
			log.Infof("[DEBUG] zz-jason, partsStr: %s", partsStr)
		}

		// Show message content if available
		if msg.Content != "" {
			fmt.Fprintf(&sb, "%s\n", msg.Content)
		}

	} else {
		sb.WriteString("No messages yet.\n")
	}

	result := sb.String()

	pages := b.paginateDisplayText(result, false)
	var sendErr error
	for i, page := range pages {
		_, err := b.sendRenderedTelegramMessage(c, page, false)
		if err != nil {
			sendErr = err
			log.Errorf("Failed to send page %d/%d: %v", i+1, len(pages), err)
		}
	}
	return sendErr
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

		log.Infof("[DEBUG zz-jason] Processing opencode.MessagePartResponse")
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
	if toolName == "" {
		toolName = "tool"
	}

	sourceData := toStringAnyMap(state)
	if sourceData == nil {
		sourceData = snapshotData
	}

	emoji := "🛠️"
	if sourceData != nil {
		if status, _ := sourceData["status"].(string); strings.EqualFold(status, "completed") {
			emoji = "✅"
		} else if status, _ := sourceData["status"].(string); strings.EqualFold(status, "error") || strings.EqualFold(status, "failed") {
			emoji = "❌"
		}
	}

	description := extractToolDescription(sourceData, text)
	command := extractToolCommand(sourceData)
	output := extractToolOutput(sourceData)
	if output == "" && text != "" && text != description {
		output = text
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "• %s %s: %s\n", emoji, toolName, description)
	if command != "" {
		fmt.Fprintf(&sb, "  $ %s\n", truncateAndInline(command, 300))
	}
	if output != "" {
		outputText := truncateMultiline(output, 700)
		outputText = strings.ReplaceAll(outputText, "\r\n", "\n")
		outputText = strings.TrimSpace(outputText)
		if outputText != "" {
			sb.WriteString("  output:\n")
			sb.WriteString("    " + strings.ReplaceAll(outputText, "\n", "\n    ") + "\n")
		}
	}
	return sb.String()
}

// formatMessageWithMetadata formats a single OpenCode message with role, timestamp, parts, and content
// Follows the same logic as /current command for displaying messages
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

// formatAllMessagesForStreaming formats all messages for streaming display
// Returns an array of display strings, each suitable for a Telegram message
func (b *Bot) formatAllMessagesForStreaming(messages []opencode.Message, sessionMeta *session.SessionMeta) []string {
	if len(messages) == 0 {
		return []string{"No messages yet."}
	}

	var displays []string

	// If we have session metadata, add session info at the beginning
	if sessionMeta != nil {
		var sb strings.Builder
		fmt.Fprintf(&sb, "## Session: %s\n", sessionMeta.Name)
		sb.WriteString("---\n")
		fmt.Fprintf(&sb, "- Messages: %d\n", sessionMeta.MessageCount)
		if sessionMeta.ProviderID != "" && sessionMeta.ModelID != "" {
			fmt.Fprintf(&sb, "- Model: %s/%s\n", sessionMeta.ProviderID, sessionMeta.ModelID)
		}
		sb.WriteString("\n")
		sessionInfo := sb.String()
		// Split session info if needed (unlikely to be long)
		displays = append(displays, sessionInfo)
	}

	// Format each message
	for _, msg := range messages {
		formatted := formatMessageWithMetadata(msg)

		// Split long messages while preserving code blocks
		chunks := b.splitLongContentPreserveCodeBlocks(formatted)
		displays = append(displays, chunks...)
	}

	return displays
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

// handleStatus handles the /status command
func (b *Bot) handleStatus(c telebot.Context) error {
	userID := c.Sender().ID
	sessionID, exists := b.sessionManager.GetUserSession(userID)

	if !exists {
		return c.Send("You don't have a current session. Use /new to create a new session.")
	}

	// Get recent messages
	messages, err := b.opencodeClient.GetMessages(b.ctx, sessionID)
	if err != nil {
		log.Errorf("Failed to get messages: %v", err)
		return c.Send(fmt.Sprintf("Failed to get messages: %v", err))
	}

	if len(messages) == 0 {
		return c.Send("Current session has no messages yet.")
	}

	// Determine current status
	statusStr := "Waiting For Your Input"
	if len(messages) > 0 {
		lastMsg := messages[len(messages)-1]
		if !(lastMsg.Role == "assistant" && lastMsg.Finish != "") {
			statusStr = "Assistant is processing..."
		}
	}

	var sb strings.Builder
	sb.WriteString("📊 Session Status\n")
	sb.WriteString("────────────────\n")

	// Show session info
	session, err := b.opencodeClient.GetSession(b.ctx, sessionID)
	if err == nil && session != nil {
		fmt.Fprintf(&sb, "• Name: %s\n", session.Title)
		createdAt := time.UnixMilli(session.Time.Created)
		fmt.Fprintf(&sb, "• Created: %s\n", createdAt.Format("2006-01-02 15:04"))
	}

	fmt.Fprintf(&sb, "• Messages: %d\n", len(messages))
	fmt.Fprintf(&sb, "• Status: %s\n\n", statusStr)

	// Show last 3 messages in a cleaner format
	start := len(messages) - 3
	if start < 0 {
		start = 0
	}

	for i := start; i < len(messages); i++ {
		msg := messages[i]
		// Compute relative index (0 = latest, -1 = previous, -2 = older)
		relIndex := i - (len(messages) - 1)
		role := "👤 You"
		if msg.Role == "assistant" {
			role = "🤖 Assistant"
		} else if msg.Role == "system" {
			role = "⚙️ System"
		}
		timeStr := msg.CreatedAt.Format("15:04")

		fmt.Fprintf(&sb, "\n[Message %d] %s [%s]\n", relIndex, role, timeStr)
		sb.WriteString("────────────────\n")

		// Show message content
		if len(msg.Parts) > 0 {
			// Always show detailed parts if available, especially for tool calls
			partsStr := formatMessageParts(msg.Parts)
			if partsStr != "No detailed content" {
				fmt.Fprintf(&sb, "%s\n", partsStr)
			} else if msg.Content != "" {
				// Fallback to content if parts don't provide details
				content := msg.Content
				if len(content) > 400 {
					content = content[:400] + "..."
				}
				fmt.Fprintf(&sb, "%s\n", content)
			} else {
				sb.WriteString("(No content)\n")
			}
		} else if msg.Content != "" {
			// No parts, just show content
			content := msg.Content
			if len(content) > 400 {
				content = content[:400] + "..."
			}
			fmt.Fprintf(&sb, "%s\n", content)
		} else {
			sb.WriteString("(No content)\n")
		}

	}

	// Truncate if too long
	result := sb.String()
	if len(result) > 4000 {
		result = result[:4000] + "\n... (content too long, truncated)"
	}

	return c.Send(result)
}

// handleModels lists available AI models
func (b *Bot) handleModels(c telebot.Context) error {
	// Synchronize models from OpenCode to local storage
	if err := b.sessionManager.SyncModels(b.ctx); err != nil {
		log.Warnf("Failed to synchronize models: %v", err)
		// Continue to show models anyway
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
		sb.WriteString("Please configure API keys for at least one AI provider first.\n\n")
		sb.WriteString("Use /providers to view provider connection status.")
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

// handleProviders lists AI providers
func (b *Bot) handleProviders(c telebot.Context) error {
	providersResp, err := b.opencodeClient.GetProviders(b.ctx)
	if err != nil {
		log.Errorf("Failed to get providers: %v", err)
		return c.Send(fmt.Sprintf("Failed to get providers: %v", err))
	}

	// Create a set of connected provider IDs for faster lookup
	connectedSet := make(map[string]bool)
	for _, providerID := range providersResp.Connected {
		connectedSet[providerID] = true
	}

	var sb strings.Builder
	sb.WriteString("🏢 AI Providers\n\n")

	// Show connected providers first
	hasConnected := false
	for _, provider := range providersResp.All {
		if connectedSet[provider.ID] {
			if !hasConnected {
				sb.WriteString("✅ Connected Providers:\n\n")
				hasConnected = true
			}
			fmt.Fprintf(&sb, "✅ %s\n", provider.Name)
			fmt.Fprintf(&sb, "  ID: %s\n", provider.ID)
			fmt.Fprintf(&sb, "  Source: %s\n", provider.Source)
			if len(provider.Env) > 0 {
				fmt.Fprintf(&sb, "  Environment Variables: %s\n", strings.Join(provider.Env, ", "))
			}
			if len(provider.Models) > 0 {
				fmt.Fprintf(&sb, "  Models: %d\n", len(provider.Models))
			}
			sb.WriteString("\n")
		}
	}

	// Show unconnected providers
	hasUnconnected := false
	for _, provider := range providersResp.All {
		if !connectedSet[provider.ID] {
			if !hasUnconnected {
				sb.WriteString("⚠️ Unconnected Providers (API key required):\n\n")
				hasUnconnected = true
			}
			fmt.Fprintf(&sb, "⚪ %s\n", provider.Name)
			fmt.Fprintf(&sb, "  ID: %s\n", provider.ID)
			fmt.Fprintf(&sb, "  Source: %s\n", provider.Source)
			if len(provider.Env) > 0 {
				fmt.Fprintf(&sb, "  Required Environment Variables: %s\n", strings.Join(provider.Env, ", "))
			}
			if len(provider.Models) > 0 {
				fmt.Fprintf(&sb, "  Available Models: %d\n", len(provider.Models))
			}
			sb.WriteString("\n")
		}
	}

	// Summary
	sb.WriteString("📊 Summary:\n")
	fmt.Fprintf(&sb, "  • Connected: %d providers\n", len(providersResp.Connected))
	fmt.Fprintf(&sb, "  • Total: %d providers\n", len(providersResp.All))
	sb.WriteString("\n")

	sb.WriteString("Use /models to view available models from connected providers.")

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

	// Apply the model selection with timeout - model initialization can take time
	ctx, cancel := context.WithTimeout(b.ctx, 60*time.Second)
	defer cancel()

	log.Debugf("Calling SetSessionModel for session %s with model %s/%s", sessionID, selection.ProviderID, selection.ModelID)
	if err := b.sessionManager.SetSessionModel(ctx, sessionID, selection.ProviderID, selection.ModelID); err != nil {
		log.Errorf("Failed to set session model: %v", err)
		// Check if it's a timeout error
		if strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "timeout") {
			return c.Send(fmt.Sprintf("Model setting timeout: Model initialization may take longer. Please try again later or use the default model."))
		}
		return c.Send(fmt.Sprintf("Failed to set model: %v", err))
	}

	log.Infof("Successfully set model for user %d session %s to %s/%s", userID, sessionID, selection.ProviderID, selection.ModelID)
	return c.Send(fmt.Sprintf("✅ Current session model set to %s (%s/%s)\n\nThis model will be used as your default for new sessions.", selection.ModelName, selection.ProviderID, selection.ModelID))
}

// handleText handles plain text messages (non-commands) with real-time streaming
func (b *Bot) handleText(c telebot.Context) error {
	userID := c.Sender().ID
	text := c.Text()

	// Ignore empty messages
	if strings.TrimSpace(text) == "" {
		return nil
	}

	// Get or create session for user
	sessionID, err := b.sessionManager.GetOrCreateSession(b.ctx, userID)
	if err != nil {
		log.Errorf("Failed to get/create session: %v", err)
		return c.Send(fmt.Sprintf("Session error: %v", err))
	}

	// Get session metadata to check model configuration
	var messageModel *opencode.MessageModel
	meta, exists := b.sessionManager.GetSessionMeta(sessionID)
	if exists && meta.ProviderID != "" && meta.ModelID != "" {
		messageModel = &opencode.MessageModel{
			ProviderID: meta.ProviderID,
			ModelID:    meta.ModelID,
		}
		log.Debugf("Using session model %s/%s for message", meta.ProviderID, meta.ModelID)
	} else {
		// No model configured for this session
		log.Warnf("No model configured for session %s", sessionID)
		return c.Send("⚠️ No AI model configured for this session.\n\nPlease use `/models` to view available models, then use `/setmodel <number>` to set a model for this session.")
	}

	// Cancel any existing streaming for this session
	b.streamingStateMu.Lock()
	if existingState, ok := b.streamingStates[sessionID]; ok && existingState.isStreaming {
		existingState.cancel()
		existingState.isStreaming = false
		log.Infof("Cancelled existing streaming for session %s before starting new request", sessionID)
	}
	b.streamingStateMu.Unlock()

	// Send initial "processing" message
	processingMsg, err := b.sendRenderedTelegramMessage(c, "🤖 Processing...", true)
	if err != nil {
		return err
	}

	// Prepare context for event-driven streaming request lifecycle.
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()

	requestMessageID := opencode.GenerateMessageID()

	// Track streaming state
	streamingState := &streamingState{
		ctx:                 ctx,
		cancel:              cancel,
		telegramMsg:         processingMsg,
		telegramMessages:    []*telebot.Message{processingMsg},
		lastRendered:        []string{"🤖 Processing..."},
		telegramCtx:         c,
		content:             &strings.Builder{},
		lastUpdate:          time.Now(),
		updateMutex:         &sync.Mutex{},
		renderedMessageIDs:  make(map[string]bool),
		cachedMessageChunks: make(map[string][]string),
		allDisplayChunks:    nil,
		sessionInfoAdded:    false,
		sessionID:           sessionID,
		requestMessageID:    requestMessageID,
		requestStartedAt:    time.Now().UnixMilli(),
		initialMessageIDs:   make(map[string]bool),
		eventMessages:       make(map[string]*eventMessageState),
		displaySet:          make(map[string]bool),
		pendingSet:          make(map[string]bool),
		isStreaming:         true,
	}

	// Capture existing message IDs before sending the new request.
	// Event-driven rendering will ignore these historical IDs.
	if existingMessages, getErr := b.opencodeClient.GetMessages(ctx, sessionID); getErr == nil {
		for _, msg := range existingMessages {
			if msg.ID == "" {
				continue
			}
			streamingState.initialMessageIDs[msg.ID] = true
		}
	} else {
		log.Warnf("Failed to preload existing messages for event stream filtering: %v", getErr)
	}

	// Store streaming state for potential abort
	b.streamingStateMu.Lock()
	b.streamingStates[sessionID] = streamingState
	b.streamingStateMu.Unlock()

	// Clean up streaming state when done
	defer func() {
		b.streamingStateMu.Lock()
		delete(b.streamingStates, sessionID)
		b.streamingStateMu.Unlock()
		streamingState.isStreaming = false
	}()

	// Start event stream updates as the single real-time source.
	eventDone := make(chan struct{})
	go func() {
		defer close(eventDone)
		b.consumeSessionEvents(ctx, streamingState, sessionID)
	}()

	// Check if OpenCode server is reachable before sending the request.
	healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second)
	defer healthCancel()

	if healthErr := b.opencodeClient.HealthCheck(healthCtx); healthErr != nil {
		log.Warnf("OpenCode health check failed before sending message: %v", healthErr)
	} else {
		log.Debugf("OpenCode health check passed before sending message")
	}

	sendReq := &opencode.SendMessageRequest{
		MessageID: requestMessageID,
		Parts: []opencode.MessagePart{
			{
				Type: "text",
				Text: text,
			},
		},
	}
	if messageModel != nil {
		sendReq.Model = messageModel
	}

	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- b.opencodeClient.PostMessage(ctx, sessionID, sendReq)
	}()

	sendErr := <-sendErrCh
	streamingState.isComplete = true

	// Always reconcile against final snapshots to close any missed event gaps.
	b.reconcileEventStateWithLatestMessages(streamingState)

	settleTimeout := 2 * time.Duration(b.config.OpenCode.Timeout) * time.Second
	if settleTimeout < 2*time.Minute {
		settleTimeout = 2 * time.Minute
	}
	if settleTimeout > 30*time.Minute {
		settleTimeout = 30 * time.Minute
	}
	settleDeadline := time.Now().Add(settleTimeout)
	lastReconcileAt := time.Now()
	noOutputSince := time.Now()
	for {
		if time.Since(lastReconcileAt) >= time.Second {
			b.reconcileEventStateWithLatestMessages(streamingState)
			lastReconcileAt = time.Now()
		}

		streamingState.updateMutex.Lock()
		for b.tryPromoteNextActiveMessage(streamingState) {
		}
		b.flushEventDisplaysLocked(streamingState, true)
		settled := b.eventPipelineSettledLocked(streamingState, true)
		hasOutput := len(streamingState.displayOrder) > 0
		hasEvents := streamingState.hasEventUpdates
		if hasOutput {
			noOutputSince = time.Now()
		}
		if !hasOutput && !hasEvents && time.Since(noOutputSince) >= 3*time.Second {
			// Some providers return quickly with no assistant output.
			settled = true
		}
		streamingState.updateMutex.Unlock()

		if settled || time.Now().After(settleDeadline) {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}

	cancel()
	select {
	case <-eventDone:
	case <-time.After(2 * time.Second):
		log.Warnf("Timed out waiting for event stream to stop for session %s", sessionID)
	}

	streamingState.updateMutex.Lock()
	for b.tryPromoteNextActiveMessage(streamingState) {
	}
	displays := b.buildEventDrivenDisplaysLocked(streamingState)
	if len(displays) > 0 {
		b.updateStreamingTelegramMessages(streamingState, displays)
		streamingState.updateMutex.Unlock()
		if sendErr != nil {
			log.Warnf("SendMessage returned error after event rendering completed: %v", sendErr)
		}
		return nil
	}
	streamingState.updateMutex.Unlock()

	if sendErr != nil {
		errorMsg := fmt.Sprintf("Processing error: %v", sendErr)
		b.updateTelegramMessage(c, processingMsg, errorMsg, false)
		return nil
	}

	b.updateTelegramMessage(c, processingMsg, "🤖 Response completed with no content.", false)
	return nil
}

func (b *Bot) waitForLatestAssistantContent(sessionID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		content, err := b.getLatestAssistantContent(sessionID)
		if err != nil {
			lastErr = err
		} else if content != "" {
			return content, nil
		}

		time.Sleep(500 * time.Millisecond)
	}

	// Final immediate check at timeout boundary.
	content, err := b.getLatestAssistantContent(sessionID)
	if err != nil {
		if lastErr != nil {
			return "", lastErr
		}
		return "", err
	}
	return content, nil
}

func (b *Bot) getLatestAssistantContent(sessionID string) (string, error) {
	messages, err := b.opencodeClient.GetMessages(b.ctx, sessionID)
	if err != nil {
		return "", err
	}

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "assistant" {
			continue
		}

		content := strings.TrimSpace(msg.Content)
		if content != "" {
			return content, nil
		}

		if len(msg.Parts) == 0 {
			continue
		}
		parts := strings.TrimSpace(formatMessageParts(msg.Parts))
		if parts != "" && parts != "No detailed content" {
			return parts, nil
		}
	}

	return "", nil
}

// The following handlers are stubs for future implementation

func (b *Bot) handleFiles(c telebot.Context) error {
	args := c.Args()
	path := "."
	if len(args) > 0 {
		path = strings.Join(args, " ")
	}

	files, err := b.opencodeClient.ListFiles(b.ctx, path)
	if err != nil {
		log.Errorf("Failed to list files: %v", err)
		return c.Send(fmt.Sprintf("Failed to list files: %v", err))
	}

	if len(files) == 0 {
		return c.Send(fmt.Sprintf("Directory '%s' is empty or does not exist.", path))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📁 File List: %s\n\n", path)

	// Separate directories and files
	var dirs []opencode.FileInfo
	var fileList []opencode.FileInfo

	for _, file := range files {
		if file.Type == "directory" {
			dirs = append(dirs, file)
		} else {
			fileList = append(fileList, file)
		}
	}

	// Show directories first
	if len(dirs) > 0 {
		sb.WriteString("📂 Directories:\n")
		for _, dir := range dirs {
			ignored := ""
			if dir.Ignored {
				ignored = " [Ignored]"
			}
			fmt.Fprintf(&sb, "  • %s%s\n", dir.Name, ignored)
		}
		sb.WriteString("\n")
	}

	// Then files
	if len(fileList) > 0 {
		sb.WriteString("📄 Files:\n")
		for _, file := range fileList {
			ignored := ""
			if file.Ignored {
				ignored = " [Ignored]"
			}
			fmt.Fprintf(&sb, "  • %s%s\n", file.Name, ignored)
		}
		sb.WriteString("\n")
	}

	fmt.Fprintf(&sb, "Total: %d items (%d directories, %d files)", len(files), len(dirs), len(fileList))

	result := sb.String()
	if len(result) > 4000 {
		result = result[:4000] + "\n...(content too long, truncated)"
	}

	return c.Send(result)
}

func (b *Bot) handleSearch(c telebot.Context) error {
	args := c.Args()
	if len(args) == 0 {
		return c.Send("Please specify search content.\nUsage: /search <search pattern>")
	}

	query := strings.Join(args, " ")

	// Try to use OpenCode search API
	results, err := b.opencodeClient.SearchFiles(b.ctx, query)
	if err != nil {
		// API not available, provide helpful message
		log.Debugf("Search API not available: %v", err)
		return c.Send(fmt.Sprintf("🔍 Search functionality is currently unavailable.\n\nReason: %v\n\nYou can directly send a message to the assistant to request a search, for example:\n\"Search for code containing '%s'\"", err, query))
	}

	if len(results) == 0 {
		return c.Send(fmt.Sprintf("No code containing '%s' was found.", query))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🔍 Search Results: '%s'\n\n", query)

	// Limit results to prevent message overflow
	maxResults := 10
	if len(results) > maxResults {
		fmt.Fprintf(&sb, "Found %d results, showing first %d:\n\n", len(results), maxResults)
		results = results[:maxResults]
	}

	for i, result := range results {
		fmt.Fprintf(&sb, "%d. %s:%d\n", i+1, result.Path, result.Line)
		fmt.Fprintf(&sb, "   %s\n\n", strings.TrimSpace(result.Content))
	}

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...(content too long, truncated)"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleFindFile(c telebot.Context) error {
	args := c.Args()
	if len(args) == 0 {
		return c.Send("Please specify file pattern.\nUsage: /findfile <file pattern>")
	}

	pattern := strings.Join(args, " ")

	// Try to use OpenCode find file API
	files, err := b.opencodeClient.FindFile(b.ctx, pattern)
	if err != nil {
		// API not available, provide helpful message
		log.Debugf("Find file API not available: %v", err)
		return c.Send(fmt.Sprintf("🔍 File search functionality is currently unavailable.\n\nReason: %v\n\nYou can use the /files command to browse directories, or directly send a message to the assistant to request file search.", err))
	}

	if len(files) == 0 {
		return c.Send(fmt.Sprintf("No files matching '%s' were found.", pattern))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🔍 File Search Results: '%s'\n\n", pattern)

	// Separate directories and files
	var dirs []opencode.FileInfo
	var fileList []opencode.FileInfo

	for _, file := range files {
		if file.Type == "directory" {
			dirs = append(dirs, file)
		} else {
			fileList = append(fileList, file)
		}
	}

	// Limit results
	maxResults := 15
	totalResults := len(files)
	if totalResults > maxResults {
		fmt.Fprintf(&sb, "Found %d results, showing first %d:\n\n", totalResults, maxResults)
		if len(dirs) > maxResults/2 {
			dirs = dirs[:maxResults/2]
		}
		if len(fileList) > maxResults/2 {
			fileList = fileList[:maxResults/2]
		}
	}

	if len(dirs) > 0 {
		sb.WriteString("📂 Directories:\n")
		for _, dir := range dirs {
			ignored := ""
			if dir.Ignored {
				ignored = " [Ignored]"
			}
			fmt.Fprintf(&sb, "  • %s%s\n", dir.Path, ignored)
		}
		sb.WriteString("\n")
	}

	if len(fileList) > 0 {
		sb.WriteString("📄 Files:\n")
		for _, file := range fileList {
			ignored := ""
			if file.Ignored {
				ignored = " [Ignored]"
			}
			fmt.Fprintf(&sb, "  • %s%s\n", file.Path, ignored)
		}
		sb.WriteString("\n")
	}

	fmt.Fprintf(&sb, "Total: %d items", totalResults)

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...(content too long, truncated)"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleSymbol(c telebot.Context) error {
	args := c.Args()
	if len(args) == 0 {
		return c.Send("Please specify symbol name.\nUsage: /symbol <symbol name>")
	}

	symbol := strings.Join(args, " ")

	// Try to use OpenCode symbol search API
	results, err := b.opencodeClient.SearchSymbol(b.ctx, symbol)
	if err != nil {
		// API not available, provide helpful message
		log.Debugf("Symbol search API not available: %v", err)
		return c.Send(fmt.Sprintf("🔍 Symbol search functionality is currently unavailable.\n\nReason: %v\n\nYou can directly send a message to the assistant to request symbol search, for example:\n\"Find function %s\"", err, symbol))
	}

	if len(results) == 0 {
		return c.Send(fmt.Sprintf("Symbol '%s' not found.", symbol))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🔍 Symbol Search Results: '%s'\n\n", symbol)

	// Limit results
	maxResults := 10
	if len(results) > maxResults {
		fmt.Fprintf(&sb, "Found %d results, showing first %d:\n\n", len(results), maxResults)
		results = results[:maxResults]
	}

	for i, result := range results {
		fmt.Fprintf(&sb, "%d. %s (%s)\n", i+1, result.Name, result.Kind)
		fmt.Fprintf(&sb, "   Location: %s:%d\n", result.Path, result.Line)
		if result.Signature != "" {
			fmt.Fprintf(&sb, "   Signature: %s\n", result.Signature)
		}
		sb.WriteString("\n")
	}

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...(content too long, truncated)"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleAgent(c telebot.Context) error {
	// Try to get agents list
	agents, err := b.opencodeClient.ListAgents(b.ctx)
	if err != nil {
		// API not available, provide helpful message
		log.Debugf("Agents API not available: %v", err)
		return c.Send(fmt.Sprintf("🤖 Agent list functionality is currently unavailable.\n\nReason: %v\n\nYou can use /models and /providers commands to view available AI models and providers.", err))
	}

	if len(agents) == 0 {
		return c.Send("No AI agents are currently available.")
	}

	var sb strings.Builder
	sb.WriteString("🤖 Available AI Agents:\n\n")

	for i, agent := range agents {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, agent.Name)
		if agent.Description != "" {
			fmt.Fprintf(&sb, "   Description: %s\n", agent.Description)
		}
		fmt.Fprintf(&sb, "   ID: %s\n\n", agent.ID)
	}

	fmt.Fprintf(&sb, "Total: %d agents", len(agents))

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...(content too long, truncated)"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleCommand(c telebot.Context) error {
	return c.Send("Command list functionality is not yet implemented.")
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

// clearModelMapping clears the model mapping for a user
func (b *Bot) clearModelMapping(userID int64) {
	b.modelMappingMu.Lock()
	defer b.modelMappingMu.Unlock()
	delete(b.modelMapping, userID)
}

// periodicMessageUpdates periodically polls OpenCode and updates streaming messages.
// It serves as a fallback path when SSE text chunks are sparse or absent.
func (b *Bot) periodicMessageUpdates(ctx context.Context, state *streamingState, sessionID string, stopCh <-chan struct{}) {
	if state == nil {
		return
	}

	// Ticker for periodic updates (every 2 seconds)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Count updates for logging
	updateCount := 0

	for {
		select {
		case <-ctx.Done():
			log.Debugf("Periodic updates stopped for session %s: context done", sessionID)
			return
		case <-stopCh:
			log.Debugf("Periodic updates stopped for session %s: stop signal", sessionID)
			return
		case <-ticker.C:
			state.updateMutex.Lock()
			hasRecentEvents := state.hasEventUpdates && time.Since(state.lastEventAt) < 5*time.Second
			state.updateMutex.Unlock()
			if hasRecentEvents {
				continue
			}

			updateCount++
			log.Debugf("Periodic update #%d for session %s", updateCount, sessionID)

			messages, err := b.opencodeClient.GetMessages(ctx, sessionID)
			if err != nil {
				log.Errorf("Failed to get messages for periodic update: %v", err)
				continue
			}
			if len(messages) == 0 {
				continue
			}

			state.updateMutex.Lock()
			if state.hasEventUpdates {
				b.reconcileEventStateWithMessagesLocked(state, messages)
				for b.tryPromoteNextActiveMessage(state) {
				}
				displays := b.buildEventDrivenDisplaysLocked(state)
				if len(displays) == 0 {
					displays = []string{"🤖 Processing...\n\nModel is thinking, please wait..."}
				}
				b.updateStreamingTelegramMessages(state, displays)
				state.updateMutex.Unlock()
				continue
			}

			// Get session metadata for formatting
			var sessionMeta *session.SessionMeta
			if b.sessionManager != nil {
				if meta, exists := b.sessionManager.GetSessionMeta(sessionID); exists {
					sessionMeta = meta
				}
			}

			// Format all messages for streaming display (with cache)
			displays := b.buildDisplayChunksFromMessagesWithCache(messages, sessionMeta, state)

			// If no displays (should not happen), show a placeholder
			if len(displays) == 0 {
				displays = []string{"🤖 Processing...\n\nModel is thinking, please wait..."}
			}

			b.updateStreamingTelegramMessages(state, displays)
			state.updateMutex.Unlock()
		}
	}
}

func (b *Bot) consumeSessionEvents(ctx context.Context, state *streamingState, sessionID string) {
	if state == nil || b.opencodeClient == nil {
		return
	}

	backoff := 200 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}

		err := b.opencodeClient.StreamSessionEvents(ctx, func(event opencode.SessionEvent) error {
			state.updateMutex.Lock()
			changed, forceFlush := b.applySessionEventLocked(state, sessionID, event)
			if !changed {
				state.updateMutex.Unlock()
				return nil
			}

			state.hasEventUpdates = true
			state.lastEventAt = time.Now()
			for b.tryPromoteNextActiveMessage(state) {
			}
			b.flushEventDisplaysLocked(state, forceFlush)
			state.updateMutex.Unlock()
			return nil
		})

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Warnf("Event stream disconnected for session %s: %v", sessionID, err)
		} else {
			log.Warnf("Event stream closed unexpectedly for session %s", sessionID)
		}

		b.reconcileEventStateWithLatestMessages(state)

		wait := backoff
		if wait > 3*time.Second {
			wait = 3 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff < 3*time.Second {
			backoff *= 2
		}
	}
}

func (b *Bot) applySessionEventLocked(state *streamingState, sessionID string, event opencode.SessionEvent) (changed bool, forceFlush bool) {
	switch event.Type {
	case "message.updated":
		return b.applyMessageUpdatedEventLocked(state, sessionID, event.Properties)
	case "message.part.updated":
		return b.applyMessagePartUpdatedEventLocked(state, sessionID, event.Properties)
	default:
		return false, false
	}
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
	if info.ID == state.requestMessageID && strings.EqualFold(strings.TrimSpace(info.Role), "user") {
		return false, false
	}
	if !b.shouldTrackEventMessageLocked(state, info.ID, info.Time.Created) {
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
		return false, false
	}
	if !b.shouldTrackEventMessageLocked(state, part.MessageID, 0) {
		return false, false
	}

	msgState := b.getOrCreateEventMessageStateLocked(state, part.MessageID)
	changed := upsertEventPartLocked(msgState, part)
	return changed, false
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
	if created > 0 && state.requestStartedAt > 0 && created+500 < state.requestStartedAt {
		return false
	}
	return true
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

func (b *Bot) flushEventDisplaysLocked(state *streamingState, force bool) bool {
	if state == nil {
		return false
	}
	now := time.Now()
	if !force && now.Sub(state.lastUpdate) < 300*time.Millisecond {
		return false
	}

	displays := b.buildEventDrivenDisplaysLocked(state)
	if len(displays) == 0 {
		return false
	}
	state.lastUpdate = now
	b.updateStreamingTelegramMessages(state, displays)
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
		return len(state.displayOrder) > 0 || state.hasEventUpdates
	}
	active := state.eventMessages[state.activeMessageID]
	return isEventMessageCompleted(active)
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

	ordered := make([]opencode.MessagePartResponse, 0, len(msg.Parts))
	seen := make(map[string]bool, len(msg.Parts))
	for _, partID := range msg.PartOrder {
		part, exists := msg.Parts[partID]
		if !exists {
			continue
		}
		ordered = append(ordered, part)
		seen[partID] = true
	}

	if len(ordered) == len(msg.Parts) {
		return ordered
	}

	remainingIDs := make([]string, 0, len(msg.Parts)-len(ordered))
	for partID := range msg.Parts {
		if seen[partID] {
			continue
		}
		remainingIDs = append(remainingIDs, partID)
	}
	sort.Strings(remainingIDs)
	for _, partID := range remainingIDs {
		ordered = append(ordered, msg.Parts[partID])
	}
	return ordered
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

func (b *Bot) reconcileEventStateWithLatestMessages(state *streamingState) {
	if state == nil || state.sessionID == "" || b.opencodeClient == nil {
		return
	}

	ctx, cancel := context.WithTimeout(state.ctx, 4*time.Second)
	defer cancel()

	messages, err := b.opencodeClient.GetMessages(ctx, state.sessionID)
	if err != nil {
		log.Warnf("Failed to reconcile event state from message snapshots: %v", err)
		return
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].CreatedAt.Before(messages[j].CreatedAt)
	})

	state.updateMutex.Lock()
	defer state.updateMutex.Unlock()
	b.reconcileEventStateWithMessagesLocked(state, messages)
}

func (b *Bot) reconcileEventStateWithMessagesLocked(state *streamingState, messages []opencode.Message) {
	if state == nil {
		return
	}

	for _, msg := range messages {
		if msg.ID == "" {
			continue
		}
		created := int64(0)
		if !msg.CreatedAt.IsZero() {
			created = msg.CreatedAt.UnixMilli()
		}
		if !b.shouldTrackEventMessageLocked(state, msg.ID, created) {
			continue
		}

		msgState := b.getOrCreateEventMessageStateLocked(state, msg.ID)
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

		for _, rawPart := range msg.Parts {
			part, ok := rawPart.(opencode.MessagePartResponse)
			if !ok {
				continue
			}
			upsertEventPartLocked(msgState, part)
		}

		if role := strings.ToLower(strings.TrimSpace(msgState.Info.Role)); role != "user" && role != "" {
			b.enqueueEventMessageLocked(state, msg.ID)
		}
	}

	for b.tryPromoteNextActiveMessage(state) {
	}
}

func upsertEventPartLocked(msgState *eventMessageState, part opencode.MessagePartResponse) bool {
	if msgState == nil {
		return false
	}

	partID := strings.TrimSpace(part.ID)
	if partID == "" {
		partID = fmt.Sprintf("%s:%d", part.Type, len(msgState.PartOrder)+1)
		part.ID = partID
	}

	existing, exists := msgState.Parts[partID]
	if exists && sameEventPart(existing, part) {
		return false
	}

	if !exists {
		msgState.PartOrder = append(msgState.PartOrder, partID)
	}
	msgState.Parts[partID] = part
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
	for _, chunk := range chunks {
		// Only output content, no progress indicators or pagination headers
		displays = append(displays, chunk)
	}
	return b.ensureTelegramRenderSafeDisplays(displays, true)
}

func (b *Bot) paginateDisplayText(content string, streaming bool) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return []string{""}
	}

	chunks := b.splitLongContentPreserveCodeBlocks(content)
	if len(chunks) <= 1 {
		return b.ensureTelegramRenderSafeDisplays([]string{content}, streaming)
	}

	displays := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		// Only add content, no pagination headers
		displays = append(displays, chunk)
	}
	return b.ensureTelegramRenderSafeDisplays(displays, streaming)
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
				bestDistance = dist
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

// handleFinalResponse handles the final response after streaming is complete.
func (b *Bot) handleFinalResponse(c telebot.Context, state *streamingState, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		content = "🤖 Response completed."
	}

	if state == nil || len(state.telegramMessages) == 0 {
		msg, err := b.sendRenderedTelegramMessage(c, "🤖 Processing...", false)
		if err != nil {
			log.Errorf("Failed to create fallback message for final response: %v", err)
			return
		}
		state = &streamingState{
			telegramMessages:    []*telebot.Message{msg},
			renderedMessageIDs:  make(map[string]bool),
			cachedMessageChunks: make(map[string][]string),
			allDisplayChunks:    nil,
			sessionInfoAdded:    false,
			initialMessageIDs:   make(map[string]bool),
			eventMessages:       make(map[string]*eventMessageState),
			displaySet:          make(map[string]bool),
			pendingSet:          make(map[string]bool),
		}
	}

	chunks := b.ensureTelegramRenderSafeDisplays([]string{content}, false)
	if len(chunks) == 0 {
		b.updateTelegramMessage(c, state.telegramMessages[0], "✅ Response completed.", false)
		return
	}

	// Ensure enough Telegram messages exist for all parts.
	for len(state.telegramMessages) < len(chunks) {
		newMsg, err := b.sendRenderedTelegramMessage(c, "🤖 Processing...", false)
		if err != nil {
			log.Errorf("Failed to create final response part message %d: %v", len(state.telegramMessages)+1, err)
			return
		}
		state.telegramMessages = append(state.telegramMessages, newMsg)
	}

	for i, chunk := range chunks {
		// Only output content, no pagination headers or completion markers
		b.updateTelegramMessage(c, state.telegramMessages[i], chunk, false)
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
	truncated := false

	for i, line := range lines {
		// If we've already truncated, stop processing
		if truncated {
			break
		}
		// Check if this line starts or ends a code block
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inCodeBlock {
				// Starting a code block
				inCodeBlock = true
				codeBlockMarker = trimmed
			} else {
				// Check if this ends the current code block
				if strings.HasPrefix(trimmed, codeBlockMarker) ||
					(len(trimmed) >= 3 && trimmed[:3] == "```") {
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
				currentChunk.WriteString("```\n")
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
				currentChunk.WriteString(codeBlockMarker + "\n")
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

// formatLatestMessage formats the latest message from a session for display
func (b *Bot) formatLatestMessage(sessionID string, userID int64) (string, error) {
	// Get recent messages
	messages, err := b.opencodeClient.GetMessages(b.ctx, sessionID)
	if err != nil {
		log.Errorf("Failed to get messages for latest update: %v", err)
		return "", err
	}

	if len(messages) == 0 {
		return "No messages in session.", nil
	}

	// Get the latest message
	latestMsg := messages[len(messages)-1]

	// Check if this is an assistant message with detailed parts
	hasDetailedParts := len(latestMsg.Parts) > 0 && formatMessageParts(latestMsg.Parts) != "No detailed content"

	// If it's not an assistant message or doesn't have detailed parts, no need for separate update
	if latestMsg.Role != "assistant" || !hasDetailedParts {
		return "", nil
	}

	// Format the message similar to /status command
	role := "🤖 Assistant"
	timeStr := latestMsg.CreatedAt.Format("15:04")

	var sb strings.Builder
	sb.WriteString("📋 Latest Message Details\n")
	sb.WriteString("────────────────\n")
	fmt.Fprintf(&sb, "[Message 0] %s [%s]\n", role, timeStr)
	sb.WriteString("────────────────\n")

	// Show detailed parts
	partsStr := formatMessageParts(latestMsg.Parts)
	fmt.Fprintf(&sb, "%s\n", partsStr)

	// Truncate if too long for Telegram
	result := sb.String()
	const maxTelegramLength = 3500
	if len(result) > maxTelegramLength {
		result = result[:maxTelegramLength] + "\n... (content too long, use /status or /current for full details)"
	}

	return result, nil
}

// Close closes the bot and releases resources
func (b *Bot) Close() error {
	if b.cancel != nil {
		b.cancel()
	}

	if b.sessionManager != nil {
		return b.sessionManager.Close()
	}
	return nil
}
