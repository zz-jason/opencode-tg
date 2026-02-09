# Makefile for Telegram Bot for OpenCode

.PHONY: build test lint fmt-check vet staticcheck govulncheck clean run check-opencode deps run-with-config release help

# Build the bot
build:
	go build -o opencode-tg ./cmd/bot

# Run tests
test:
	go test ./...

# Run lint and static analysis checks
lint: fmt-check vet staticcheck govulncheck

# Check gofmt formatting
fmt-check:
	@unformatted="$$(find . -name '*.go' -not -path './vendor/*' -exec gofmt -l {} +)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following Go files are not formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

# Run go vet
vet:
	go vet ./...

# Run staticcheck
staticcheck:
	@staticcheck_bin="$$(command -v staticcheck || true)"; \
	if [ -z "$$staticcheck_bin" ]; then \
		echo "staticcheck not found; installing..."; \
		go install honnef.co/go/tools/cmd/staticcheck@latest; \
		staticcheck_bin="$$(go env GOPATH)/bin/staticcheck"; \
	fi; \
	"$$staticcheck_bin" ./...

# Run vulnerability scan
govulncheck:
	@govulncheck_bin="$$(command -v govulncheck || true)"; \
	if [ -z "$$govulncheck_bin" ]; then \
		echo "govulncheck not found; installing..."; \
		go install golang.org/x/vuln/cmd/govulncheck@latest; \
		govulncheck_bin="$$(go env GOPATH)/bin/govulncheck"; \
	fi; \
	"$$govulncheck_bin" ./...

# Clean build artifacts
clean:
	rm -f opencode-tg
	rm -f opencode-tg-linux-amd64
	rm -f opencode-tg-linux-arm64
	rm -f opencode-tg-darwin-amd64
	rm -f opencode-tg-darwin-arm64
	rm -f bot.log
	rm -f sessions.json
	rm -f bot-state.json
	rm -rf release
	rm -f *.tar.gz

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
	GOOS=linux GOARCH=amd64 go build -o opencode-tg-linux-amd64 ./cmd/bot
	GOOS=linux GOARCH=arm64 go build -o opencode-tg-linux-arm64 ./cmd/bot
	GOOS=darwin GOARCH=amd64 go build -o opencode-tg-darwin-amd64 ./cmd/bot
	GOOS=darwin GOARCH=arm64 go build -o opencode-tg-darwin-arm64 ./cmd/bot

# Create release packages
release-packages: release
	@echo "Creating release packages..."
	
	# Linux amd64 package
	mkdir -p release/linux-amd64
	cp opencode-tg-linux-amd64 release/linux-amd64/opencode-tg
	cp config.example.toml release/linux-amd64/config.toml
	cp README.md release/linux-amd64/
	tar -czf opencode-tg-linux-amd64.tar.gz -C release/linux-amd64 .
	
	# Linux arm64 package
	mkdir -p release/linux-arm64
	cp opencode-tg-linux-arm64 release/linux-arm64/opencode-tg
	cp config.example.toml release/linux-arm64/config.toml
	cp README.md release/linux-arm64/
	tar -czf opencode-tg-linux-arm64.tar.gz -C release/linux-arm64 .
	
	# Darwin amd64 package
	mkdir -p release/darwin-amd64
	cp opencode-tg-darwin-amd64 release/darwin-amd64/opencode-tg
	cp config.example.toml release/darwin-amd64/config.toml
	cp README.md release/darwin-amd64/
	tar -czf opencode-tg-darwin-amd64.tar.gz -C release/darwin-amd64 .
	
	# Darwin arm64 package
	mkdir -p release/darwin-arm64
	cp opencode-tg-darwin-arm64 release/darwin-arm64/opencode-tg
	cp config.example.toml release/darwin-arm64/config.toml
	cp README.md release/darwin-arm64/
	tar -czf opencode-tg-darwin-arm64.tar.gz -C release/darwin-arm64 .
	
	# Source code package
	tar --exclude='.git' --exclude='release' --exclude='opencode-tg-*' --exclude='*.tar.gz' -czf opencode-tg-src.tar.gz .
	
	@echo "Release packages created:"
	@ls -la *.tar.gz

# Help
help:
	@echo "Available targets:"
	@echo "  build          - Build the bot"
	@echo "  test           - Run all tests"
	@echo "  lint           - Run formatting, vet, staticcheck, and vulnerability checks"
	@echo "  clean          - Remove build artifacts"
	@echo "  run            - Build and run the bot"
	@echo "  check-opencode - Check OpenCode server connectivity"
	@echo "  deps           - Install dependencies"
	@echo "  release        - Build for multiple platforms"
	@echo "  help           - Show this help"
