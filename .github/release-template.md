# OpenCode Telegram Bot v{{VERSION}}

## Download Packages

Each package contains everything you need: binary executable, configuration template, and documentation.

### Linux
- **x86_64**: [opencode-tg-linux-amd64.tar.gz](https://github.com/anomalyco/opencode-tg/releases/download/v{{VERSION}}/opencode-tg-linux-amd64.tar.gz)
- **ARM64**: [opencode-tg-linux-arm64.tar.gz](https://github.com/anomalyco/opencode-tg/releases/download/v{{VERSION}}/opencode-tg-linux-arm64.tar.gz)

### macOS
- **Intel**: [opencode-tg-darwin-amd64.tar.gz](https://github.com/anomalyco/opencode-tg/releases/download/v{{VERSION}}/opencode-tg-darwin-amd64.tar.gz)
- **Apple Silicon**: [opencode-tg-darwin-arm64.tar.gz](https://github.com/anomalyco/opencode-tg/releases/download/v{{VERSION}}/opencode-tg-darwin-arm64.tar.gz)

### Source Code
- **Complete source**: [opencode-tg-src.tar.gz](https://github.com/anomalyco/opencode-tg/releases/download/v{{VERSION}}/opencode-tg-src.tar.gz)

### Verify Integrity
After downloading, verify file integrity:
```bash
sha256sum -c checksums.txt
```

## Quick Start

### 1. Download and Extract
```bash
# Download the package for your platform
wget https://github.com/anomalyco/opencode-tg/releases/download/v{{VERSION}}/opencode-tg-linux-amd64.tar.gz

# Extract
tar -xzf opencode-tg-linux-amd64.tar.gz
cd opencode-tg-linux-amd64
```

### 2. Configure
```bash
# Edit the included configuration file
vim config.toml
```

Set your Telegram Bot Token and OpenCode server address:
```toml
[telegram]
token = "YOUR_BOT_TOKEN"

[opencode]
server_url = "http://localhost:8080"
```

### 3. Run
```bash
# Make executable
chmod +x opencode-tg

# Start the bot
./opencode-tg
```

## Configuration

Detailed configuration options are available in [config.example.toml](https://github.com/anomalyco/opencode-tg/blob/main/config.example.toml).

## Changelog

{{CHANGELOG}}

## Security Notes

- Configuration file `config.toml` contains sensitive information - do not commit to version control
- Session state is saved in `bot-state.json`
- Logs are written to `bot.log` by default

## License

MIT License