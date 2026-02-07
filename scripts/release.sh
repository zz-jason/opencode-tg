#!/bin/bash
set -e

# Release script for OpenCode Telegram Bot
# Usage: ./scripts/release.sh <version>
# Example: ./scripts/release.sh v1.0.0

if [ $# -ne 1 ]; then
    echo "Usage: $0 <version>"
    echo "Example: $0 v1.0.0"
    exit 1
fi

VERSION=$1

# Validate version format
if [[ ! $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Version format should be vX.Y.Z"
    exit 1
fi

echo "Preparing release: $VERSION"

# 1. Check for uncommitted changes
if [[ -n $(git status --porcelain) ]]; then
    echo "Error: There are uncommitted changes, please commit or stash first"
    git status
    exit 1
fi

# 2. Run tests
echo "Running tests..."
make test

# 3. Create release packages (local test)
echo "Creating release packages..."
make release-packages

# 4. Show created packages
echo "Created packages:"
ls -la opencode-tg-*.tar.gz

# 5. Create tag
echo "Creating tag $VERSION..."
git tag -a "$VERSION" -m "Release $VERSION"

# 6. Push tag
echo "Pushing tag to remote..."
git push origin "$VERSION"

# 7. Show release instructions
echo ""
echo "Release process started!"
echo ""
echo "Next steps:"
echo "1. Wait for GitHub Actions to complete build and release"
echo "2. Check release page: https://github.com/anomalyco/opencode-tg/releases"
echo "3. Edit release notes if needed"
echo ""
echo "Each package contains:"
echo "  - opencode-tg (binary)"
echo "  - config.toml (configuration template)"
echo "  - README.md (documentation)"
echo ""
echo "Total 5 packages:"
echo "  - opencode-tg-linux-amd64.tar.gz"
echo "  - opencode-tg-linux-arm64.tar.gz"
echo "  - opencode-tg-darwin-amd64.tar.gz"
echo "  - opencode-tg-darwin-arm64.tar.gz"
echo "  - opencode-tg-src.tar.gz"