# Telegram Bot for OpenCode

一个 Telegram 机器人，用于与内网部署的 OpenCode AI 编程助手进行交互。Bot 运行在内网环境，通过 HTTP 代理访问 Telegram API，采用轮询方式获取消息。

## 功能特性

- ✅ 通过 Telegram Bot 与 OpenCode 交互
- ✅ 支持 HTTP 代理（用于访问境外服务）
- ✅ 轮询模式（无需公网 IP）
- ✅ 会话管理（每个用户独立会话）
- ✅ 查看任务状态和中间输出
- ✅ 发起编程任务并流式接收响应
- ✅ 支持中断正在执行的任务
- ✅ 文件浏览、代码搜索等辅助功能（待实现）

## 系统架构

```
Telegram API <--[HTTP Proxy]--> Telegram Bot (Golang) <--[内网HTTP]--> OpenCode Server
```

## 快速开始

### 前提条件

1. OpenCode 服务器运行在 `http://192.168.50.100:8080`
2. HTTP 代理可访问 Telegram API（如 `http://127.0.0.1:7890`）
3. Telegram Bot Token（从 @BotFather 获取）
4. Go 1.21+ 开发环境

### 配置

复制 `config.example.toml` 为 `config.toml` 并修改配置：

```toml
[telegram]
token = "YOUR_BOT_TOKEN"
polling_timeout = 60
polling_limit = 100

[proxy]
enabled = true
url = "http://127.0.0.1:7890"

[opencode]
url = "http://192.168.50.100:8080"
timeout = 30

[storage]
type = "memory"

[logging]
level = "info"
output = "bot.log"
```

### 构建和运行

```bash
# 安装依赖
make deps

# 构建
make build

# 检查 OpenCode 连接
make check-opencode

# 运行
make run
```

或者直接使用：

```bash
go run cmd/bot/main.go
```

## 使用指南

### 基本命令

- `/start` - 显示欢迎信息
- `/help` - 显示帮助
- `/sessions` - 列出所有会话
- `/new [名称]` - 创建新会话
- `/switch <会话ID>` - 切换当前会话
- `/current` - 显示当前会话信息
- `/abort` - 中止当前任务
- `/status` - 查看当前任务状态

### 交互模式

发送任何非命令文本，Bot 会将其作为指令发送给 OpenCode 并流式返回响应。

示例：
```
用户: 写一个Go函数计算斐波那契数列
Bot: 🤖 处理中...
Bot: 这是一个计算斐波那契数列的Go函数...
```

### 会话管理

- 每个 Telegram 用户默认有一个会话
- 使用 `/new` 可以创建多个会话用于不同任务
- 使用 `/switch` 可以在会话间切换
- 会话状态保存在内存中（重启后丢失）

## 开发

### 项目结构

```
tg-bot/
├── cmd/bot/main.go          # 程序入口
├── internal/
│   ├── config/              # 配置管理（TOML）
│   ├── handler/             # Telegram 命令处理器
│   ├── opencode/            # OpenCode API 客户端
│   ├── session/             # 会话管理器
│   ├── stream/              # SSE 流式处理
│   └── logging/             # 日志配置
├── config.toml              # 配置文件
└── docs/tg-coding.md        # 设计文档
```

### 测试

```bash
# 运行所有测试
make test

# 运行特定包测试
go test ./internal/config
go test ./internal/opencode
go test ./internal/session
```

### 添加新命令

1. 在 `internal/handler/handlers.go` 中注册命令：
   ```go
   b.tgBot.Handle("/newcommand", b.handleNewCommand)
   ```

2. 实现处理函数：
   ```go
   func (b *Bot) handleNewCommand(c telebot.Context) error {
       // 处理逻辑
       return c.Send("响应")
   }
   ```

## 配置说明

### Telegram 配置
- `token`: Telegram Bot Token（必需）
- `polling_timeout`: 轮询超时时间（秒）
- `polling_limit`: 每次轮询获取的消息数量

### 代理配置
- `enabled`: 是否启用代理
- `url`: 代理服务器地址

### OpenCode 配置
- `url`: OpenCode 服务器地址（必需）
- `timeout`: API 请求超时时间（秒）

### 存储配置
- `type`: 存储类型（`memory` 或 `sqlite`）
- `sqlite_path`: SQLite 数据库路径（当 type=sqlite 时）

### 日志配置
- `level`: 日志级别（debug, info, warn, error）
- `output`: 日志输出文件（stdout 或文件路径）

## 故障排除

### OpenCode 连接失败
```
ERROR: OpenCode health check failed
```
- 检查 OpenCode 服务器是否运行
- 检查网络连通性
- 验证 `opencode.url` 配置

### Telegram 连接失败
```
ERROR: Failed to create Telegram bot
```
- 检查 Bot Token 是否正确
- 检查代理配置是否正确
- 验证代理服务器可访问 Telegram API

### 流式响应中断
- 检查 OpenCode 的 SSE 端点是否正常工作
- 查看日志中的错误信息

## 许可证

MIT License

## 贡献

欢迎提交 Issue 和 Pull Request。