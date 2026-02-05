# Makefile for Telegram Bot for OpenCode

.PHONY: build test clean run check-opencode

# Build the bot
build:
	go build -o tg-bot cmd/bot/main.go

# Run tests
test:
	go test ./...

# Clean build artifacts
clean:
	rm -f tg-bot
	rm -f bot.log
	rm -f sessions.db

# Run the bot
run: build
	./tg-bot

# Check OpenCode connection
check-opencode:
	@echo "Checking OpenCode connection..."
	@if curl -s --max-time 10 "http://192.168.50.100:8080/global/health" > /dev/null; then \
		echo "✅ OpenCode is reachable"; \
	else \
		echo "❌ OpenCode is not reachable"; \
		exit 1; \
	fi

# Install dependencies
deps:
	go mod download
	go mod tidy

# Run with specific config file
run-with-config:
	./tg-bot --config $(config)

# Build for production
release: test
	GOOS=linux GOARCH=amd64 go build -o tg-bot-linux-amd64 cmd/bot/main.go
	GOOS=darwin GOARCH=amd64 go build -o tg-bot-darwin-amd64 cmd/bot/main.go

# Help
help:
	@echo "Available targets:"
	@echo "  build          - Build the bot"
	@echo "  test           - Run all tests"
	@echo "  clean          - Remove build artifacts"
	@echo "  run            - Build and run the bot"
	@echo "  check-opencode - Check OpenCode server connectivity"
	@echo "  deps           - Install dependencies"
	@echo "  release        - Build for multiple platforms"
	@echo "  help           - Show this help"