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

	// Create OpenCode client
	client := opencode.NewClient(cfg.OpenCode.URL, cfg.OpenCode.Timeout)

	// Test OpenCode connection
	healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second)
	defer healthCancel()

	if err := client.HealthCheck(healthCtx); err != nil {
		log.Warnf("OpenCode health check failed: %v", err)
		// Continue anyway, as the server might become available later
	} else {
		log.Info("OpenCode connection successful")
	}

	// Create session manager
	sessionManager := session.NewManager(client)

	bot := &Bot{
		config:         cfg,
		opencodeClient: client,
		sessionManager: sessionManager,
		ctx:            ctx,
		cancel:         cancel,
		modelMapping:   make(map[int64]map[int]modelSelection),
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

	// Handle plain text messages (non-commands)
	b.tgBot.Handle(telebot.OnText, b.handleText)
}

// handleStart handles the /start command
func (b *Bot) handleStart(c telebot.Context) error {
	user := c.Sender()
	message := fmt.Sprintf(`👋 你好 %s!

欢迎使用 OpenCode Telegram Bot。

我是一个 AI 编程助手，可以帮助你：
• 编写和重构代码
• 回答编程问题
• 浏览项目文件
• 搜索代码和符号

基本命令：
/start - 显示此帮助信息
/help - 显示详细帮助
/sessions - 列出你的会话
/new [名称] - 创建新会话
/switch <会话ID> - 切换会话
/current - 显示当前会话
/status - 查看当前任务状态

发送任何非命令文本，我将将其作为指令发送给 OpenCode。

使用 /help 查看所有可用命令。`, user.FirstName)

	return c.Send(message)
}

// handleHelp handles the /help command
func (b *Bot) handleHelp(c telebot.Context) error {
	helpText := `📚 OpenCode Bot 帮助

核心命令：
• /start - 显示欢迎信息
• /help - 显示此帮助
• /sessions - 列出所有会话
• /new [名称] - 创建新会话
• /switch <会话ID> - 切换当前会话
• /current - 显示当前会话信息
• /abort - 中止当前任务
• /status - 查看当前任务状态

文件操作：
• /files [路径] - 浏览项目文件（默认当前目录）
• /search <模式> - 搜索代码文本
• /findfile <模式> - 搜索文件
• /symbol <符号> - 搜索符号（函数、类等）

系统信息：
• /agent - 列出可用 AI 代理
• /command - 列出可用命令

	模型选择：
• /models - 列出可用 AI 模型（显示编号）
• /providers - 列出 AI 提供商
• /setmodel <编号> - 设置当前会话模型
• /newmodel <名称> <编号> - 创建新会话并指定模型

交互模式：
发送任何非命令文本，我会将其作为指令发送给 OpenCode 并流式返回响应。

注意事项：
• 每个用户默认有一个会话
• 使用 /new 创建多个会话用于不同任务
• 使用 /abort 可以中止长时间运行的任务
• 发送新消息会自动中止之前的流式响应`

	return c.Send(helpText)
}

// handleSessions handles the /sessions command
func (b *Bot) handleSessions(c telebot.Context) error {
	userID := c.Sender().ID
	sessions, err := b.sessionManager.ListUserSessions(b.ctx, userID)
	if err != nil {
		log.Errorf("Failed to list sessions: %v", err)
		return c.Send(fmt.Sprintf("获取会话列表失败: %v", err))
	}

	if len(sessions) == 0 {
		return c.Send("你还没有任何会话。使用 /new 创建一个新会话。")
	}

	var sb strings.Builder
	sb.WriteString("📋 你的会话：\n\n")

	currentSessionID, hasCurrent := b.sessionManager.GetUserSession(userID)

	for i, sess := range sessions {
		prefix := "  "
		if hasCurrent && sess.SessionID == currentSessionID {
			prefix = "✅ "
		}
		sb.WriteString(fmt.Sprintf("%s%d. `%s`\n", prefix, i+1, sess.SessionID))
		sb.WriteString(fmt.Sprintf("   名称: %s\n", sess.Name))
		sb.WriteString(fmt.Sprintf("   创建: %s\n", sess.CreatedAt.Format("2006-01-02 15:04")))
		sb.WriteString(fmt.Sprintf("   最后使用: %s\n", sess.LastUsedAt.Format("2006-01-02 15:04")))
		sb.WriteString(fmt.Sprintf("   消息数: %d\n", sess.MessageCount))
		if sess.ProviderID != "" && sess.ModelID != "" {
			sb.WriteString(fmt.Sprintf("   模型: %s/%s\n", sess.ProviderID, sess.ModelID))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("使用 /switch <会话ID> 切换会话，或 /new 创建新会话。")

	return c.Send(sb.String())
}

// handleNew handles the /new command
func (b *Bot) handleNew(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()

	name := "新会话"
	if len(args) > 0 {
		name = strings.Join(args, " ")
	}

	sessionID, err := b.sessionManager.CreateNewSession(b.ctx, userID, name)
	if err != nil {
		log.Errorf("Failed to create session: %v", err)
		return c.Send(fmt.Sprintf("创建会话失败: %v", err))
	}

	// Set as current session
	b.sessionManager.SetUserSession(userID, sessionID)

	return c.Send(fmt.Sprintf("✅ 已创建新会话：%s\n会话ID: `%s`\n\n此会话已设置为当前会话。", name, sessionID))
}

// handleSwitch handles the /switch command
func (b *Bot) handleSwitch(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()

	if len(args) == 0 {
		return c.Send("请指定要切换到的会话ID。\n用法: /switch <会话ID>\n使用 /sessions 查看你的会话列表。")
	}

	sessionID := args[0]

	// Check if session exists for this user
	sessions, err := b.sessionManager.ListUserSessions(b.ctx, userID)
	if err != nil {
		log.Errorf("Failed to get user sessions: %v", err)
		return c.Send(fmt.Sprintf("获取会话列表失败: %v", err))
	}
	found := false
	for _, sess := range sessions {
		if sess.SessionID == sessionID {
			found = true
			break
		}
	}

	if !found {
		return c.Send("未找到该会话ID，或会话不属于你。\n使用 /sessions 查看你的会话列表。")
	}

	if err := b.sessionManager.SetUserSession(userID, sessionID); err != nil {
		log.Errorf("Failed to switch session: %v", err)
		return c.Send(fmt.Sprintf("切换会话失败: %v", err))
	}

	return c.Send(fmt.Sprintf("✅ 已切换到会话：`%s`", sessionID))
}

// handleCurrent handles the /current command
func (b *Bot) handleCurrent(c telebot.Context) error {
	userID := c.Sender().ID
	sessionID, exists := b.sessionManager.GetUserSession(userID)

	if !exists {
		return c.Send("你还没有当前会话。使用 /new 创建一个新会话。")
	}

	meta, exists := b.sessionManager.GetSessionMeta(sessionID)
	if !exists {
		return c.Send("会话信息已丢失。使用 /new 创建一个新会话。")
	}

	// Get session details from OpenCode
	session, err := b.opencodeClient.GetSession(b.ctx, sessionID)
	if err != nil {
		log.Errorf("Failed to get session details: %v", err)
		// Continue with basic info
	}

	var sb strings.Builder
	sb.WriteString("📁 当前会话信息\n\n")
	sb.WriteString(fmt.Sprintf("会话ID: `%s`\n", sessionID))
	sb.WriteString(fmt.Sprintf("名称: %s\n", meta.Name))
	sb.WriteString(fmt.Sprintf("创建时间: %s\n", meta.CreatedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("最后使用: %s\n", meta.LastUsedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("消息数: %d\n", meta.MessageCount))
	if meta.ProviderID != "" && meta.ModelID != "" {
		sb.WriteString(fmt.Sprintf("当前模型: %s/%s\n", meta.ProviderID, meta.ModelID))
	} else {
		sb.WriteString("当前模型: 默认\n")
	}

	if session != nil {
		createdAt := time.UnixMilli(session.Time.Created)
		sb.WriteString(fmt.Sprintf("OpenCode 创建时间: %s\n", createdAt.Format("2006-01-02 15:04:05")))
		updatedAt := time.UnixMilli(session.Time.Updated)
		sb.WriteString(fmt.Sprintf("OpenCode 更新时间: %s\n", updatedAt.Format("2006-01-02 15:04:05")))
	}

	sb.WriteString("\n使用 /sessions 查看所有会话，或 /switch 切换会话。")

	return c.Send(sb.String())
}

// handleAbort handles the /abort command
func (b *Bot) handleAbort(c telebot.Context) error {
	userID := c.Sender().ID
	sessionID, exists := b.sessionManager.GetUserSession(userID)

	if !exists {
		return c.Send("你还没有当前会话。")
	}

	if err := b.opencodeClient.AbortSession(b.ctx, sessionID); err != nil {
		log.Errorf("Failed to abort session: %v", err)
		return c.Send(fmt.Sprintf("中止会话失败: %v", err))
	}

	return c.Send("🛑 已发送中止信号。当前任务将被中断。")
}

// formatMessageParts formats message parts for display
func formatMessageParts(parts []interface{}) string {
	if len(parts) == 0 {
		return "无详细内容"
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
					if len(reasoningText) > 300 {
						reasoningText = reasoningText[:300] + "..."
					}
					sb.WriteString(fmt.Sprintf("🤔 推理过程:\n%s\n", reasoningText))
				} else {
					sb.WriteString("🤔 推理过程: 已处理\n")
				}
			case "step-start":
				// Skip "任务开始" message as it's redundant
				// sb.WriteString("🚀 任务开始\n")
			case "step-finish":
				finishMsg := fmt.Sprintf("✅ 任务完成")
				if partResp.Reason != "" {
					finishMsg += fmt.Sprintf(" (原因: %s)", partResp.Reason)
				}
				if partResp.Cost > 0 {
					finishMsg += fmt.Sprintf(" [成本: %.4f]", partResp.Cost)
				}
				sb.WriteString(finishMsg + "\n")
			case "tool":
				toolInfo := "🛠️ 工具调用"

				// Try to parse snapshot as JSON for more details
				if partResp.Snapshot != "" {
					var snapshotData map[string]interface{}
					if err := json.Unmarshal([]byte(partResp.Snapshot), &snapshotData); err == nil {
						// Extract tool name/type from various possible fields
						toolName := ""
						if name, ok := snapshotData["name"].(string); ok && name != "" {
							toolName = name
						} else if toolType, ok := snapshotData["type"].(string); ok && toolType != "" {
							toolName = toolType
						} else if tool, ok := snapshotData["tool"].(string); ok && tool != "" {
							toolName = tool
						}

						if toolName != "" {
							toolInfo += fmt.Sprintf(": %s", toolName)

							// Try to show arguments if available
							if args, ok := snapshotData["args"].(map[string]interface{}); ok && len(args) > 0 {
								// Show first few args
								var argStrs []string
								for k, v := range args {
									argStr := fmt.Sprintf("%s", v)
									if len(argStr) > 30 {
										argStr = argStr[:30] + "..."
									}
									argStrs = append(argStrs, fmt.Sprintf("%s=%s", k, argStr))
								}
								if len(argStrs) > 0 {
									// Show at most 2 arguments
									maxArgs := 2
									if maxArgs > len(argStrs) {
										maxArgs = len(argStrs)
									}
									toolInfo += fmt.Sprintf(" (%s)", strings.Join(argStrs[:maxArgs], ", "))
								}
							} else if input, ok := snapshotData["input"].(string); ok && input != "" {
								// Show truncated input
								if len(input) > 50 {
									input = input[:50] + "..."
								}
								toolInfo += fmt.Sprintf(" (%s)", input)
							}
						} else {
							// Fallback to showing first 100 chars of snapshot
							snapshot := partResp.Snapshot
							if len(snapshot) > 100 {
								snapshot = snapshot[:100] + "..."
							}
							toolInfo += fmt.Sprintf(": %s", snapshot)
						}
					} else {
						// Not JSON, show truncated snapshot
						snapshot := partResp.Snapshot
						if len(snapshot) > 100 {
							snapshot = snapshot[:100] + "..."
						}
						toolInfo += fmt.Sprintf(": %s", snapshot)
					}
				} else if partResp.Reason != "" {
					toolInfo += fmt.Sprintf(" (%s)", partResp.Reason)
				}
				sb.WriteString(toolInfo + "\n")
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
						sb.WriteString(fmt.Sprintf("🤔 推理过程:\n%s\n", reasoningText))
					} else {
						sb.WriteString("🤔 推理过程: 已处理\n")
					}
				default:
					sb.WriteString(fmt.Sprintf("🔹 %s\n", partType))
				}
			} else {
				sb.WriteString(fmt.Sprintf("🔹 未知类型\n"))
			}
		} else {
			sb.WriteString(fmt.Sprintf("🔹 未知部件\n"))
		}
	}

	// Add text content at the end if we have any
	if hasTextContent {
		text := strings.TrimSpace(textContent.String())
		if text != "" {
			// Truncate if too long
			if len(text) > 1000 {
				text = text[:1000] + "..."
			}
			sb.WriteString(fmt.Sprintf("\n💬 回复内容:\n%s\n", text))
		}
	}

	result := strings.TrimSpace(sb.String())
	if result == "" {
		return "无详细内容"
	}
	return result
}

// handleStatus handles the /status command
func (b *Bot) handleStatus(c telebot.Context) error {
	userID := c.Sender().ID
	sessionID, exists := b.sessionManager.GetUserSession(userID)

	if !exists {
		return c.Send("你还没有当前会话。使用 /new 创建一个新会话。")
	}

	// Get recent messages
	messages, err := b.opencodeClient.GetMessages(b.ctx, sessionID)
	if err != nil {
		log.Errorf("Failed to get messages: %v", err)
		return c.Send(fmt.Sprintf("获取消息失败: %v", err))
	}

	if len(messages) == 0 {
		return c.Send("当前会话还没有消息。")
	}

	var sb strings.Builder
	sb.WriteString("📊 会话状态\n\n")

	// Show session info
	session, err := b.opencodeClient.GetSession(b.ctx, sessionID)
	if err == nil && session != nil {
		sb.WriteString(fmt.Sprintf("标题: %s\n", session.Title))
		sb.WriteString(fmt.Sprintf("ID: `%s`\n", session.ID))
		createdAt := time.UnixMilli(session.Time.Created)
		sb.WriteString(fmt.Sprintf("创建: %s\n", createdAt.Format("2006-01-02 15:04")))
	}

	sb.WriteString(fmt.Sprintf("消息数: %d\n\n", len(messages)))

	// Show last 3 messages in a cleaner format
	start := len(messages) - 3
	if start < 0 {
		start = 0
	}

	sb.WriteString("最近消息:\n")
	sb.WriteString("═══════════════════════════════\n")

	for i := start; i < len(messages); i++ {
		msg := messages[i]
		role := "👤 你"
		if msg.Role == "assistant" {
			role = "🤖 助手"
		} else if msg.Role == "system" {
			role = "⚙️ 系统"
		}
		timeStr := msg.CreatedAt.Format("15:04")

		sb.WriteString(fmt.Sprintf("\n%s [%s]\n", role, timeStr))
		sb.WriteString("───────────────────────────────\n")

		// Show message content
		if msg.Content != "" {
			content := msg.Content
			if len(content) > 400 {
				content = content[:400] + "..."
			}
			sb.WriteString(fmt.Sprintf("%s\n", content))
		} else if len(msg.Parts) > 0 {
			// If no direct content, try to extract from parts
			partsStr := formatMessageParts(msg.Parts)
			if partsStr != "无详细内容" {
				sb.WriteString(fmt.Sprintf("%s\n", partsStr))
			} else {
				sb.WriteString("（无内容）\n")
			}
		} else {
			sb.WriteString("（无内容）\n")
		}

		// Only show detailed process for assistant messages with multiple parts
		if msg.Role == "assistant" && len(msg.Parts) > 1 {
			partsStr := formatMessageParts(msg.Parts)
			if partsStr != "无详细内容" && !strings.Contains(partsStr, "💬 回复内容:") {
				// Already included in formatMessageParts output
			}
		}
	}

	// Show current status
	sb.WriteString("\n═══════════════════════════════\n")
	if len(messages) > 0 {
		lastMsg := messages[len(messages)-1]
		if lastMsg.Role == "assistant" && lastMsg.Finish != "" {
			sb.WriteString("📊 状态: 等待你的输入\n")
		} else {
			sb.WriteString("📊 状态: 助手正在处理中...\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n使用 /current 查看会话详情，/sessions 管理会话。"))

	// Truncate if too long
	result := sb.String()
	if len(result) > 4000 {
		result = result[:4000] + "\n...（内容过长，已截断）"
	}

	return c.Send(result)
}

// handleModels lists available AI models
func (b *Bot) handleModels(c telebot.Context) error {
	providersResp, err := b.opencodeClient.GetProviders(b.ctx)
	if err != nil {
		log.Errorf("Failed to get providers: %v", err)
		return c.Send(fmt.Sprintf("获取模型列表失败: %v", err))
	}

	var sb strings.Builder
	sb.WriteString("🤖 可用 AI 模型\n\n")

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
		sb.WriteString("⚠️ 没有已连接的 AI 提供商。\n")
		sb.WriteString("请先配置至少一个 AI 提供商的 API 密钥。\n\n")

		// Show all available providers for reference
		sb.WriteString("可配置的 AI 提供商:\n")
		for _, provider := range providersResp.All {
			sb.WriteString(fmt.Sprintf("  • %s (%s)\n", provider.Name, provider.ID))
			if len(provider.Env) > 0 {
				sb.WriteString(fmt.Sprintf("    需要环境变量: %s\n", strings.Join(provider.Env, ", ")))
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
		sb.WriteString("\n📝 使用说明:\n")
		sb.WriteString("• 使用 /setmodel <编号> 设置当前会话模型\n")
		sb.WriteString("• 使用 /newmodel <名称> <编号> 创建新会话并指定模型\n")
	}

	// Store the model mapping in the bot context (for this user)
	// We'll store it in a simple way for now - could be enhanced with persistence
	b.storeModelMapping(c.Sender().ID, modelMapping)

	result := sb.String()
	if len(result) > 4000 {
		result = result[:4000] + "\n...（内容过长，已截断）"
	}
	return c.Send(result)
}

// handleProviders lists AI providers
func (b *Bot) handleProviders(c telebot.Context) error {
	providersResp, err := b.opencodeClient.GetProviders(b.ctx)
	if err != nil {
		log.Errorf("Failed to get providers: %v", err)
		return c.Send(fmt.Sprintf("获取提供商失败: %v", err))
	}

	// Create a set of connected provider IDs for faster lookup
	connectedSet := make(map[string]bool)
	for _, providerID := range providersResp.Connected {
		connectedSet[providerID] = true
	}

	var sb strings.Builder
	sb.WriteString("🏢 AI 提供商\n\n")

	// Show connected providers first
	hasConnected := false
	for _, provider := range providersResp.All {
		if connectedSet[provider.ID] {
			if !hasConnected {
				sb.WriteString("✅ 已连接提供商:\n\n")
				hasConnected = true
			}
			sb.WriteString(fmt.Sprintf("✅ %s\n", provider.Name))
			sb.WriteString(fmt.Sprintf("  ID: %s\n", provider.ID))
			sb.WriteString(fmt.Sprintf("  来源: %s\n", provider.Source))
			if len(provider.Env) > 0 {
				sb.WriteString(fmt.Sprintf("  环境变量: %s\n", strings.Join(provider.Env, ", ")))
			}
			if len(provider.Models) > 0 {
				sb.WriteString(fmt.Sprintf("  模型数: %d\n", len(provider.Models)))
			}
			sb.WriteString("\n")
		}
	}

	// Show unconnected providers
	hasUnconnected := false
	for _, provider := range providersResp.All {
		if !connectedSet[provider.ID] {
			if !hasUnconnected {
				sb.WriteString("⚠️ 未连接提供商 (需要配置API密钥):\n\n")
				hasUnconnected = true
			}
			sb.WriteString(fmt.Sprintf("⚪ %s\n", provider.Name))
			sb.WriteString(fmt.Sprintf("  ID: %s\n", provider.ID))
			sb.WriteString(fmt.Sprintf("  来源: %s\n", provider.Source))
			if len(provider.Env) > 0 {
				sb.WriteString(fmt.Sprintf("  需要环境变量: %s\n", strings.Join(provider.Env, ", ")))
			}
			if len(provider.Models) > 0 {
				sb.WriteString(fmt.Sprintf("  可用模型数: %d\n", len(provider.Models)))
			}
			sb.WriteString("\n")
		}
	}

	// Summary
	sb.WriteString("📊 摘要:\n")
	sb.WriteString(fmt.Sprintf("  • 已连接: %d 个提供商\n", len(providersResp.Connected)))
	sb.WriteString(fmt.Sprintf("  • 总共: %d 个提供商\n", len(providersResp.All)))
	sb.WriteString("\n")

	sb.WriteString("使用 /models 查看已连接提供商的可用模型。")

	result := sb.String()
	if len(result) > 4000 {
		result = result[:4000] + "\n...（内容过长，已截断）"
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
		return c.Send("请指定模型编号。\n用法: /setmodel <编号>\n使用 /models 查看可用模型和编号。")
	}

	sessionID, exists := b.sessionManager.GetUserSession(userID)
	if !exists {
		log.Warnf("User %d has no current session", userID)
		return c.Send("你还没有当前会话。使用 /new 创建一个新会话。")
	}
	log.Debugf("User %d current session: %s", userID, sessionID)

	modelNum, err := strconv.Atoi(args[0])
	if err != nil {
		log.Warnf("Invalid model number: %s", args[0])
		return c.Send(fmt.Sprintf("无效的模型编号: %s。编号必须是整数。\n使用 /models 查看可用模型和编号。", args[0]))
	}
	log.Debugf("Model number: %d", modelNum)

	// Get model selection from mapping
	selection, exists := b.getModelSelection(userID, modelNum)
	if !exists {
		log.Warnf("Model mapping not found for user %d, model %d", userID, modelNum)
		return c.Send(fmt.Sprintf("未找到编号为 %d 的模型。请先使用 /models 查看最新模型列表。", modelNum))
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
			return c.Send(fmt.Sprintf("设置模型超时: 模型初始化可能需要更长时间。请稍后重试或使用默认模型。"))
		}
		return c.Send(fmt.Sprintf("设置模型失败: %v", err))
	}

	log.Infof("Successfully set model for user %d session %s to %s/%s", userID, sessionID, selection.ProviderID, selection.ModelID)
	return c.Send(fmt.Sprintf("✅ 已设置当前会话模型为 %s (%s/%s)", selection.ModelName, selection.ProviderID, selection.ModelID))
}

// handleNewModel creates a new session with a specific model
func (b *Bot) handleNewModel(c telebot.Context) error {
	userID := c.Sender().ID
	args := c.Args()

	if len(args) != 2 {
		return c.Send("请指定会话名称和模型编号。\n用法: /newmodel <名称> <编号>\n使用 /models 查看可用模型和编号。")
	}

	name := args[0]
	modelNum, err := strconv.Atoi(args[1])
	if err != nil {
		return c.Send(fmt.Sprintf("无效的模型编号: %s。编号必须是整数。\n使用 /models 查看可用模型和编号。", args[1]))
	}

	// Get model selection from mapping
	selection, exists := b.getModelSelection(userID, modelNum)
	if !exists {
		return c.Send(fmt.Sprintf("未找到编号为 %d 的模型。请先使用 /models 查看最新模型列表。", modelNum))
	}

	// Create session with timeout
	ctx, cancel := context.WithTimeout(b.ctx, 30*time.Second)
	defer cancel()

	sessionID, err := b.sessionManager.CreateNewSessionWithModel(ctx, userID, name, selection.ProviderID, selection.ModelID)
	if err != nil {
		log.Errorf("Failed to create session with model: %v", err)
		return c.Send(fmt.Sprintf("创建会话失败: %v", err))
	}

	// Set as current session
	b.sessionManager.SetUserSession(userID, sessionID)

	return c.Send(fmt.Sprintf("✅ 已创建新会话 '%s' 并使用模型 %s (%s/%s)\n会话ID: `%s`", name, selection.ModelName, selection.ProviderID, selection.ModelID, sessionID))
}

// handleText handles plain text messages (non-commands) with periodic updates
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
		return c.Send(fmt.Sprintf("会话错误: %v", err))
	}

	// Send initial "processing" message
	processingMsg, err := c.Bot().Send(c.Chat(), "🤖 处理中...")
	if err != nil {
		return err
	}

	// Prepare context for the main request
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()

	// Channel to signal when to stop periodic updates
	stopUpdates := make(chan struct{})
	defer close(stopUpdates)

	// Start periodic updates in a goroutine
	go b.periodicMessageUpdates(ctx, c, processingMsg, sessionID, stopUpdates)

	// Send the message to OpenCode
	req := opencode.SendMessageRequest{
		Parts: []opencode.MessagePart{
			{
				Type: "text",
				Text: text,
			},
		},
	}

	// Use SendMessage which will block until response is complete
	// This allows periodic updates to show progress while waiting
	_, err = b.opencodeClient.SendMessage(ctx, sessionID, &req)
	if err != nil {
		log.Errorf("Failed to send message: %v", err)
		// Update with error message
		errorMsg := fmt.Sprintf("处理错误: %v", err)
		if len(errorMsg) > 4000 {
			errorMsg = errorMsg[:4000]
		}
		c.Bot().Edit(processingMsg, errorMsg)
		return nil
	}

	// Message sent successfully, periodic updates will handle the rest
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
		return c.Send(fmt.Sprintf("列出文件失败: %v", err))
	}

	if len(files) == 0 {
		return c.Send(fmt.Sprintf("目录 '%s' 为空或不存在。", path))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📁 文件列表: %s\n\n", path))

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
		sb.WriteString("📂 目录:\n")
		for _, dir := range dirs {
			ignored := ""
			if dir.Ignored {
				ignored = " [已忽略]"
			}
			sb.WriteString(fmt.Sprintf("  • %s%s\n", dir.Name, ignored))
		}
		sb.WriteString("\n")
	}

	// Then files
	if len(fileList) > 0 {
		sb.WriteString("📄 文件:\n")
		for _, file := range fileList {
			ignored := ""
			if file.Ignored {
				ignored = " [已忽略]"
			}
			sb.WriteString(fmt.Sprintf("  • %s%s\n", file.Name, ignored))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("总计: %d 个项目 (%d 目录, %d 文件)", len(files), len(dirs), len(fileList)))

	result := sb.String()
	if len(result) > 4000 {
		result = result[:4000] + "\n...（内容过长，已截断）"
	}

	return c.Send(result)
}

func (b *Bot) handleSearch(c telebot.Context) error {
	args := c.Args()
	if len(args) == 0 {
		return c.Send("请指定搜索内容。\n用法: /search <搜索模式>")
	}

	query := strings.Join(args, " ")

	// Try to use OpenCode search API
	results, err := b.opencodeClient.SearchFiles(b.ctx, query)
	if err != nil {
		// API not available, provide helpful message
		log.Debugf("Search API not available: %v", err)
		return c.Send(fmt.Sprintf("🔍 搜索功能当前不可用。\n\n原因: %v\n\n您可以直接向助手发送消息请求搜索，例如:\n\"搜索包含 '%s' 的代码\"", err, query))
	}

	if len(results) == 0 {
		return c.Send(fmt.Sprintf("未找到包含 '%s' 的代码。", query))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔍 搜索结果: '%s'\n\n", query))

	// Limit results to prevent message overflow
	maxResults := 10
	if len(results) > maxResults {
		sb.WriteString(fmt.Sprintf("找到 %d 个结果，显示前 %d 个:\n\n", len(results), maxResults))
		results = results[:maxResults]
	}

	for i, result := range results {
		sb.WriteString(fmt.Sprintf("%d. %s:%d\n", i+1, result.Path, result.Line))
		sb.WriteString(fmt.Sprintf("   %s\n\n", strings.TrimSpace(result.Content)))
	}

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...（内容过长，已截断）"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleFindFile(c telebot.Context) error {
	args := c.Args()
	if len(args) == 0 {
		return c.Send("请指定文件模式。\n用法: /findfile <文件模式>")
	}

	pattern := strings.Join(args, " ")

	// Try to use OpenCode find file API
	files, err := b.opencodeClient.FindFile(b.ctx, pattern)
	if err != nil {
		// API not available, provide helpful message
		log.Debugf("Find file API not available: %v", err)
		return c.Send(fmt.Sprintf("🔍 文件搜索功能当前不可用。\n\n原因: %v\n\n您可以使用 /files 命令浏览目录，或直接向助手发送消息请求查找文件。", err))
	}

	if len(files) == 0 {
		return c.Send(fmt.Sprintf("未找到匹配 '%s' 的文件。", pattern))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔍 文件搜索结果: '%s'\n\n", pattern))

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
		sb.WriteString(fmt.Sprintf("找到 %d 个结果，显示前 %d 个:\n\n", totalResults, maxResults))
		if len(dirs) > maxResults/2 {
			dirs = dirs[:maxResults/2]
		}
		if len(fileList) > maxResults/2 {
			fileList = fileList[:maxResults/2]
		}
	}

	if len(dirs) > 0 {
		sb.WriteString("📂 目录:\n")
		for _, dir := range dirs {
			ignored := ""
			if dir.Ignored {
				ignored = " [已忽略]"
			}
			sb.WriteString(fmt.Sprintf("  • %s%s\n", dir.Path, ignored))
		}
		sb.WriteString("\n")
	}

	if len(fileList) > 0 {
		sb.WriteString("📄 文件:\n")
		for _, file := range fileList {
			ignored := ""
			if file.Ignored {
				ignored = " [已忽略]"
			}
			sb.WriteString(fmt.Sprintf("  • %s%s\n", file.Path, ignored))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("总计: %d 个项目", totalResults))

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...（内容过长，已截断）"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleSymbol(c telebot.Context) error {
	args := c.Args()
	if len(args) == 0 {
		return c.Send("请指定符号名称。\n用法: /symbol <符号名称>")
	}

	symbol := strings.Join(args, " ")

	// Try to use OpenCode symbol search API
	results, err := b.opencodeClient.SearchSymbol(b.ctx, symbol)
	if err != nil {
		// API not available, provide helpful message
		log.Debugf("Symbol search API not available: %v", err)
		return c.Send(fmt.Sprintf("🔍 符号搜索功能当前不可用。\n\n原因: %v\n\n您可以直接向助手发送消息请求查找符号，例如:\n\"查找函数 %s\"", err, symbol))
	}

	if len(results) == 0 {
		return c.Send(fmt.Sprintf("未找到符号 '%s'。", symbol))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔍 符号搜索结果: '%s'\n\n", symbol))

	// Limit results
	maxResults := 10
	if len(results) > maxResults {
		sb.WriteString(fmt.Sprintf("找到 %d 个结果，显示前 %d 个:\n\n", len(results), maxResults))
		results = results[:maxResults]
	}

	for i, result := range results {
		sb.WriteString(fmt.Sprintf("%d. %s (%s)\n", i+1, result.Name, result.Kind))
		sb.WriteString(fmt.Sprintf("   位置: %s:%d\n", result.Path, result.Line))
		if result.Signature != "" {
			sb.WriteString(fmt.Sprintf("   签名: %s\n", result.Signature))
		}
		sb.WriteString("\n")
	}

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...（内容过长，已截断）"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleAgent(c telebot.Context) error {
	// Try to get agents list
	agents, err := b.opencodeClient.ListAgents(b.ctx)
	if err != nil {
		// API not available, provide helpful message
		log.Debugf("Agents API not available: %v", err)
		return c.Send(fmt.Sprintf("🤖 代理列表功能当前不可用。\n\n原因: %v\n\n您可以使用 /models 和 /providers 命令查看可用的 AI 模型和提供商。", err))
	}

	if len(agents) == 0 {
		return c.Send("当前没有可用的 AI 代理。")
	}

	var sb strings.Builder
	sb.WriteString("🤖 可用 AI 代理:\n\n")

	for i, agent := range agents {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, agent.Name))
		if agent.Description != "" {
			sb.WriteString(fmt.Sprintf("   描述: %s\n", agent.Description))
		}
		sb.WriteString(fmt.Sprintf("   ID: %s\n\n", agent.ID))
	}

	sb.WriteString(fmt.Sprintf("总计: %d 个代理", len(agents)))

	resultStr := sb.String()
	if len(resultStr) > 4000 {
		resultStr = resultStr[:4000] + "\n...（内容过长，已截断）"
	}

	return c.Send(resultStr)
}

func (b *Bot) handleCommand(c telebot.Context) error {
	return c.Send("命令列表功能暂未实现。")
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
				b.updateTelegramMessage(c, msg, "🤖 处理中...\n\n模型正在思考中，请稍候...")
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
		sb.WriteString("✅ 任务完成\n\n")
	}

	// Add message content if available
	if msg.Content != "" {
		content := msg.Content
		if len(content) > 3000 {
			content = content[:3000] + "...\n\n(内容过长，已截断)"
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	// Add detailed parts information
	if len(msg.Parts) > 0 {
		partsStr := formatMessageParts(msg.Parts)
		if partsStr != "无详细内容" {
			sb.WriteString("📋 处理过程:\n")
			sb.WriteString(partsStr)
			sb.WriteString("\n\n")
		}
	}

	// Add status
	if isCompleted {
		sb.WriteString("📊 状态: 任务已完成")
		if msg.Finish != "" {
			sb.WriteString(fmt.Sprintf(" (原因: %s)", msg.Finish))
		}
		if msg.ModelID != "" {
			sb.WriteString(fmt.Sprintf("\n🤖 模型: %s", msg.ModelID))
		}
	} else {
		// For ongoing tasks, only show the auto-update indicator at the end
		// Don't show redundant status lines
		if msg.Content == "" && len(msg.Parts) == 0 {
			// If no content yet, show minimal status
			sb.WriteString("🤖 处理中...")
		}
		sb.WriteString("\n\n⏳ 自动更新中...")
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
		content = content[:4000] + "\n...（内容过长，已截断）"
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
