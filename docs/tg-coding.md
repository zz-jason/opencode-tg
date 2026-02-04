# Design Template

- Co-Authored-By: opencode(@opencode), deepseek-reasoner
- Date: 2026-02-04

## Summary

设计一个 Telegram Bot，用于与内网部署的 OpenCode AI 编程助手进行交互。Bot 运行在内网环境，通过 HTTP 代理访问 Telegram API，采用轮询方式获取消息。Bot 集成了 OpenCode 的 OpenAPI，支持用户查看任务状态、发起编程任务、实时交互等功能，提供类似 CLI 的使用体验。

## Background

### 问题背景
- OpenCode 是一个 AI 编程助手，部署在内网服务器 (192.168.50.100:8080)，提供了丰富的 OpenAPI 接口。
- 用户希望通过 Telegram 便捷地与 OpenCode 交互，无需直接访问 CLI 或 Web 界面。
- 内网环境无法被外网访问，因此 Bot 必须使用轮询 (polling) 方式获取消息。
- 内网访问外网需要 HTTP 代理（用于访问 Telegram、Google 等境外服务）。

### 使用场景
1. **基本场景**：用户通过 Telegram Bot 查看 OpenCode 当前任务的执行状态和中间输出。
2. **高级场景**：用户通过 Bot 发起新的编程任务（如代码生成、重构、测试），并与 OpenCode 进行多轮交互，可随时打断、提供额外信息。
3. **辅助场景**：用户通过 Bot 进行文件浏览、代码搜索、会话管理等操作。

### 功能需求
- **基本需求**：
  - 支持通过代理连接 Telegram API
  - 周期性拉取消息（polling）
  - 查看 OpenCode 会话状态和消息历史
- **高级需求**：
  - 创建/选择会话
  - 发送指令到 OpenCode 并接收流式响应
  - 支持交互式对话（可中断、可追加信息）
  - 支持 OpenCode 常用操作（文件操作、搜索、终端执行等）

## Detailed Design

### 系统架构

```
+-------------+      +-------------------+      +-------------------+
| Telegram    |      | Telegram Bot      |      | OpenCode Server   |
| API         |<---->| (Python)          |<---->| (192.168.50.100:8080)|
+-------------+      +-------------------+      +-------------------+
                         ^       ^
                         |       |
                    HTTP Proxy  内网 HTTP
                    (访问外网)    (无需代理)
```

**组件说明**：
1. **Telegram Bot**：Golang 程序，使用 `telebot` 库，通过 HTTP 代理连接 Telegram API，支持轮询模式。
2. **配置管理器**：读取和管理 TOML 格式的配置文件，包含 Bot Token、代理设置、OpenCode URL 等。
3. **OpenCode Client**：封装 OpenCode OpenAPI 的 HTTP 客户端，支持会话管理、消息发送、事件订阅等。
4. **会话管理器**：管理用户与会话的映射关系（内存或 SQLite 存储）。
5. **命令处理器**：解析用户命令，调用相应的 OpenCode API。
6. **消息流处理器**：处理 OpenCode 的流式响应，实时推送至 Telegram。

### 数据流
1. **用户发送命令**：用户向 Telegram Bot 发送消息（文本或命令）。
2. **Bot 接收消息**：Bot 通过轮询获取消息，经过代理转发。
3. **命令解析**：Bot 解析命令，确定目标会话和操作类型。
4. **调用 OpenCode API**：Bot 通过内网 HTTP 请求调用 OpenCode API。
5. **处理响应**：Bot 接收 OpenCode 的响应（立即响应或流式输出）。
6. **返回结果**：Bot 将结果格式化后发送回 Telegram 用户。

### 关键功能实现

#### 1. 配置管理（TOML）
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
type = "memory"  # 或 "sqlite"
sqlite_path = "sessions.db"

[logging]
level = "info"
output = "bot.log"
```

#### 2. Golang 代理配置与轮询
```go
package main

import (
    "gopkg.in/telebot.v4"
    "net/http"
    "net/url"
    "time"
)

func main() {
    // 读取配置
    cfg := config.Load("config.toml")
    
    // 设置 HTTP 代理
    proxyURL, _ := url.Parse(cfg.Proxy.URL)
    transport := &http.Transport{
        Proxy: http.ProxyURL(proxyURL),
    }
    httpClient := &http.Client{
        Transport: transport,
        Timeout:   time.Second * 30,
    }
    
    // 创建 Bot
    pref := telebot.Settings{
        Token:  cfg.Telegram.Token,
        Poller: &telebot.LongPoller{Timeout: cfg.Telegram.PollingTimeout},
        Client: httpClient,
    }
    
    bot, err := telebot.NewBot(pref)
    if err != nil {
        panic(err)
    }
    
    // 注册命令处理器
    bot.Handle("/start", startHandler)
    bot.Handle("/help", helpHandler)
    // ... 其他命令
    
    // 启动轮询
    bot.Start()
}
```

#### 3. OpenCode 客户端
封装 OpenCode API 的主要端点：
- `POST /session` - 创建会话
- `POST /session/{id}/message` - 发送消息（支持流式）
- `GET /session` - 获取会话列表
- `GET /session/{id}/message` - 获取消息历史
- `POST /session/{id}/abort` - 中止会话
- `GET /file`, `GET /find` 等辅助 API

#### 4. 会话管理策略
- **默认策略**：每个 Telegram 用户对应一个 OpenCode 会话（简化管理）。
- **多会话支持**：用户可通过命令创建新会话、切换会话。
- **会话状态**：存储在内存字典中，可扩展为 SQLite 持久化。

#### 5. 命令设计

| 命令 | 参数 | 说明 |
|------|------|------|
| `/start` | - | 启动 Bot，显示帮助信息 |
| `/help` | - | 显示命令帮助 |
| `/sessions` | - | 列出当前用户的所有会话 |
| `/new` | [名称] | 创建新会话 |
| `/switch` | 会话ID | 切换当前会话 |
| `/current` | - | 显示当前会话信息 |
| `/abort` | - | 中止当前会话的任务 |
| `/files` | [路径] | 浏览项目文件 |
| `/search` | 模式 | 搜索代码文本 |
| `/findfile` | 模式 | 搜索文件 |
| `/symbol` | 符号 | 搜索符号 |
| `/agent` | - | 列出可用 AI 代理 |
| `/command` | - | 列出可用命令 |
| `/status` | - | 显示当前任务状态（最新消息） |

**非命令消息处理**：用户发送的普通文本将作为指令发送到当前会话，Bot 将流式返回 OpenCode 的响应。

#### 6. 流式响应处理
OpenCode 的 `/session/{id}/message` 端点支持 Server-Sent Events (SSE) 流式响应。Bot 需要：
- 发送用户消息后，订阅 SSE 流。
- 将接收到的每个事件实时转发到 Telegram（作为多条消息或编辑同一消息）。
- 支持中断：用户发送 `/abort` 或新消息时，中止当前流式请求。

#### 7. 中断机制
- 用户发送 `/abort` 命令时，调用 `POST /session/{id}/abort` 中止当前执行。
- 用户发送新消息时，自动中止当前会话的流式响应（如果仍在进行）。

#### 8. 状态查看
- 用户可通过 `/status` 查看当前会话的最新消息（包括中间输出）。
- Bot 可定期缓存会话的最新消息，便于快速查询。

### 部署与配置

#### 配置文件
Bot 使用 TOML 格式的配置文件，主要包含以下部分：
- `telegram`：Bot Token、轮询参数
- `proxy`：HTTP 代理设置
- `opencode`：OpenCode 服务器地址和超时
- `storage`：会话存储配置（内存或 SQLite）
- `logging`：日志配置

配置文件默认路径为 `config.toml`，也可通过环境变量 `CONFIG_PATH` 指定。

#### 环境变量（可选）
```bash
CONFIG_PATH=/path/to/config.toml  # 指定配置文件路径
```

#### 依赖
```txt
# go.mod
module tg-bot

go 1.21

require (
    github.com/pelletier/go-toml/v2 v2.2.0  # TOML 配置解析
    gopkg.in/telebot.v4 v4.0.0-beta.5      # Telegram Bot 库
    github.com/valyala/fasthttp v1.52.0    # HTTP 客户端（代理支持）
)
```

#### 编译与启动
```bash
# 编译
go build -o tg-bot cmd/bot/main.go

# 运行（使用默认配置文件）
./tg-bot

# 运行（指定配置文件）
CONFIG_PATH=/path/to/config.toml ./tg-bot
```

### 安全考虑
- Telegram Bot Token 保密。
- OpenCode 内网访问无需认证（假设内网安全）。
- 可添加用户白名单限制访问。

## Alternative Designs Considered

### 1. Webhook 方式
- **优点**：实时性更高，Telegram 推送消息。
- **缺点**：需要公网可访问的 HTTPS 端点，内网环境无法满足。需要额外的内网穿透工具（ngrok/frp），增加复杂度。

### 2. 直接 CLI 包装
- **优点**：直接调用 OpenCode CLI 命令，无需 API 封装。
- **缺点**：OpenCode 可能未提供完整 CLI；交互式输出处理更复杂；难以实现会话管理和状态查看。

### 3. 使用其他消息平台（如 Slack、Discord）
- **优点**：某些平台提供更好的 Bot 开发支持。
- **缺点**：用户偏好 Telegram；平台访问可能同样需要代理。

### 4. 独立 Web 界面
- **优点**：更丰富的交互体验。
- **缺点**：开发成本高；移动端便捷性不如 Telegram。

当前设计选择 Telegram Bot + 轮询 + OpenCode API，平衡了开发成本、用户体验和环境限制。

## Unresolved Questions

1. **OpenCode API 的流式响应格式**：需要进一步确认 SSE 事件的具体格式，以正确解析中间输出。
2. **会话超时与清理**：OpenCode 会话的生存期策略，Bot 是否需要自动清理闲置会话。
3. **大文件/大输出处理**：Telegram 消息有长度限制（4096字符），需要处理长输出的分片或截断。
4. **多用户并发**：内存存储的会话映射在重启后丢失，是否需要持久化。
5. **OpenCode 权限控制**：某些操作可能需要权限批准（如文件写入），Bot 如何处理权限请求。
6. **性能考虑**：轮询间隔设置，避免过频请求 Telegram API。

## 附录：OpenCode API 功能映射表

| OpenCode 功能         | 对应 Bot 命令/能力                                                                 |
|-----------------------|------------------------------------------------------------------------------------|
| 会话管理              | `/sessions`, `/new`, `/switch`, `/current`, `/abort`                               |
| 发送消息              | 普通文本消息（非命令）                                                             |
| 获取消息历史          | `/status`                                                                          |
| 文件浏览              | `/files [路径]`                                                                    |
| 文本搜索              | `/search <模式>`                                                                   |
| 文件搜索              | `/findfile <模式>`                                                                 |
| 符号搜索              | `/symbol <符号>`                                                                   |
| 代理列表              | `/agent`                                                                           |
| 命令列表              | `/command`                                                                         |
| 终端操作              | 暂不支持（需 PTY 会话，交互复杂）                                                  |
| 项目信息              | 可扩展 `/project`                                                                  |
| 健康检查              | 可扩展 `/health`                                                                   |
| 事件订阅              | 可用于实时通知（需扩展）                                                           |

**注意**：部分高级功能（如终端操作、工作树管理）因交互复杂暂不实现，后续可根据需求扩展。