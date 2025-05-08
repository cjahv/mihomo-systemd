#!/bin/bash

set -e

# 颜色设置
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m' # 重置颜色

# 检查必要的命令
check_commands() {
  local missing=()
  for cmd in "$@"; do
    if ! command -v "$cmd" &> /dev/null; then
      missing+=("$cmd")
    fi
  done
  if [ ${#missing[@]} -ne 0 ]; then
    echo -e "${RED}错误: 缺少以下依赖命令: ${missing[*]}，请先安装。${NC}"
    exit 1
  fi
}

check_commands curl grep awk jq sudo git nft

# 输出信息函数
info() {
  echo -e "${GREEN}[信息]${NC} $1"
}

warn() {
  echo -e "${YELLOW}[警告]${NC} $1"
}

error() {
  echo -e "${RED}[错误]${NC} $1"
  exit 1
}

# 获取系统信息
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')

# 架构转换
case $ARCH in
  x86_64)
    ARCH="amd64"
    ;;
  aarch64)
    ARCH="arm64"
    ;;
  *)
    error "不支持的架构: $ARCH"
    ;;
esac

# 检测包管理器类型
HAS_DEB=0
HAS_RPM=0

if command -v dpkg &> /dev/null; then
  HAS_DEB=1
fi

if command -v rpm &> /dev/null; then
  HAS_RPM=1
fi

# 获取最新版本
info "正在获取 Mihomo 最新版本..."
RELEASE_JSON=$(curl -s ${GITHUB_API_PROXY}https://api.github.com/repos/MetaCubeX/mihomo/releases/latest)

LATEST_VERSION=$(echo "$RELEASE_JSON" | grep "tag_name" | awk -F'"' '{print $4}')

if [ -z "$LATEST_VERSION" ]; then
  error "无法获取最新版本信息"
fi

info "最新版本: $LATEST_VERSION"

# 选择文件格式
FORMAT="gz"  # 默认格式
if [ $HAS_DEB -eq 1 ]; then
  FORMAT="deb"
  info "检测到 Debian/Ubuntu 系统，将使用 .deb 格式"
elif [ $HAS_RPM -eq 1 ]; then
  FORMAT="rpm"
  info "检测到 RHEL/CentOS/Fedora 系统，将使用 .rpm 格式"
else
  info "默认使用 .gz 格式"
fi

# 获取发布包列表（从缓存变量中提取）
info "获取可用的发布包列表..."
RELEASE_FILES=$(echo "$RELEASE_JSON" | jq -r '.assets[].name')

# 根据系统信息筛选适合的文件
info "正在筛选适合 ${OS}-${ARCH}-${FORMAT} 的文件..."
PATTERN="mihomo-${OS}-${ARCH}.*\.${FORMAT}$"
AVAILABLE_FILES=$(echo "$RELEASE_FILES" | grep -E "$PATTERN" || echo "")

if [ -z "$AVAILABLE_FILES" ]; then
  error "未找到适合您系统的版本包"
fi

# 优先选择Go版本从高到低
info "找到以下可用版本，将按Go版本从高到低排序选择："
echo "$AVAILABLE_FILES" | sort -r

# 获取第一个匹配的文件（版本最高的）
FILENAME=$(echo "$AVAILABLE_FILES" | sort -r | head -n 1)
DOWNLOAD_URL="${GITHUB_PROXY}https://github.com/MetaCubeX/mihomo/releases/download/${LATEST_VERSION}/${FILENAME}"

info "选择的文件: ${FILENAME}"
info "下载地址: ${DOWNLOAD_URL}"

# 下载文件
info "正在下载 Mihomo..."
curl -L -o "/tmp/${FILENAME}" "$DOWNLOAD_URL"

# 安装
info "正在安装 Mihomo..."
case $FORMAT in
  deb)
    sudo dpkg -i "/tmp/${FILENAME}"
    ;;
  rpm)
    sudo rpm -i "/tmp/${FILENAME}"
    ;;
  gz)
    sudo mkdir -p /usr/local/bin
    sudo gunzip -c "/tmp/${FILENAME}" > "/tmp/mihomo"
    sudo chmod +x "/tmp/mihomo"
    sudo mv "/tmp/mihomo" /usr/local/bin/
    ;;
esac

# 清理
rm -f "/tmp/${FILENAME}"

# 验证安装
info "正在验证安装..."
if command -v mihomo &> /dev/null; then
  VERSION_OUTPUT=$(mihomo -v 2>&1)
  if [ $? -eq 0 ]; then
    info "验证成功: ${VERSION_OUTPUT}"
  else
    warn "mihomo命令存在但返回错误: ${VERSION_OUTPUT}"
  fi
else
  warn "无法执行mihomo命令，请检查安装路径是否在PATH中"
fi

info "Mihomo 安装成功！"

# 安装UI
info "正在准备安装UI..."

# 创建UI目录
UI_DIR="./ui"
info "正在下载UI到 ${UI_DIR}..."

# 如果目录已存在，先询问是否覆盖
if [ -d "$UI_DIR" ]; then
  echo -n -e "${YELLOW}UI目录已存在，是否覆盖？[y/N] ${NC}"
  read -r response
  if [[ "$response" =~ ^([yY][eE][sS]|[yY])$ ]]; then
    info "正在删除旧UI目录..."
    rm -rf "$UI_DIR"
  else
    info "跳过UI安装"
    exit 0
  fi
fi

# 克隆UI仓库
git clone -b gh-pages --single-branch ${GITHUB_PROXY}https://github.com/MetaCubeX/metacubexd.git "$UI_DIR"
if [ $? -eq 0 ]; then
  info "UI安装成功！"
else
  error "UI安装失败，请检查网络连接或手动安装"
fi

info "安装完成！Mihomo和UI均已成功安装"
