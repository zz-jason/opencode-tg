# AGENTS.md

> Project-level guide for AI coding agents. Since agents are "stateless" between sessions, this document helps them quickly understand the project and produce high-quality code.

---

## Project Overview

<!-- Project purpose, positioning, core value -->

**Project Name**: OpenCode Telegram Bot (tg-bot)

**Description**: A Telegram bot for interacting with OpenCode AI programming assistant deployed in internal networks. The bot runs in internal network environments, accesses Telegram API via HTTP proxy, and uses polling to receive messages.

**Core Features**:
- Interact with OpenCode through Telegram Bot with CLI-like experience
- HTTP proxy support for accessing external services
- Polling mode (no public IP required)
- Session management (independent sessions per user)
- Real-time message streaming with periodic updates
- File browsing and project exploration
- AI model and provider management
- Task status monitoring and abortion

---

## Directory Structure

```
.
├── cmd/bot/                 # Application entry point (main.go)
├── internal/                # Internal packages
│   ├── config/             # Configuration management (TOML)
│   ├── handler/            # Telegram command handlers
│   ├── opencode/           # OpenCode API client
│   ├── session/            # Session manager
│   ├── stream/             # SSE streaming utilities
│   └── logging/            # Logging configuration
├── docs/                   # Documentation
│   └── tg-coding.md        # Development notes
├── .github/workflows/      # CI/CD workflows
├── .AGENTS/                # Agent knowledge base
│   └── templates/
│       └── DESIGN.md       # Design document template
├── config.toml             # Runtime configuration
├── config.example.toml     # Example configuration
├── Makefile                # Build and development tasks
├── go.mod                  # Go module definition
└── AGENTS.md               # This file
```

---

## Tech Stack & Tools

- **Language**: Go 1.25.5
- **Build**: Make, Go build
- **Test**: go test (standard Go testing framework)
- **Formatter**: gofmt (standard Go formatter)
- **Linter**: Standard Go vet and staticcheck (golangci-lint recommended but not configured)

---

## Development with Git Worktree

This project uses **Git Worktree** for parallel development of specific tasks. Each feature or bug fix should be developed in a separate worktree to keep the main branch clean and enable parallel development.

### Worktree Commands

```bash
# List existing worktrees
git worktree list

# Create new worktree for a feature/bug
git worktree add ../tg-coding-feature-name feature-branch-name

# Remove worktree when done
git worktree remove ../tg-coding-feature-name
```

### Worktree Naming Convention

Worktrees should follow the naming pattern:
- `../tg-coding-<feature-name>` (relative to main worktree)
- Branch names: descriptive kebab-case (e.g., `add-config-validation`)

### Shared Agent Knowledge Base

To share AGENTS.md and other agent knowledge files across worktrees:

**Option 1: Symbolic Links (Recommended)**
```bash
# Create shared directory outside git repository
mkdir -p /path/to/shared/agents

# Copy AGENTS.md to shared location
cp AGENTS.md /path/to/shared/agents/

# Create symbolic link in each worktree
ln -sf /path/to/shared/agents/AGENTS.md AGENTS.md
```

**Option 2: Git Configuration Reference**
```bash
# Set shared path in git config
git config --local agents.shared-path /path/to/shared/agents

# Read in scripts/automation
agents_path=$(git config agents.shared-path)
```

### Worktree Development Flow

1. **Create Worktree**: `git worktree add ../tg-coding-task-name task-branch`
2. **Switch Context**: `cd ../tg-coding-task-name`
3. **Develop**: Follow standard workflow (Context → Design → Code → Validate)
4. **Test & Validate**: Run tests in worktree
5. **Merge/PR**: Create PR from worktree branch to main
6. **Cleanup**: Remove worktree after merge

---

## Workflow: Context → Design → Code → Validate

### 1. Context First

Before starting any task, gather sufficient context:

```
[ ] Understand task objectives and acceptance criteria
[ ] Read relevant code and documentation
[ ] Identify upstream/downstream dependencies
[ ] Find reference implementations for similar features
[ ] Read .AGENTS/KNOWLEDGE_BASE.md for project knowledge
```

**Ways to gather context**:
- Read specified code files
- Search codebase for related implementations
- Consult documents in .AGENTS/ directory
- Ask user for clarification on unclear requirements

### 2. Design Doc

Complex tasks must produce a Design Doc first (see `.AGENTS/templates/DESIGN.md`):

```
[ ] Fill in Background & Context
[ ] Define Goals & Non-Goals
[ ] Describe Technical Design
[ ] List Test Plan
[ ] Get user confirmation before coding
```

### 3. Coding

```
[ ] Follow .AGENTS/CODING_STYLE.md
[ ] Implement incrementally, keep code compilable
[ ] Test each logical unit immediately after completion
```

### 4. Self Validation

**Must complete before submission**:

```bash
# Build
[ ] Compilation passes with no warnings
[ ] make build succeeds

# Test
[ ] Unit tests pass (make test)
[ ] Integration tests pass (if applicable)

# Code Quality
[ ] Formatter check passes (gofmt -d .)
[ ] Linter check passes (go vet ./...)
[ ] Self code review completed
```

### 5. Commit and PR Workflow

#### When to Commit
Commit changes when:
- A logical unit of work is completed and tested
- All self-validation checks pass (build, tests, formatting)
- The change is ready for review or integration

#### Commit Process
1. **Stage Changes**: Add relevant files to staging area
2. **Create Commit**: Use descriptive commit message following template
3. **Push to Remote**: Push branch to origin for PR creation

Follow commit message template (see `.AGENTS/templates/COMMIT_MESSAGE.md`):

```
<type>(<scope>): <subject>

<body>

<footer>
```

Common commit types:
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `refactor`: Code refactoring
- `test`: Test-related changes
- `chore`: Maintenance tasks

#### PR Creation Process
After committing changes in a worktree branch:

1. **Push Branch**:
   ```bash
   git push -u origin <branch-name>
   ```

2. **Create Pull Request** (using GitHub CLI):
   ```bash
   gh pr create --title "<type>(<scope>): <subject>" --body "$(cat <<'EOF'
   ## Summary
   <1-3 bullet points describing changes>
   
   ## Changes
   - File 1: description of changes
   - File 2: description of changes
   
   ## Testing
   - [ ] Unit tests pass
   - [ ] Integration tests pass (if applicable)
   - [ ] Manual testing performed
   
   ## Notes
   Any additional notes or context
   EOF
   )"
   ```

3. **PR Requirements**:
   - Title follows commit message format
   - Body includes summary, changes, testing details
   - Link related issues if applicable
   - Assign appropriate reviewers

#### Agent Autonomy Guidelines
- **Auto-commit**: Yes, when changes are complete and validated
- **Auto-PR creation**: Yes, after successful commit and push
- **User notification**: Always provide PR URL after creation
- **User confirmation**: Ask before force-pushing or destructive operations

---

## Task Completion Checklist

After each task:

```
[ ] Code follows coding style
[ ] All tests pass
[ ] Auto-commit changes with descriptive message
[ ] Auto-create PR with proper summary and testing details
[ ] Update .AGENTS/KNOWLEDGE_BASE.md (if new knowledge gained)
[ ] Update .AGENTS/CODING_STYLE.md (if new style conventions)
[ ] Record issues in .AGENTS/SUGGESTIONS.md (if improvements identified)
[ ] Update Design Doc to final version
```

---

## Communication Guidelines

### When encountering issues

1. **Insufficient info**: Clearly state what's missing and suggest where to find it
2. **Uncertain direction**: List options with trade-offs, ask user to decide
3. **Stuck for long time**: Proactively report current state and attempted approaches

### Output format

- Code changes: Explain what changed and why
- Design decisions: Document decision and rationale
- Debugging: Record attempted approaches and results

---

## Anti-Patterns (Prohibited)

- ❌ Starting to code without gathering sufficient context
- ❌ Making large code changes without testing
- ❌ Blindly attempting fixes without analyzing root cause
- ❌ Ignoring warnings or linter reports
- ❌ Deviating from coding style without approval

---

## Quick Reference

| File | Purpose |
|------|---------|
| `.AGENTS/KNOWLEDGE_BASE.md` | Factual project knowledge |
| `.AGENTS/CODING_STYLE.md` | Code style guidelines |
| `.AGENTS/SUGGESTIONS.md` | Improvement suggestions backlog |
| `.AGENTS/templates/DESIGN.md` | Design document template |
| `.AGENTS/templates/COMMIT_MESSAGE.md` | Commit message template |
| `.AGENTS/templates/CODE_REVIEW.md` | Code review checklist |
| `.AGENTS/templates/TASK_HANDOFF.md` | Task handoff template |