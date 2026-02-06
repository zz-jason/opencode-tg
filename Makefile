# Makefile for Telegram Bot for OpenCode

.PHONY: build test test-integration clean run check-opencode deps run-with-config release help

# Build the bot
build:
	go build -o tg-bot ./cmd/bot

# Run tests
test:
	go test ./...

# Run integration tests (requires network access for OpenCode install if OPENCODE_BIN is not set)
test-integration:
	go test -tags=integration ./internal/handler -run TestIntegration_HandleCoreCommands -count=1 -v

# Clean build artifacts
clean:
	rm -f tg-bot
	rm -f tg-bot-linux-amd64
	rm -f tg-bot-darwin-amd64
	rm -f bot.log
	rm -f sessions.json
	rm -f bot-state.json

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
run-with-config: build
	./tg-bot --config $(config)

# Build for production
release: test
	GOOS=linux GOARCH=amd64 go build -o tg-bot-linux-amd64 ./cmd/bot
	GOOS=darwin GOARCH=amd64 go build -o tg-bot-darwin-amd64 ./cmd/bot

# Help
help:
	@echo "Available targets:"
	@echo "  build          - Build the bot"
	@echo "  test           - Run all tests"
	@echo "  test-integration - Run integration test suite"
	@echo "  clean          - Remove build artifacts"
	@echo "  run            - Build and run the bot"
	@echo "  check-opencode - Check OpenCode server connectivity"
	@echo "  deps           - Install dependencies"
	@echo "  release        - Build for multiple platforms"
	@echo "  help           - Show this help"
