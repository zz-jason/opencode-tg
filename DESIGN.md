# Design Template

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
| API         |<---->| (Python)          |<---->| (192.168.50.100:8080)|
+-------------+      +-------------------+      +-------------------+
                         ^       ^
                         |       |
                    HTTP Proxy  Internal HTTP
                    (External)   (No proxy needed)
```

**Component Description**:
1. **Telegram Bot**: Python program using `python-telegram-bot` library, configured with HTTP proxy to connect to Telegram API.
2. **OpenCode Client**: HTTP client encapsulating OpenCode OpenAPI, supporting session management, message sending, event subscription, etc.
3. **Session Manager**: Manages user-to-session mapping (memory or SQLite storage).
4. **Command Handler**: Parses user commands and calls corresponding OpenCode APIs.
5. **Message Stream Handler**: Processes OpenCode's streaming responses and pushes them to Telegram in real-time.

### Data Flow
1. **User sends command**: User sends a message (text or command) to Telegram Bot.
2. **Bot receives message**: Bot retrieves messages via polling, forwarded through proxy.
3. **Command parsing**: Bot parses the command to determine target session and operation type.
4. **Call OpenCode API**: Bot calls OpenCode API via internal HTTP request.
5. **Process response**: Bot receives OpenCode response (immediate or streaming output).
6. **Return result**: Bot formats the result and sends it back to Telegram user.

### Key Feature Implementation

#### 1. Proxy Configuration
```python
import asyncio
from telegram.ext import Application
from telegram.request import HTTPXRequest

request = HTTPXRequest(proxy_url="http://proxy:port")
application = Application.builder().token("TOKEN").request(request).build()
```

#### 2. Polling Settings
```python
application.run_polling(allowed_updates=Update.ALL_TYPES)
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

#### Environment Variables
```bash
TELEGRAM_BOT_TOKEN=xxx
HTTP_PROXY=http://proxy:port
OPENCODE_URL=http://192.168.50.100:8080
STORAGE_TYPE=memory  # or sqlite
```

#### Dependencies
```txt
python-telegram-bot==21.7
httpx
sqlite3 (optional)
```

#### Startup Method
```bash
python bot.py
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