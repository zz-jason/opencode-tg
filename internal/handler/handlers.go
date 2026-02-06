package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/telebot.v4"
	"tg-bot/internal/config"
	"tg-bot/internal/opencode"
	"tg-bot/internal/session"
	"tg-bot/internal/storage"
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

	// Session mapping for each user (userID -> map[int]sessionID)
	sessionMappingMu sync.RWMutex
	sessionMapping   map[int64]map[int]string

	// Streaming state management
	streamingStateMu sync.RWMutex
	streamingStates  map[string]*streamingState
}

// streamingState tracks the state of an active streaming response
type streamingState struct {
	ctx         context.Context
	cancel      context.CancelFunc
	stopUpdates chan struct{}

	telegramMsg *telebot.Message
	telegramCtx telebot.Context

	content     *strings.Builder
	lastUpdate  time.Time
	updateMutex *sync.Mutex

	isStreaming bool
	isComplete  bool
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
		config:          cfg,
		opencodeClient:  client,
		sessionManager:  sessionManager,
		ctx:             ctx,
		cancel:          cancel,
		modelMapping:    make(map[int64]map[int]modelSelection),
		sessionMapping:  make(map[int64]map[int]string),
		streamingStates: make(map[string]*streamingState),
	}

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
	b.tgBot.Handle("/newmodel", b.handleNewModel)
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
• /newmodel <name> <number> - Create new session with specified model

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
			sb.WriteString(fmt.Sprintf("[✅ CURRENT] %d. %s\n", i+1, sess.Name))
		} else {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, sess.Name))
		}

		// Add separator line (fixed length)
		sb.WriteString("════════════════\n")

		// Add session details with bullet points
		sb.WriteString(fmt.Sprintf("• Created: %s\n", sess.CreatedAt.Format("2006-01-02 15:04")))
		sb.WriteString(fmt.Sprintf("• Last used: %s\n", sess.LastUsedAt.Format("2006-01-02 15:04")))
		sb.WriteString(fmt.Sprintf("• Messages: %d\n", sess.MessageCount))

		// Add model information if available
		if sess.ProviderID != "" && sess.ModelID != "" {
			sb.WriteString(fmt.Sprintf("• Model: %s/%s\n", sess.ProviderID, sess.ModelID))
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

	return c.Send(fmt.Sprintf("✅ Created new session: %s\n\nThis session has been set as your current session.", name))
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
		return c.Send("You don't have a current session. Use /new to create a new session.")
	}

	meta, exists := b.sessionManager.GetSessionMeta(sessionID)
	if !exists {
		return c.Send("Session information lost. Use /new to create a new session.")
	}

	// Get recent messages
	messages, err := b.opencodeClient.GetMessages(b.ctx, sessionID)
	if err != nil {
		log.Errorf("Failed to get messages: %v", err)
		return c.Send(fmt.Sprintf("Failed to get messages: %v", err))
	}

	// Get session details from OpenCode
	session, err := b.opencodeClient.GetSession(b.ctx, sessionID)
	if err != nil {
		log.Errorf("Failed to get session details: %v", err)
		// Continue with basic info
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
	sb.WriteString("📁 Current Session\n")
	sb.WriteString("════════════════\n")

	// Show session info in bullet points (same format as /status)
	sb.WriteString(fmt.Sprintf("• Name: %s\n", meta.Name))
	sb.WriteString(fmt.Sprintf("• Created: %s\n", meta.CreatedAt.Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("• Last used: %s\n", meta.LastUsedAt.Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("• Messages: %d\n", meta.MessageCount))
	if meta.ProviderID != "" && meta.ModelID != "" {
		sb.WriteString(fmt.Sprintf("• Current model: %s/%s\n", meta.ProviderID, meta.ModelID))
	} else {
		sb.WriteString("• Current model: Default\n")
	}
	sb.WriteString(fmt.Sprintf("• Status: %s\n", statusStr))

	if session != nil {
		createdAt := time.UnixMilli(session.Time.Created)
		sb.WriteString(fmt.Sprintf("• OpenCode created: %s\n", createdAt.Format("2006-01-02 15:04")))
		updatedAt := time.UnixMilli(session.Time.Updated)
		sb.WriteString(fmt.Sprintf("• OpenCode updated: %s\n", updatedAt.Format("2006-01-02 15:04")))
	}

	sb.WriteString("\n")

	// Show latest message if available
	if len(messages) > 0 {
		msg := messages[len(messages)-1]
		role := "👤 You"
		if msg.Role == "assistant" {
			role = "🤖 Assistant"
		} else if msg.Role == "system" {
			role = "⚙️ System"
		}
		timeStr := msg.CreatedAt.Format("15:04")

		sb.WriteString(fmt.Sprintf("[Message 0] %s [%s]\n", role, timeStr))
		sb.WriteString("════════════════\n")

		// Show message content
		if len(msg.Parts) > 0 {
			// Always show detailed parts if available, especially for tool calls
			partsStr := formatMessageParts(msg.Parts)
			if partsStr != "No detailed content" {
				sb.WriteString(fmt.Sprintf("%s\n", partsStr))
			} else if msg.Content != "" {
				// Fallback to content if parts don't provide details
				content := msg.Content
				if len(content) > 400 {
					content = content[:400] + "..."
				}
				sb.WriteString(fmt.Sprintf("%s\n", content))
			} else {
				sb.WriteString("(No content)\n")
			}
		} else if msg.Content != "" {
			// No parts, just show content
			content := msg.Content
			if len(content) > 400 {
				content = content[:400] + "..."
			}
			sb.WriteString(fmt.Sprintf("%s\n", content))
		} else {
			sb.WriteString("(No content)\n")
		}
	} else {
		sb.WriteString("No messages yet.\n")
	}

	// Truncate if too long
	result := sb.String()
	if len(result) > 4000 {
		result = result[:4000] + "\n... (content too long, truncated)"
	}

	return c.Send(result)
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
		close(state.stopUpdates)
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
	if len(parts) == 0 {
		return "No detailed content"
	}

	var sb strings.Builder
	hasTextContent := false
	var textContent strings.Builder

	for _, part := range parts {
		// Try to cast to opencode.MessagePartResponse
		if partResp, ok := part.(opencode.MessagePartResponse); ok {
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
				// Show reasoning text if available
				if partResp.Text != "" {
					reasoningText := partResp.Text
					if len(reasoningText) > 2000 {
						reasoningText = reasoningText[:2000] + "..."
					}
					sb.WriteString(fmt.Sprintf("• Thinking: %s\n", reasoningText))
				} else {
					sb.WriteString("• Thinking: Processed\n")
				}
			case "step-start":
				// Skip "Task start" message as it's redundant
				// sb.WriteString("🚀 Task start\n")
			case "step-finish":
				// Skip step-finish in status display as it's redundant
				// finishMsg := fmt.Sprintf("✅ Task completed")
				// if partResp.Reason != "" {
				// 	finishMsg += fmt.Sprintf(" (Reason: %s)", partResp.Reason)
				// }
				// if partResp.Cost > 0 {
				// 	finishMsg += fmt.Sprintf(" [Cost: %.4f]", partResp.Cost)
				// }
				// sb.WriteString(finishMsg + "\n")
			case "tool":
				// Get tool name
				toolName := partResp.Tool

				// Try to parse snapshot as JSON for backward compatibility
				var snapshotData map[string]interface{}
				if toolName == "" && partResp.Snapshot != "" {
					if err := json.Unmarshal([]byte(partResp.Snapshot), &snapshotData); err == nil {
						// Extract tool name/type from various possible fields
						if name, ok := snapshotData["name"].(string); ok && name != "" {
							toolName = name
						} else if toolType, ok := snapshotData["type"].(string); ok && toolType != "" {
							toolName = toolType
						} else if tool, ok := snapshotData["tool"].(string); ok && tool != "" {
							toolName = tool
						} else if function, ok := snapshotData["function"].(string); ok && function != "" {
							toolName = function
						}
					}
				}

				// Default tool name if still empty
				if toolName == "" {
					toolName = "tool"
				}

				// Get state data
				var stateData map[string]interface{}
				if partResp.State != nil {
					if stateMap, ok := partResp.State.(map[string]interface{}); ok {
						stateData = stateMap
					}
				}

				// Use state data if available, otherwise fall back to snapshot data
				sourceData := stateData
				if sourceData == nil {
					sourceData = snapshotData
				}

				// Determine emoji based on status
				emoji := "🛠️"
				if sourceData != nil {
					if status, ok := sourceData["status"].(string); ok && status == "completed" {
						emoji = "✅"
					}
				}

				// Build description
				description := ""
				if sourceData != nil {
					// Try to get description from input.description
					if input, ok := sourceData["input"].(map[string]interface{}); ok {
						if desc, ok := input["description"].(string); ok && desc != "" {
							description = desc
						} else if cmd, ok := input["command"].(string); ok && cmd != "" {
							// Use command as description, truncated
							cmdDisplay := cmd
							if len(cmdDisplay) > 100 {
								cmdDisplay = cmdDisplay[:100] + "..."
							}
							cmdDisplay = strings.ReplaceAll(cmdDisplay, "\n", "\\n")
							description = cmdDisplay
						}
					} else if input, ok := sourceData["input"].(string); ok && input != "" {
						// Input as string
						inputDisplay := input
						if len(inputDisplay) > 100 {
							inputDisplay = inputDisplay[:100] + "..."
						}
						inputDisplay = strings.ReplaceAll(inputDisplay, "\n", "\\n")
						description = inputDisplay
					} else if content, ok := sourceData["content"].(string); ok && content != "" {
						contentDisplay := content
						if len(contentDisplay) > 100 {
							contentDisplay = contentDisplay[:100] + "..."
						}
						contentDisplay = strings.ReplaceAll(contentDisplay, "\n", "\\n")
						description = contentDisplay
					}
				}

				// If no description, use tool text or default
				if description == "" && partResp.Text != "" {
					toolText := partResp.Text
					if len(toolText) > 100 {
						toolText = toolText[:100] + "..."
					}
					toolText = strings.ReplaceAll(toolText, "\n", "\\n")
					description = toolText
				}
				if description == "" {
					description = "executed"
				}

				// Output formatted tool info
				sb.WriteString(fmt.Sprintf("• %s %s: %s\n", emoji, toolName, description))
			default:
				sb.WriteString(fmt.Sprintf("🔹 %s\n", partResp.Type))
			}
		} else if partMap, ok := part.(map[string]interface{}); ok {
			// Fallback to map representation
			if partType, ok := partMap["type"].(string); ok {
				switch partType {
				case "text":
					if text, ok := partMap["text"].(string); ok && text != "" {
						hasTextContent = true
						textContent.WriteString(text)
						if !strings.HasSuffix(text, "\n") {
							textContent.WriteString("\n")
						}
					}
				case "reasoning":
					if text, ok := partMap["text"].(string); ok && text != "" {
						reasoningText := text
						if len(reasoningText) > 300 {
							reasoningText = reasoningText[:300] + "..."
						}
						sb.WriteString(fmt.Sprintf("• Thinking: %s\n", reasoningText))
					} else {
						sb.WriteString("• Thinking: Processed\n")
					}
				case "tool":
					// Get tool name
					toolName := ""
					if tool, ok := partMap["tool"].(string); ok && tool != "" {
						toolName = tool
					}

					// Try to parse snapshot as JSON for backward compatibility
					var snapshotData map[string]interface{}
					if snapshot, ok := partMap["snapshot"].(string); ok && snapshot != "" {
						if err := json.Unmarshal([]byte(snapshot), &snapshotData); err == nil {
							// Extract tool name if not already found
							if toolName == "" {
								if name, ok := snapshotData["name"].(string); ok && name != "" {
									toolName = name
								} else if toolType, ok := snapshotData["type"].(string); ok && toolType != "" {
									toolName = toolType
								} else if tool, ok := snapshotData["tool"].(string); ok && tool != "" {
									toolName = tool
								}
							}
						}
					}

					// Default tool name if still empty
					if toolName == "" {
						toolName = "tool"
					}

					// Get state data
					var stateData map[string]interface{}
					if state, ok := partMap["state"]; ok {
						if stateMap, ok := state.(map[string]interface{}); ok {
							stateData = stateMap
						}
					}

					// Use state data if available, otherwise fall back to snapshot data
					sourceData := stateData
					if sourceData == nil {
						sourceData = snapshotData
					}

					// Determine emoji based on status
					emoji := "🛠️"
					if sourceData != nil {
						if status, ok := sourceData["status"].(string); ok && status == "completed" {
							emoji = "✅"
						}
					}

					// Build description
					description := ""
					if sourceData != nil {
						// Try to get description from input.description
						if input, ok := sourceData["input"].(map[string]interface{}); ok {
							if desc, ok := input["description"].(string); ok && desc != "" {
								description = desc
							} else if cmd, ok := input["command"].(string); ok && cmd != "" {
								// Use command as description, truncated
								cmdDisplay := cmd
								if len(cmdDisplay) > 100 {
									cmdDisplay = cmdDisplay[:100] + "..."
								}
								cmdDisplay = strings.ReplaceAll(cmdDisplay, "\n", "\\n")
								description = cmdDisplay
							}
						} else if input, ok := sourceData["input"].(string); ok && input != "" {
							// Input as string
							inputDisplay := input
							if len(inputDisplay) > 100 {
								inputDisplay = inputDisplay[:100] + "..."
							}
							inputDisplay = strings.ReplaceAll(inputDisplay, "\n", "\\n")
							description = inputDisplay
						} else if content, ok := sourceData["content"].(string); ok && content != "" {
							contentDisplay := content
							if len(contentDisplay) > 100 {
								contentDisplay = contentDisplay[:100] + "..."
							}
							contentDisplay = strings.ReplaceAll(contentDisplay, "\n", "\\n")
							description = contentDisplay
						}
					}

					// If no description, use tool text or default
					if description == "" {
						if text, ok := partMap["text"].(string); ok && text != "" {
							toolText := text
							if len(toolText) > 100 {
								toolText = toolText[:100] + "..."
							}
							toolText = strings.ReplaceAll(toolText, "\n", "\\n")
							description = toolText
						}
					}
					if description == "" {
						description = "executed"
					}

					// Output formatted tool info
					sb.WriteString(fmt.Sprintf("• %s %s: %s\n", emoji, toolName, description))
				default:
					sb.WriteString(fmt.Sprintf("🔹 %s\n", partType))
				}
			} else {
				sb.WriteString(fmt.Sprintf("🔹 Unknown type\n"))
			}
		} else {
			sb.WriteString(fmt.Sprintf("🔹 Unknown part\n"))
		}
	}

	// Add text content at the end if we have any
	if hasTextContent {
		text := strings.TrimSpace(textContent.String())
		if text != "" {
			// Truncate if too long, but be generous for important content
			if len(text) > 3000 {
				text = text[:3000] + "...\n(Reply content too long, truncated)"
			}
			sb.WriteString(fmt.Sprintf("\n• ✅ Reply content:\n%s\n", text))
		}
	}

	result := strings.TrimSpace(sb.String())
	if result == "" {
		return "No detailed content"
	}
	return result
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
	sb.WriteString("════════════════\n")

	// Show session info
	session, err := b.opencodeClient.GetSession(b.ctx, sessionID)
	if err == nil && session != nil {
		sb.WriteString(fmt.Sprintf("• Name: %s\n", session.Title))
		createdAt := time.UnixMilli(session.Time.Created)
		sb.WriteString(fmt.Sprintf("• Created: %s\n", createdAt.Format("2006-01-02 15:04")))
	}

	sb.WriteString(fmt.Sprintf("• Messages: %d\n", len(messages)))
	sb.WriteString(fmt.Sprintf("• Status: %s\n\n", statusStr))

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

		sb.WriteString(fmt.Sprintf("\n[Message %d] %s [%s]\n", relIndex, role, timeStr))
		sb.WriteString("════════════════\n")

		// Show message content
		if len(msg.Parts) > 0 {
			// Always show detailed parts if available, especially for tool calls
			partsStr := formatMessageParts(msg.Parts)
			if partsStr != "No detailed content" {
				sb.WriteString(fmt.Sprintf("%s\n", partsStr))
			} else if msg.Content != "" {
				// Fallback to content if parts don't provide details
				content := msg.Content
				if len(content) > 400 {
					content = content[:400] + "..."
				}
				sb.WriteString(fmt.Sprintf("%s\n", content))
			} else {
				sb.WriteString("(No content)\n")
			}
		} else if msg.Content != "" {
			// No parts, just show content
			content := msg.Content
			if len(content) > 400 {
				content = content[:400] + "..."
			}
			sb.WriteString(fmt.Sprintf("%s\n", content))
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
	providersResp, err := b.opencodeClient.GetProviders(b.ctx)
	if err != nil {
		log.Errorf("Failed to get providers: %v", err)
		return c.Send(fmt.Sprintf("Failed to get model list: %v", err))
	}

	var sb strings.Builder
	sb.WriteString("🤖 Available AI Models\n\n")

	// Create a set of connected provider IDs for faster lookup
	connectedSet := make(map[string]bool)
	for _, providerID := range providersResp.Connected {
		connectedSet[providerID] = true
	}

	// Track if we found any models
	foundAnyModels := false

	// Map to store model ID mapping (sequential integer -> model selection)
	modelCounter := 1
	modelMapping := make(map[int]modelSelection)

	// First, show models from connected providers
	for _, provider := range providersResp.All {
		if !connectedSet[provider.ID] {
			continue // Skip unconnected providers
		}

		if len(provider.Models) == 0 {
			continue
		}

		foundAnyModels = true
		sb.WriteString(fmt.Sprintf("🏷️ %s\n", provider.Name))

		for _, model := range provider.Models {
			// Store mapping
			modelMapping[modelCounter] = modelSelection{
				ProviderID: provider.ID,
				ModelID:    model.ID,
				ModelName:  model.Name,
			}

			sb.WriteString(fmt.Sprintf("  %d. %s\n", modelCounter, model.Name))
			modelCounter++
		}
		sb.WriteString("----\n")
	}

	// If no connected providers, show a message
	if !foundAnyModels {
		sb.WriteString("⚠️ No connected AI providers.\n")
		sb.WriteString("Please configure API keys for at least one AI provider first.\n\n")

		// Show all available providers for reference
		sb.WriteString("Configurable AI providers:\n")
		for _, provider := range providersResp.All {
			sb.WriteString(fmt.Sprintf("  • %s (%s)\n", provider.Name, provider.ID))
			if len(provider.Env) > 0 {
				sb.WriteString(fmt.Sprintf("    Environment variables required: %s\n", strings.Join(provider.Env, ", ")))
			}
		}
		sb.WriteString("\n")
	} else {
		// Remove the last "----" separator
		resultStr := sb.String()
		if strings.HasSuffix(resultStr, "----\n") {
			resultStr = strings.TrimSuffix(resultStr, "----\n")
			sb.Reset()
			sb.WriteString(resultStr)
		}

		// Add usage instructions
		sb.WriteString("\n📝 Usage Instructions:\n")
		sb.WriteString("• Use /setmodel <number> to set model for current session\n")
		sb.WriteString("• Use /newmodel <name> <number> to create new session with specified model\n")
	}

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
			sb.WriteString(fmt.Sprintf("✅ %s\n", provider.Name))
			sb.WriteString(fmt.Sprintf("  ID: %s\n", provider.ID))
			sb.WriteString(fmt.Sprintf("  Source: %s\n", provider.Source))
			if len(provider.Env) > 0 {
				sb.WriteString(fmt.Sprintf("  Environment Variables: %s\n", strings.Join(provider.Env, ", ")))
			}
			if len(provider.Models) > 0 {
				sb.WriteString(fmt.Sprintf("  Models: %d\n", len(provider.Models)))
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
			sb.WriteString(fmt.Sprintf("⚪ %s\n", provider.Name))
			sb.WriteString(fmt.Sprintf("  ID: %s\n", provider.ID))
			sb.WriteString(fmt.Sprintf("  Source: %s\n", provider.Source))
			if len(provider.Env) > 0 {
				sb.WriteString(fmt.Sprintf("  Required Environment Variables: %s\n", strings.Join(provider.Env, ", ")))
			}
			if len(provider.Models) > 0 {
				sb.WriteString(fmt.Sprintf("  Available Models: %d\n", len(provider.Models)))
			}
			sb.WriteString("\n")
		}
	}

	// Summary
	sb.WriteString("📊 Summary:\n")
	sb.WriteString(fmt.Sprintf("  • Connected: %d providers\n", len(providersResp.Connected)))
	sb.WriteString(fmt.Sprintf("  • Total: %d providers\n", len(providersResp.All)))
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
	return c.Send(fmt.Sprintf("✅ Current session model set to %s (%s/%s)", selection.ModelName, selection.ProviderID, selection.ModelID))
}

// handleNewModel creates a new session with a specific model
func (b *Bot) handleNewModel(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()

	if len(args) != 2 {
		return c.Send("Please specify session name and model number.\nUsage: /newmodel <name> <number>\nUse /models to view available models and their numbers.")
	}

	name := args[0]
	modelNum, err := strconv.Atoi(args[1])
	if err != nil {
		return c.Send(fmt.Sprintf("Invalid model number: %s. Number must be an integer.\nUse /models to view available models and their numbers.", args[1]))
	}

	// Get model selection from mapping
	selection, exists := b.getModelSelection(userID, modelNum)
	if !exists {
		return c.Send(fmt.Sprintf("Model with number %d not found. Please use /models to view the latest model list first.", modelNum))
	}

	// Create session with timeout
	ctx, cancel := context.WithTimeout(b.ctx, 30*time.Second)
	defer cancel()

	sessionID, err := b.sessionManager.CreateNewSessionWithModel(ctx, userID, name, selection.ProviderID, selection.ModelID)
	if err != nil {
		log.Errorf("Failed to create session with model: %v", err)
		return c.Send(fmt.Sprintf("Failed to create session: %v", err))
	}

	// Set as current session
	b.sessionManager.SetUserSession(userID, sessionID)

	return c.Send(fmt.Sprintf("✅ Created new session '%s' with model %s (%s/%s)", name, selection.ModelName, selection.ProviderID, selection.ModelID))
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

	// Cancel any existing streaming for this session
	b.streamingStateMu.Lock()
	if existingState, ok := b.streamingStates[sessionID]; ok && existingState.isStreaming {
		existingState.cancel()
		close(existingState.stopUpdates)
		existingState.isStreaming = false
		log.Infof("Cancelled existing streaming for session %s before starting new request", sessionID)
	}
	b.streamingStateMu.Unlock()

	// Send initial "processing" message
	processingMsg, err := c.Bot().Send(c.Chat(), "🤖 Processing...")
	if err != nil {
		return err
	}

	// Prepare context for the streaming request
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()

	// Channel to signal when to stop updates
	stopUpdates := make(chan struct{})
	defer close(stopUpdates)

	// Track streaming state
	streamingState := &streamingState{
		ctx:         ctx,
		cancel:      cancel,
		stopUpdates: stopUpdates,
		telegramMsg: processingMsg,
		telegramCtx: c,
		content:     &strings.Builder{},
		lastUpdate:  time.Now(),
		updateMutex: &sync.Mutex{},
		isStreaming: true,
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

	// Stream callback function to handle real-time updates
	streamCallback := func(textChunk string) error {
		return b.handleStreamChunk(streamingState, textChunk)
	}

	// Start streaming the message
	err = b.opencodeClient.StreamMessage(ctx, sessionID, text, streamCallback)
	if err != nil {
		log.Errorf("Failed to stream message: %v", err)

		// Update with error message
		errorMsg := fmt.Sprintf("Processing error: %v", err)
		if len(errorMsg) > 4000 {
			errorMsg = errorMsg[:4000]
		}

		// Add any partial content we received
		finalContent := streamingState.content.String()
		if finalContent != "" {
			errorMsg = finalContent + "\n\n---\n\n" + errorMsg
		}

		c.Bot().Edit(processingMsg, errorMsg)
		return nil
	}

	// Streaming completed successfully
	// Mark streaming as complete
	streamingState.isComplete = true

	// Get final content
	finalContent := streamingState.content.String()
	if finalContent == "" {
		finalContent = "🤖 Response completed with no content."
	}

	// Handle final content (may need to split into multiple messages)
	b.handleFinalResponse(c, processingMsg, finalContent)

	return nil
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
	sb.WriteString(fmt.Sprintf("📁 File List: %s\n\n", path))

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
			sb.WriteString(fmt.Sprintf("  • %s%s\n", dir.Name, ignored))
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
			sb.WriteString(fmt.Sprintf("  • %s%s\n", file.Name, ignored))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("Total: %d items (%d directories, %d files)", len(files), len(dirs), len(fileList)))

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
	sb.WriteString(fmt.Sprintf("🔍 Search Results: '%s'\n\n", query))

	// Limit results to prevent message overflow
	maxResults := 10
	if len(results) > maxResults {
		sb.WriteString(fmt.Sprintf("Found %d results, showing first %d:\n\n", len(results), maxResults))
		results = results[:maxResults]
	}

	for i, result := range results {
		sb.WriteString(fmt.Sprintf("%d. %s:%d\n", i+1, result.Path, result.Line))
		sb.WriteString(fmt.Sprintf("   %s\n\n", strings.TrimSpace(result.Content)))
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
	sb.WriteString(fmt.Sprintf("🔍 File Search Results: '%s'\n\n", pattern))

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
		sb.WriteString(fmt.Sprintf("Found %d results, showing first %d:\n\n", totalResults, maxResults))
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
			sb.WriteString(fmt.Sprintf("  • %s%s\n", dir.Path, ignored))
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
			sb.WriteString(fmt.Sprintf("  • %s%s\n", file.Path, ignored))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("Total: %d items", totalResults))

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
	sb.WriteString(fmt.Sprintf("🔍 Symbol Search Results: '%s'\n\n", symbol))

	// Limit results
	maxResults := 10
	if len(results) > maxResults {
		sb.WriteString(fmt.Sprintf("Found %d results, showing first %d:\n\n", len(results), maxResults))
		results = results[:maxResults]
	}

	for i, result := range results {
		sb.WriteString(fmt.Sprintf("%d. %s (%s)\n", i+1, result.Name, result.Kind))
		sb.WriteString(fmt.Sprintf("   Location: %s:%d\n", result.Path, result.Line))
		if result.Signature != "" {
			sb.WriteString(fmt.Sprintf("   Signature: %s\n", result.Signature))
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
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, agent.Name))
		if agent.Description != "" {
			sb.WriteString(fmt.Sprintf("   Description: %s\n", agent.Description))
		}
		sb.WriteString(fmt.Sprintf("   ID: %s\n\n", agent.ID))
	}

	sb.WriteString(fmt.Sprintf("Total: %d agents", len(agents)))

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...(content too long, truncated)"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleCommand(c telebot.Context) error {
	return c.Send("Command list functionality is not yet implemented.")
}

// storeModelMapping stores the model mapping for a user
func (b *Bot) storeModelMapping(userID int64, mapping map[int]modelSelection) {
	b.modelMappingMu.Lock()
	defer b.modelMappingMu.Unlock()
	b.modelMapping[userID] = mapping
}

// getModelSelection gets a model selection by ID for a user
func (b *Bot) getModelSelection(userID int64, modelID int) (modelSelection, bool) {
	b.modelMappingMu.RLock()
	defer b.modelMappingMu.RUnlock()

	userMapping, exists := b.modelMapping[userID]
	if !exists {
		return modelSelection{}, false
	}

	selection, exists := userMapping[modelID]
	return selection, exists
}

// clearModelMapping clears the model mapping for a user
func (b *Bot) clearModelMapping(userID int64) {
	b.modelMappingMu.Lock()
	defer b.modelMappingMu.Unlock()
	delete(b.modelMapping, userID)
}

// periodicMessageUpdates periodically updates a message with the latest session status
func (b *Bot) periodicMessageUpdates(ctx context.Context, c telebot.Context, msg *telebot.Message, sessionID string, stopCh <-chan struct{}) {
	// Ticker for periodic updates (every 5 seconds)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Track the last message ID we've processed to avoid repeated updates
	lastProcessedMsgID := ""
	// Track if we've seen a completed message
	hasCompletedMessage := false
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
			updateCount++
			log.Debugf("Periodic update #%d for session %s", updateCount, sessionID)
			// Get latest messages from the session
			messages, err := b.opencodeClient.GetMessages(ctx, sessionID)
			if err != nil {
				log.Errorf("Failed to get messages for periodic update: %v", err)
				continue
			}

			log.Debugf("Found %d total messages in session %s", len(messages), sessionID)
			if len(messages) == 0 {
				continue
			}

			// Find the latest assistant message
			var latestAssistantMsg opencode.Message
			foundAssistantMsg := false

			// Search from newest to oldest
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Role == "assistant" {
					latestAssistantMsg = messages[i]
					foundAssistantMsg = true
					break
				}
			}

			if !foundAssistantMsg {
				log.Debugf("No assistant message found yet for session %s, showing processing", sessionID)
				// No assistant message yet, just show processing
				b.updateTelegramMessage(c, msg, "🤖 Processing...\n\nModel is thinking, please wait...")
				continue
			}

			log.Debugf("Latest assistant message ID: %s, finish: %s, last processed: %s",
				latestAssistantMsg.ID, latestAssistantMsg.Finish, lastProcessedMsgID)

			// Check if this is the same message we already processed
			if latestAssistantMsg.ID == lastProcessedMsgID && !hasCompletedMessage {
				// Same message, no need to update unless it's now completed
				if latestAssistantMsg.Finish == "" {
					log.Debugf("Same incomplete message, skipping update")
					continue
				}
			}

			// Update last processed message ID
			lastProcessedMsgID = latestAssistantMsg.ID

			// Check if message is completed
			if latestAssistantMsg.Finish != "" {
				hasCompletedMessage = true
				log.Debugf("Message marked as completed with finish reason: %s", latestAssistantMsg.Finish)
			}

			// Format the message for display
			displayText := b.formatMessageForDisplay(latestAssistantMsg, hasCompletedMessage)
			log.Debugf("Formatted display text length: %d chars", len(displayText))

			// Update the Telegram message
			b.updateTelegramMessage(c, msg, displayText)
			log.Debugf("Telegram message updated for session %s (hasCompleted: %v)", sessionID, hasCompletedMessage)

			// If message is completed and we've shown it, we can stop updates
			// But wait a couple more cycles to ensure everything is shown
			if hasCompletedMessage {
				log.Debugf("Message completed, will continue for a few more updates")
				// Continue for a few more updates to ensure final state is shown
				// The stopCh or context will eventually stop this goroutine
			}
		}
	}
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
		partsStr := formatMessageParts(msg.Parts)
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
			sb.WriteString(fmt.Sprintf(" (Reason: %s)", msg.Finish))
		}
		if msg.ModelID != "" {
			sb.WriteString(fmt.Sprintf("\n🤖 Model: %s", msg.ModelID))
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

// updateTelegramMessage updates a Telegram message with new content
func (b *Bot) updateTelegramMessage(c telebot.Context, msg *telebot.Message, content string) {
	if msg == nil {
		log.Warn("updateTelegramMessage called with nil message")
		return
	}

	// Ensure content is not too long for Telegram
	if len(content) > 4000 {
		log.Debugf("Message content too long (%d chars), truncating to 4000", len(content))
		content = content[:4000] + "\n...(content too long, truncated)"
	}

	// Try to update the message
	if _, err := c.Bot().Edit(msg, content); err != nil {
		log.Warnf("Failed to update Telegram message: %v", err)
		// If editing fails, try to send a new message
		newMsg, err := c.Bot().Send(c.Chat(), content)
		if err != nil {
			log.Errorf("Failed to send new message: %v", err)
			return
		}
		// Update the message reference for future updates
		*msg = *newMsg
		log.Debugf("Sent new message due to edit failure, new message ID: %d", newMsg.ID)
	} else {
		log.Debugf("Successfully edited message ID %d", msg.ID)
	}
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
	if err := b.sessionManager.RenameSession(b.ctx, sessionID, newName); err != nil {
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

	// Append the new chunk to our content
	state.content.WriteString(textChunk)
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

	// Estimate content growth since last update (rough)
	contentGrowth := len(textChunk) // This chunk size

	shouldUpdate := false
	if timeSinceLastUpdate >= 2*time.Second {
		shouldUpdate = true
	} else if timeSinceLastUpdate >= 500*time.Millisecond && contentGrowth >= 100 {
		// Significant content growth, update more frequently
		shouldUpdate = true
	} else if currentLength < 1000 && timeSinceLastUpdate >= 1*time.Second {
		// For short content, update more frequently to show progress
		shouldUpdate = true
	}

	if !shouldUpdate {
		return nil
	}

	state.lastUpdate = now

	// Format the content for display
	displayContent := b.formatStreamingContent(currentContent)

	// Update the Telegram message
	b.updateTelegramMessage(state.telegramCtx, state.telegramMsg, displayContent)

	return nil
}

// formatStreamingContent formats streaming content for display
func (b *Bot) formatStreamingContent(content string) string {
	// Trim trailing whitespace
	content = strings.TrimSpace(content)

	if content == "" {
		return "🤖 Processing..."
	}

	// Calculate approximate percentage complete based on content length
	// This is a rough estimate since we don't know the total length
	progressIndicator := "▌"
	contentLength := len(content)

	// Simple heuristic: if content is getting long, show progress
	var progressText string
	if contentLength > 5000 {
		progressText = " (streaming... ~80%)"
	} else if contentLength > 3000 {
		progressText = " (streaming... ~60%)"
	} else if contentLength > 1500 {
		progressText = " (streaming... ~40%)"
	} else if contentLength > 500 {
		progressText = " (streaming... ~20%)"
	} else {
		progressText = " (streaming...)"
	}

	// If content is getting long, show a truncated version
	displayContent := content
	if len(displayContent) > 3000 {
		// Show last 3000 characters to keep message readable
		// Try to find a good truncation point (not in middle of line)
		if len(displayContent) > 3000 {
			truncated := displayContent
			// Find the last newline before 3000 characters
			cutPoint := 3000
			if cutPoint < len(truncated) {
				// Try to cut at a newline
				newlineCut := strings.LastIndex(truncated[:cutPoint], "\n")
				if newlineCut > 2500 { // Only use if it's not too far back
					cutPoint = newlineCut
				}
			}
			if cutPoint < len(truncated) {
				displayContent = truncated[cutPoint:]
				// Add ellipsis to show content was truncated
				displayContent = "..." + displayContent
			}
		}
	}

	return fmt.Sprintf("🤖%s\n%s%s", progressIndicator, displayContent, progressText)
}

// handleFinalResponse handles the final response after streaming is complete
func (b *Bot) handleFinalResponse(c telebot.Context, msg *telebot.Message, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		content = "🤖 Response completed."
	}

	// Check if content is too long for a single Telegram message
	if len(content) <= 3500 {
		// Content fits in one message, just update it
		finalMessage := fmt.Sprintf("✅ %s", content)
		b.updateTelegramMessage(c, msg, finalMessage)
		return
	}

	// Content is too long, we need to split it
	// First, update the original message to indicate completion
	b.updateTelegramMessage(c, msg, "✅ Response completed. Content is too long for one message, sending in parts...")

	// Split the content into manageable chunks
	chunks := b.splitLongContent(content)

	// Send each chunk as a separate message
	for i, chunk := range chunks {
		// Add header for multi-part messages
		header := fmt.Sprintf("Part %d/%d:\n", i+1, len(chunks))
		message := header + chunk

		if i == 0 {
			// First chunk replaces the original message
			b.updateTelegramMessage(c, msg, message)
		} else {
			// Subsequent chunks are new messages
			_, err := c.Bot().Send(c.Chat(), message)
			if err != nil {
				log.Errorf("Failed to send message part %d: %v", i+1, err)
				// Try to send error message
				c.Bot().Send(c.Chat(), fmt.Sprintf("Failed to send part %d of response", i+1))
			}
		}

		// Small delay between messages to avoid rate limiting
		if i < len(chunks)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// splitLongContent splits long content into chunks that fit in Telegram messages
func (b *Bot) splitLongContent(content string) []string {
	const maxChunkSize = 3500
	var chunks []string

	// Try to split at natural boundaries (paragraphs, code blocks)
	lines := strings.Split(content, "\n")
	currentChunk := strings.Builder{}
	currentLength := 0

	for _, line := range lines {
		lineLength := len(line) + 1 // +1 for newline

		// If adding this line would exceed the limit and we already have content,
		// start a new chunk
		if currentLength > 0 && currentLength+lineLength > maxChunkSize {
			chunks = append(chunks, currentChunk.String())
			currentChunk.Reset()
			currentLength = 0
		}

		// Add the line
		if currentChunk.Len() > 0 {
			currentChunk.WriteString("\n")
			currentLength += 1
		}
		currentChunk.WriteString(line)
		currentLength += len(line)
	}

	// Add the last chunk if there's any content
	if currentChunk.Len() > 0 {
		chunks = append(chunks, currentChunk.String())
	}

	// If we couldn't split nicely (e.g., one very long line), fall back to simple splitting
	if len(chunks) == 0 && len(content) > 0 {
		for i := 0; i < len(content); i += maxChunkSize {
			end := i + maxChunkSize
			if end > len(content) {
				end = len(content)
			}
			chunks = append(chunks, content[i:end])
		}
	}

	return chunks
}

// Close closes the bot and releases resources
func (b *Bot) Close() error {
	if b.sessionManager != nil {
		return b.sessionManager.Close()
	}
	return nil
}
