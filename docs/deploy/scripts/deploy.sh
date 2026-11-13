#!/usr/bin/env bash
# ==============================================================================
# Platform-OPC 生产环境一键拉取、编译构建与部署脚本
# 规范：基于 release 分支的 7 位 Commit ID 标记镜像版本
# ==============================================================================

set -euo pipefail

# 脚本所在目录及仓库根目录定位
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

cd "${REPO_ROOT}"

echo "=================================================================="
echo "🚀 开始 Platform-OPC 生产环境构建与部署流程"
echo "📂 代码仓库根目录: ${REPO_ROOT}"
echo "=================================================================="

# 1. 检查是否在 git 仓库中
if [ ! -d ".git" ]; then
    echo "❌ 错误: 当前目录并非 Git 仓库，无法获取 commit 版本！"
    exit 1
fi

# 2. 切换至 release 分支并拉取最新远程代码
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
if [ "${CURRENT_BRANCH}" != "release" ]; then
    echo "⚠️  当前分支为 [${CURRENT_BRANCH}]，正在自动切换至 [release] 分支..."
    git checkout release
fi

echo "📥 正在拉取 release 分支最新代码..."
git fetch origin release
git pull origin release

# 3. 获取 release 分支最新的 7 位 short commit ID 与提交信息
COMMIT_ID="$(git rev-parse --short=7 HEAD)"
COMMIT_MSG="$(git log -1 --pretty=format:'%s (%an, %cd)' --date=short)"
echo "------------------------------------------------------------------"
echo "🏷️  当前部署目标版本 Commit ID : ${COMMIT_ID}"
echo "📝 最新提交记录               : ${COMMIT_MSG}"
echo "------------------------------------------------------------------"

# 4. 执行 Docker 多阶段构建（产物自动命名为 platform-opc）
IMAGE_TAGGED="platform-opc:${COMMIT_ID}"
IMAGE_LATEST="platform-opc:latest"

echo "🔨 正在使用 Dockerfile 执行多阶段编译与镜像构建..."
docker build -t "${IMAGE_TAGGED}" -f docs/deploy/Dockerfile .
docker tag "${IMAGE_TAGGED}" "${IMAGE_LATEST}"

echo "✅ 镜像构建成功并打标完成:"
echo "   - ${IMAGE_TAGGED} (版本化追溯镜像)"
echo "   - ${IMAGE_LATEST}  (当前生效最新镜像)"
echo "------------------------------------------------------------------"

# 5. 检查是否在部署目录中存在运行配置
DEPLOY_DIR="${REPO_ROOT}/docs/deploy"
if [ -f "${DEPLOY_DIR}/platform-opc.env" ] && [ -f "${DEPLOY_DIR}/config.yaml" ]; then
    echo "🚀 检测到部署配置文件就绪，正在滚动启动容器..."
    cd "${DEPLOY_DIR}"
    # 使用刚构建的 commit 标签启动容器
    export PLATFORM_OPC_TAG="${COMMIT_ID}"
    docker compose up -d
    echo "------------------------------------------------------------------"
    echo "🎉 Platform-OPC 容器启动完毕！运行版本: ${COMMIT_ID}"
    docker ps --filter name=platform-opc --format "table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}"
else
    echo "ℹ️  提示: 未检测到 docs/deploy/platform-opc.env 或 config.yaml。"
    echo "   若要在宿主机生产目录运行，请执行:"
    echo "   export PLATFORM_OPC_TAG=${COMMIT_ID}"
    echo "   docker compose -f docs/deploy/docker-compose.yaml up -d"
fi
echo "=================================================================="
