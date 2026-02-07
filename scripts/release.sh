#!/bin/bash
set -e

# 发布脚本
# 使用方法: ./scripts/release.sh <version>
# 例如: ./scripts/release.sh v1.0.0

if [ $# -ne 1 ]; then
    echo "使用方法: $0 <version>"
    echo "例如: $0 v1.0.0"
    exit 1
fi

VERSION=$1

# 验证版本号格式
if [[ ! $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "错误: 版本号格式应为 vX.Y.Z"
    exit 1
fi

echo "🚀 准备发布版本: $VERSION"

# 1. 检查是否有未提交的更改
if [[ -n $(git status --porcelain) ]]; then
    echo "错误: 有未提交的更改，请先提交或暂存"
    git status
    exit 1
fi

# 2. 运行测试
echo "🧪 运行测试..."
make test

# 3. 创建发布包（本地测试）
echo "📦 创建发布包..."
make release-packages

# 4. 显示创建的包
echo "📁 创建的包:"
ls -la opencode-tg-*.tar.gz

# 5. 创建标签
echo "🏷️  创建标签 $VERSION..."
git tag -a "$VERSION" -m "Release $VERSION"

# 6. 推送标签
echo "📤 推送标签到远程..."
git push origin "$VERSION"

# 7. 显示发布说明
echo ""
echo "✅ 发布流程已启动!"
echo ""
echo "下一步:"
echo "1. 等待 GitHub Actions 完成构建和发布"
echo "2. 检查发布页面: https://github.com/anomalyco/opencode-tg/releases"
echo "3. 编辑发布说明（如果需要）"
echo ""
echo "📦 每个包包含:"
echo "  - opencode-tg (二进制文件)"
echo "  - config.toml (配置文件模板)"
echo "  - README.md (说明文档)"
echo ""
echo "📁 总共5个包:"
echo "  - opencode-tg-linux-amd64.tar.gz"
echo "  - opencode-tg-linux-arm64.tar.gz"
echo "  - opencode-tg-darwin-amd64.tar.gz"
echo "  - opencode-tg-darwin-arm64.tar.gz"
echo "  - opencode-tg-src.tar.gz"