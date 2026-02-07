# Release Guide

This document explains how to release OpenCode Telegram Bot binaries on GitHub.

## Release Process

### 1. Preparation

Ensure:
- All changes are committed to main branch
- Tests pass: `make test`
- Code is formatted: `go fmt ./...`
- Static analysis passes: `go vet ./...`

### 2. Create Release

Use the release script:

```bash
# Create and push tag
./scripts/release.sh v1.0.0
```

Or manually:

```bash
# 1. Create tag
git tag -a v1.0.0 -m "Release v1.0.0"

# 2. Push tag
git push origin v1.0.0
```

### 3. Automated Release Flow

After pushing the tag, GitHub Actions will automatically:
1. Run all tests
2. Build binaries for:
   - Linux (x86_64, ARM64)
   - macOS (Intel, Apple Silicon)
3. Generate checksum file
4. Create GitHub Release with all files

### 4. Verify Release

After release completes, check:
1. [GitHub Releases page](https://github.com/anomalyco/opencode-tg/releases)
2. All platform packages are uploaded
3. Checksum file is correct

## Package Contents

Each release includes these files:

### Platform Packages
- `opencode-tg-linux-amd64.tar.gz` - Linux x86_64 (binary + config + docs)
- `opencode-tg-linux-arm64.tar.gz` - Linux ARM64 (binary + config + docs)
- `opencode-tg-darwin-amd64.tar.gz` - macOS Intel (binary + config + docs)
- `opencode-tg-darwin-arm64.tar.gz` - macOS Apple Silicon (binary + config + docs)
- `opencode-tg-src.tar.gz` - Complete source code

### Verification Files
- `checksums.txt` - SHA256 checksums

## User Instructions

### Download and Run

1. Download the appropriate package from [Releases page](https://github.com/anomalyco/opencode-tg/releases)
2. Verify file integrity:
   ```bash
   sha256sum -c checksums.txt
   ```
3. Extract the package:
   ```bash
   tar -xzf opencode-tg-linux-amd64.tar.gz
   cd opencode-tg-linux-amd64
   ```
4. Edit configuration (included in package):
   ```bash
   vim config.toml
   ```
5. Make executable and run:
   ```bash
   chmod +x opencode-tg
   ./opencode-tg
   ```

## Versioning

Follow [Semantic Versioning](https://semver.org/):
- `v1.0.0` - Stable release
- `v1.0.0-rc.1` - Release candidate
- `v1.0.0-beta.1` - Beta release
- `v1.0.0-alpha.1` - Alpha release

## Troubleshooting

### Release Fails
1. Check GitHub Actions logs
2. Ensure sufficient repository permissions
3. Verify tag format is correct

### Build Fails
1. Check Go version compatibility
2. Verify dependencies: `go mod tidy`
3. Check cross-compilation settings

### Missing Files
1. Check Makefile release target
2. Verify GitHub Actions workflow configuration
3. Check file upload steps