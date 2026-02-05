#!/bin/bash
#
# Local commit message check script
# Validates commit messages follow Conventional Commits format
# Usage: 
#   ./ci-local-check.sh           # Check all commits in current branch
#   ./ci-local-check.sh HEAD~3..  # Check commits in range
#   ./ci-local-check.sh --hook    # Use as git commit-msg hook

set -e

# Default to checking all commits in current branch
COMMIT_RANGE=${1:-"$(git merge-base origin/main HEAD)..HEAD"}

if [ "$1" = "--hook" ]; then
  # Git commit-msg hook mode
  COMMIT_MSG_FILE="$2"
  SUBJECT=$(head -n1 "$COMMIT_MSG_FILE")
  
  echo "🔍 Checking commit message: $SUBJECT"
  
  if echo "$SUBJECT" | grep -qE '^(feat|fix|docs|style|refactor|test|chore|perf|build|ci|revert)(\([a-z0-9-]+\))?: .+'; then
    echo "✅ Valid commit message format"
    exit 0
  else
    echo "❌ Invalid commit message format"
    echo "Commit messages must follow Conventional Commits format: <type>(<scope>): <description>"
    echo "Allowed types: feat, fix, docs, style, refactor, test, chore, perf, build, ci, revert"
    echo ""
    echo "Examples:"
    echo "  feat: add new feature"
    echo "  fix(api): resolve authentication issue"
    echo "  docs: update README"
    exit 1
  fi
fi

echo "🔍 Checking commit messages in range: $COMMIT_RANGE"
echo ""

ERRORS=0
git log --format="%H %s" --no-merges $COMMIT_RANGE | while read hash subject; do
  if [ -z "$subject" ]; then
    continue
  fi
  
  if echo "$subject" | grep -qE '^(feat|fix|docs|style|refactor|test|chore|perf|build|ci|revert)(\([a-z0-9-]+\))?: .+'; then
    echo "✅ $hash: $subject"
  else
    echo "❌ $hash: $subject"
    ERRORS=$((ERRORS + 1))
  fi
done

echo ""
if [ $ERRORS -eq 0 ]; then
  echo "🎉 All commit messages are valid!"
  exit 0
else
  echo "💥 Found $ERRORS invalid commit message(s)"
  echo ""
  echo "Commit messages must follow Conventional Commits format: <type>(<scope>): <description>"
  echo "Allowed types: feat, fix, docs, style, refactor, test, chore, perf, build, ci, revert"
  exit 1
fi