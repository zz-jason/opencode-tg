# Development Notes: OpenCode Telegram Bot

- Co-Authored-By: opencode(@opencode), deepseek-reasoner
- Date: 2026-02-04

## Summary

Design a Telegram Bot for interacting with OpenCode AI programming assistant deployed in internal networks. The bot runs in internal network environments, accesses Telegram API via HTTP proxy, and uses polling to receive messages. The bot integrates with OpenCode's OpenAPI, supporting users to view task status, initiate programming tasks, enable real-time interaction, and provide a CLI-like user experience.

## Background

### Problem Context
- OpenCode is an AI programming assistant deployed on an internal server (192.168.50.100:8080), providing rich OpenAPI interfaces.
- Users want to conveniently interact with OpenCode via Telegram without directly accessing CLI or web interfaces.
- Internal network environments cannot be accessed from the external internet, so the bot must use polling to receive messages.
- Internal network access to external services requires HTTP proxy (for accessing Telegram, Google, and other overseas services).

### Usage Scenarios
1. **Basic Scenario**: Users view OpenCode's current task execution status and intermediate outputs through Telegram Bot.
2. **Advanced Scenario**: Users initiate new programming tasks (such as code generation, refactoring, testing) through the bot and engage in multi-turn interactions with OpenCode, with the ability to interrupt or provide additional information at any time.
3. **Auxiliary Scenario**: Users perform file browsing, code searching, session management, and other operations through the bot.

### Functional Requirements
- **Basic Requirements**:
  - Support connecting to Telegram API via proxy
  - Periodic message polling
  - View OpenCode session status and message history
- **Advanced Requirements**:
  - Create/select sessions
  - Send instructions to OpenCode and receive streaming responses
  - Support interactive conversations (interruptible, additional information)
  - Support common OpenCode operations (file operations, search, terminal execution, etc.)

## Detailed Design

### System Architecture

```
+-------------+      +-------------------+      +-------------------+
| Telegram    |      | Telegram Bot      |      | OpenCode Server   |
| API         |<---->| (Golang)          |<---->| (192.168.50.100:8080)|
+-------------+      +-------------------+      +-------------------+
                         ^       ^
                         |       |
                    HTTP Proxy  Internal HTTP
                    (External)   (No proxy needed)
```

**Component Description**:
1. **Telegram Bot**: Golang program using `telebot` library, configured with HTTP proxy to connect to Telegram API, supporting polling mode.
2. **Configuration Manager**: Reads and manages TOML format configuration files, including Bot Token, proxy settings, OpenCode URL, etc.
3. **OpenCode Client**: HTTP client encapsulating OpenCode OpenAPI, supporting session management, message sending, event subscription, etc.
4. **Session Manager**: Manages user-to-session mapping (memory or SQLite storage).
5. **Command Handler**: Parses user commands and calls corresponding OpenCode APIs.
6. **Message Stream Handler**: Processes OpenCode's streaming responses and pushes them to Telegram in real-time.

### Data Flow
1. **User sends command**: User sends a message (text or command) to Telegram Bot.
2. **Bot receives message**: Bot retrieves messages via polling, forwarded through proxy.
3. **Command parsing**: Bot parses the command to determine target session and operation type.
4. **Call OpenCode API**: Bot calls OpenCode API via internal HTTP request.
5. **Process response**: Bot receives OpenCode response (immediate or streaming output).
6. **Return result**: Bot formats the result and sends it back to Telegram user.

### Key Feature Implementation

#### 1. Configuration Management (TOML)
```toml
# config.toml
[telegram]
token = "YOUR_BOT_TOKEN"
polling_timeout = 60
polling_limit = 100

[proxy]
enabled = true
url = "http://proxy:port"

[opencode]
url = "http://192.168.50.100:8080"
timeout = 30

[storage]
type = "memory"  # or "sqlite"
sqlite_path = "sessions.db"

[logging]
level = "info"
output = "bot.log"
```

#### 2. Golang Proxy Configuration and Polling
```go
package main

import (
    "gopkg.in/telebot.v4"
    "net/http"
    "net/url"
    "time"
)

func main() {
    // Load configuration
    cfg := config.Load("config.toml")
    
    // Configure HTTP proxy
    proxyURL, _ := url.Parse(cfg.Proxy.URL)
    transport := &http.Transport{
        Proxy: http.ProxyURL(proxyURL),
    }
    httpClient := &http.Client{
        Transport: transport,
        Timeout:   time.Second * 30,
    }
    
    // Create Bot
    pref := telebot.Settings{
        Token:  cfg.Telegram.Token,
        Poller: &telebot.LongPoller{Timeout: cfg.Telegram.PollingTimeout},
        Client: httpClient,
    }
    
    bot, err := telebot.NewBot(pref)
    if err != nil {
        panic(err)
    }
    
    // Register command handlers
    bot.Handle("/start", startHandler)
    bot.Handle("/help", helpHandler)
    // ... other commands
    
    // Start polling
    bot.Start()
}
```

#### 3. OpenCode Client
Encapsulates main OpenCode API endpoints:
- `POST /session` - Create session
- `POST /session/{id}/message` - Send message (supports streaming)
- `GET /session` - Get session list
- `GET /session/{id}/message` - Get message history
- `POST /session/{id}/abort` - Abort session
- `GET /file`, `GET /find` and other auxiliary APIs

#### 4. Session Management Strategy
- **Default strategy**: Each Telegram user corresponds to one OpenCode session (simplified management).
- **Multi-session support**: Users can create new sessions and switch between sessions via commands.
- **Session state**: Stored in memory dictionary, extendable to SQLite persistence.

#### 5. Command Design

| Command | Parameters | Description |
|---------|------------|-------------|
| `/start` | - | Start bot, show help information |
| `/help` | - | Display command help |
| `/sessions` | - | List all sessions for current user |
| `/new` | [name] | Create new session |
| `/switch` | sessionID | Switch current session |
| `/current` | - | Show current session information |
| `/abort` | - | Abort current session task |
| `/files` | [path] | Browse project files |
| `/search` | pattern | Search code text |
| `/findfile` | pattern | Search for files |
| `/symbol` | symbol | Search for symbols |
| `/agent` | - | List available AI agents |
| `/command` | - | List available commands |
| `/status` | - | Show current task status (latest messages) |

**Non-command message processing**: Regular text sent by users will be sent as instructions to the current session, and the bot will stream back OpenCode's response.

#### 6. Streaming Response Processing
OpenCode's `/session/{id}/message` endpoint supports Server-Sent Events (SSE) streaming responses. The bot needs to:
- Subscribe to SSE stream after sending user message.
- Forward each received event to Telegram in real-time (as multiple messages or editing the same message).
- Support interruption: abort current streaming request when user sends `/abort` or a new message.

#### 7. Interruption Mechanism
- When user sends `/abort` command, call `POST /session/{id}/abort` to abort current execution.
- When user sends a new message, automatically abort current session's streaming response (if still in progress).

#### 8. Status Viewing
- Users can view the latest message of current session (including intermediate outputs) via `/status`.
- Bot can periodically cache the latest messages of sessions for quick querying.

### Deployment and Configuration

#### Configuration File
The bot uses TOML format configuration file, mainly including the following sections:
- `telegram`: Bot Token, polling parameters
- `proxy`: HTTP proxy settings
- `opencode`: OpenCode server address and timeout
- `storage`: Session storage configuration (memory or SQLite)
- `logging`: Logging configuration

The default configuration file path is `config.toml`, can also be specified via environment variable `CONFIG_PATH`.

#### Environment Variables (Optional)
```bash
CONFIG_PATH=/path/to/config.toml  # Specify configuration file path
```

#### Dependencies
```txt
# go.mod
module tg-bot

go 1.21

require (
    github.com/pelletier/go-toml/v2 v2.2.0  # TOML configuration parsing
    gopkg.in/telebot.v4 v4.0.0-beta.5      # Telegram Bot library
    github.com/valyala/fasthttp v1.52.0    # HTTP client (proxy support)
)
```

#### Compilation and Startup
```bash
# Compile
go build -o tg-bot cmd/bot/main.go

# Run (using default configuration file)
./tg-bot

# Run (specify configuration file)
CONFIG_PATH=/path/to/config.toml ./tg-bot
```

### Security Considerations
- Keep Telegram Bot Token confidential.
- OpenCode internal network access requires no authentication (assuming internal network security).
- Can add user whitelist to restrict access.

## Alternative Designs Considered

### 1. Webhook Method
- **Advantages**: Higher real-time performance, Telegram pushes messages.
- **Disadvantages**: Requires publicly accessible HTTPS endpoint, which internal network environments cannot provide. Requires additional internal network penetration tools (ngrok/frp), increasing complexity.

### 2. Direct CLI Wrapper
- **Advantages**: Directly calls OpenCode CLI commands, no API encapsulation needed.
- **Disadvantages**: OpenCode may not provide complete CLI; interactive output processing is more complex; difficult to implement session management and status viewing.

### 3. Using Other Messaging Platforms (e.g., Slack, Discord)
- **Advantages**: Some platforms offer better bot development support.
- **Disadvantages**: User preference for Telegram; platform access may also require proxy.

### 4. Independent Web Interface
- **Advantages**: Richer interactive experience.
- **Disadvantages**: Higher development cost; less convenient on mobile than Telegram.

The current design chooses Telegram Bot + polling + OpenCode API, balancing development cost, user experience, and environmental constraints.

## Unresolved Questions

1. **OpenCode API streaming response format**: Need to further confirm the specific format of SSE events to correctly parse intermediate outputs.
2. **Session timeout and cleanup**: OpenCode session lifetime strategy, whether the bot needs to automatically clean up idle sessions.
3. **Large file/large output handling**: Telegram messages have length limits (4096 characters), need to handle long output fragmentation or truncation.
4. **Multi-user concurrency**: Memory-stored session mapping is lost on restart, whether persistence is needed.
5. **OpenCode permission control**: Some operations may require permission approval (e.g., file writing), how the bot handles permission requests.
6. **Performance considerations**: Polling interval settings to avoid excessive requests to Telegram API.

## Appendix: OpenCode API Feature Mapping Table

| OpenCode Feature | Corresponding Bot Command/Capability |
|------------------|--------------------------------------|
| Session Management | `/sessions`, `/new`, `/switch`, `/current`, `/abort` |
| Send Message | Regular text messages (non-commands) |
| Get Message History | `/status` |
| File Browsing | `/files [path]` |
| Text Search | `/search <pattern>` |
| File Search | `/findfile <pattern>` |
| Symbol Search | `/symbol <symbol>` |
| Agent List | `/agent` |
| Command List | `/command` |
| Terminal Operations | Not supported yet (requires PTY session, complex interaction) |
| Project Information | Extendable `/project` |
| Health Check | Extendable `/health` |
| Event Subscription | Can be used for real-time notifications (requires extension) |

**Note**: Some advanced features (such as terminal operations, work tree management) are not implemented due to interaction complexity and can be extended based on requirements.