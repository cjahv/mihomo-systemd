#!/bin/bash

set -e

# 确保PATH包含常用的Go安装路径
export PATH=$PATH:/usr/local/go/bin
# 关闭apt的交互
export DEBIAN_FRONTEND=noninteractive

# 读取.env文件
load_env() {
  if [ -f ".env" ]; then
    echo "正在加载 .env 文件..."
    # 读取.env文件并导出变量
    set -a  # 自动导出所有变量
    source .env
    set +a  # 关闭自动导出
    echo "已成功加载 .env 文件"
  else
    echo "未找到 .env 文件，将使用默认环境变量"
  fi
}

# 加载环境变量
load_env

# 特殊处理GITHUB_PROXY和GITHUB_API_PROXY，确保末尾有斜杠
if [ -n "$GITHUB_PROXY" ] && [[ "$GITHUB_PROXY" != */ ]]; then
    GITHUB_PROXY="${GITHUB_PROXY}/"
fi

if [ -n "$GITHUB_API_PROXY" ] && [[ "$GITHUB_API_PROXY" != */ ]]; then
    GITHUB_API_PROXY="${GITHUB_API_PROXY}/"
fi

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
get_system_info() {
  info "检测系统信息..."
  
  # 获取操作系统
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  
  # 获取架构
  MACHINE=$(uname -m)
  case $MACHINE in
    x86_64|amd64)
      ARCH="amd64"
      ;;
    aarch64|arm64)
      ARCH="arm64"
      ;;
    armv7l|armv6l)
      ARCH="arm"
      ;;
    i386|i686)
      ARCH="386"
      ;;
    *)
      error "不支持的架构: $MACHINE"
      ;;
  esac
  
  info "系统信息: OS=$OS, ARCH=$ARCH (原始: $MACHINE)"
}

# 使用Go检查系统架构和CPU特性（需要Go已安装）
verify_system_info_with_go() {
  if ! command -v go &> /dev/null; then
    info "Go未安装，跳过Go验证"
    return 0
  fi
  
  info "使用Go验证系统信息..."
  
  # 创建临时Go程序来检查系统信息
  cat > /tmp/sysinfo.go << 'EOF'
package main

import (
	"fmt"
	"runtime"
)

func main() {
	fmt.Printf("ARCH=%s\n", runtime.GOARCH)
	fmt.Printf("OS=%s\n", runtime.GOOS)
	
	// 检查CPU特性（仅x86_64）
	if runtime.GOARCH == "amd64" {
		// 这里可以添加更详细的CPU特性检测
		// 但对于mihomo安装，基本的amd64就足够了
		fmt.Printf("CPU_FEATURE=standard\n")
	}
}
EOF
  
  # 运行Go程序获取系统信息
  CURRENT_DIR=$(pwd)
  cd /tmp && go run sysinfo.go > sysinfo.txt
  
  # 读取结果
  GO_ARCH=$(grep "ARCH=" sysinfo.txt | cut -d= -f2)
  GO_OS=$(grep "OS=" sysinfo.txt | cut -d= -f2)
  CPU_FEATURE=$(grep "CPU_FEATURE=" sysinfo.txt | cut -d= -f2)
  
  # 清理并返回原目录
  rm -f /tmp/sysinfo.go /tmp/sysinfo.txt
  cd "$CURRENT_DIR"
  
  info "Go验证结果: OS=$GO_OS, ARCH=$GO_ARCH"
  if [ -n "$CPU_FEATURE" ]; then
    info "CPU特性: $CPU_FEATURE"
  fi
  
  # 验证一致性
  if [ "$ARCH" != "$GO_ARCH" ]; then
    warn "架构检测不一致: shell=$ARCH, go=$GO_ARCH，使用Go检测结果"
    ARCH="$GO_ARCH"
  fi
  
  if [ "$OS" != "$GO_OS" ]; then
    warn "操作系统检测不一致: shell=$OS, go=$GO_OS，使用Go检测结果"
    OS="$GO_OS"
  fi
}

# 检测包管理器类型
HAS_DEB=0
HAS_RPM=0

if command -v dpkg &> /dev/null; then
  HAS_DEB=1
fi

if command -v rpm &> /dev/null; then
  HAS_RPM=1
fi

# 安装Mihomo核心
install_mihomo_core() {
  # 注意：保留环境变量，不要覆盖
  # GITHUB_API_PROXY 和 GITHUB_PROXY 应该从环境变量中读取
  
  # 显示代理设置（用于调试）
  if [ -n "$GITHUB_PROXY" ]; then
    info "使用GitHub代理: $GITHUB_PROXY"
  fi
  if [ -n "$GITHUB_API_PROXY" ]; then
    info "使用GitHub API代理: $GITHUB_API_PROXY"
  fi
  
  # 检查是否已安装mihomo
  if command -v mihomo &> /dev/null; then
    info "检测到已安装的 Mihomo，正在检查版本..."
    
    # 获取当前版本
    CURRENT_VERSION_OUTPUT=$(mihomo -v 2>&1)
    if [ $? -eq 0 ]; then
      # 从版本输出中提取版本号（通常格式类似："Mihomo v1.18.0 linux/amd64"）
      CURRENT_VERSION=$(echo "$CURRENT_VERSION_OUTPUT" | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -n1)
      if [ -n "$CURRENT_VERSION" ]; then
        info "当前已安装版本: $CURRENT_VERSION"
        
        # 获取最新版本
        info "正在获取 Mihomo 最新版本..."
        RELEASE_JSON=$(curl -s --connect-timeout 15 --max-time 60 --compressed ${GITHUB_API_PROXY}https://api.github.com/repos/MetaCubeX/mihomo/releases/latest)
        LATEST_VERSION=$(echo "$RELEASE_JSON" | grep "tag_name" | awk -F'"' '{print $4}')
        
        if [ -z "$LATEST_VERSION" ]; then
          error "无法获取最新版本信息"
        fi
        
        info "最新版本: $LATEST_VERSION"
        
        # 比较版本
        if [ "$CURRENT_VERSION" = "$LATEST_VERSION" ]; then
          info "当前版本已是最新版本，跳过安装"
          return 0
        else
          info "发现新版本，将进行更新: $CURRENT_VERSION -> $LATEST_VERSION"
        fi
      else
        warn "无法解析当前版本信息，将继续安装"
      fi
    else
      warn "无法获取当前版本信息，将继续安装"
    fi
  else
    info "未检测到 Mihomo，将进行全新安装"
  fi
  
  # 获取最新版本和发布信息（如果之前没有获取过）
  if [ -z "$LATEST_VERSION" ] || [ -z "$RELEASE_JSON" ]; then
    info "正在获取 Mihomo 最新版本..."
    RELEASE_JSON=$(curl -s --connect-timeout 15 --max-time 60 --compressed ${GITHUB_API_PROXY}https://api.github.com/repos/MetaCubeX/mihomo/releases/latest)
    LATEST_VERSION=$(echo "$RELEASE_JSON" | grep "tag_name" | awk -F'"' '{print $4}')
    
    if [ -z "$LATEST_VERSION" ]; then
      error "无法获取最新版本信息"
    fi
    
    info "最新版本: $LATEST_VERSION"
  fi

  # 选择文件格式，优先使用系统包管理器格式
  FORMAT="gz"  # 默认格式
  if [ $HAS_DEB -eq 1 ]; then
    FORMAT="deb"
    info "检测到 Debian/Ubuntu 系统，将使用 .deb 格式"
  elif [ $HAS_RPM -eq 1 ]; then
    FORMAT="rpm"
    info "检测到 RHEL/CentOS/Fedora 系统，将使用 .rpm 格式"
  else
    info "使用 .gz 格式"
  fi

  # 获取发布包列表
  info "获取可用的发布包列表..."
  RELEASE_FILES=$(echo "$RELEASE_JSON" | jq -r '.assets[].name')

  # 根据系统信息筛选适合的文件
  info "正在筛选适合 ${OS}-${ARCH} 的文件..."
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
  curl -L --connect-timeout 30 --max-time 300 -o "/tmp/${FILENAME}" "$DOWNLOAD_URL"

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
      error "mihomo命令存在但返回错误: ${VERSION_OUTPUT}"
    fi
  else
    error "无法执行mihomo命令，请检查安装路径是否在PATH中"
  fi

  info "Mihomo 安装成功！"
}

# 安装UI
install_ui() {
  info "正在准备安装UI..."
  
  # 注意：保留环境变量，不要覆盖
  # GITHUB_PROXY 应该从环境变量中读取
  
  # 显示代理设置（用于调试）
  if [ -n "$GITHUB_PROXY" ]; then
    info "UI安装使用GitHub代理: $GITHUB_PROXY"
  fi

  # 创建UI目录
  UI_DIR="./ui"
  info "正在下载UI到 ${UI_DIR}..."

  # 如果目录已存在，直接跳过
  if [ -d "$UI_DIR" ]; then
    info "UI目录已存在，跳过安装"
    return 0
  fi

  # 克隆UI仓库
  git clone -b gh-pages --single-branch ${GITHUB_PROXY}https://github.com/MetaCubeX/metacubexd.git "$UI_DIR"
  if [ $? -eq 0 ]; then
    info "UI安装成功！"
  else
    error "UI安装失败，请检查网络连接或手动安装"
  fi
}

# 编译并安装Go管理器
install_go_manager() {
  info "正在编译并安装Go管理器..."
  
  # 确保Go环境正确
  export PATH=$PATH:/usr/local/go/bin
  
  # 检查Go是否可用
  if ! command -v go &> /dev/null; then
    error "Go命令不可用，请确保Go已正确安装"
  fi
  
  # 显示当前工作目录
  info "当前工作目录: $(pwd)"
  info "目录内容: $(ls -la)"
  
  # 检查是否有必要的源码文件
  if [ ! -f "main.go" ] || [ ! -f "go.mod" ]; then
    error "找不到Go源码文件(main.go, go.mod)，请确保在正确的目录"
  fi
  
  # 编译
  info "正在编译Go版本..."
  go build -ldflags "-w -s" -o mihomo-manager .
  
  # 安装到系统
  sudo mv mihomo-manager /usr/local/bin/
  sudo chmod +x /usr/local/bin/mihomo-manager
  
  info "Go管理器编译安装成功！"
}

# 检查基础命令
check_commands curl grep awk jq sudo git nft

# 检查并安装Go
install_go_if_needed() {
  # 确保PATH包含/usr/local/go/bin
  export PATH=$PATH:/usr/local/go/bin
  
  if command -v go &> /dev/null; then
    info "Go已安装: $(go version)"
    return 0
  fi
  
  info "未检测到Go，正在自动安装..."
  
  # 获取最新Go版本
  GO_VERSION=$(curl -s --compressed --connect-timeout 10 --max-time 30 https://go.dev/VERSION?m=text | head -n1)
  if [ -z "$GO_VERSION" ]; then
    # 如果直接获取失败，尝试使用代理
    if [ -n "$GITHUB_API_PROXY" ]; then
      info "直接获取Go版本失败，尝试使用代理重试..."
      GO_VERSION=$(curl -s --compressed --connect-timeout 10 --max-time 30 ${GITHUB_API_PROXY}https://go.dev/VERSION?m=text | head -n1)
    fi
    
    if [ -z "$GO_VERSION" ]; then
      GO_VERSION="go1.21.5"  # 备用版本
      warn "无法获取最新Go版本，使用备用版本: $GO_VERSION"
    fi
  fi
  
  info "将安装Go版本: $GO_VERSION"
  
  # 根据架构下载
  GO_TARBALL="${GO_VERSION}.linux-${ARCH}.tar.gz"
  GO_URL="https://go.dev/dl/${GO_TARBALL}"
  
  info "下载Go: $GO_URL"
  if ! curl -L --compressed --connect-timeout 3 --max-time 300 -o "/tmp/${GO_TARBALL}" "$GO_URL"; then
    # 如果直接下载失败，尝试使用代理
    if [ -n "$GITHUB_API_PROXY" ]; then
      info "直接下载Go失败，尝试使用代理重试..."
      GO_URL_PROXY="${GITHUB_API_PROXY}https://go.dev/dl/${GO_TARBALL}"
      info "使用代理下载Go: $GO_URL_PROXY"
      if ! curl -L --compressed --connect-timeout 3 --max-time 300 -o "/tmp/${GO_TARBALL}" "$GO_URL_PROXY"; then
        error "Go下载失败（包括代理重试），请检查网络连接"
      fi
    else
      error "Go下载失败，请检查网络连接或设置GITHUB_API_PROXY环境变量"
    fi
  fi
  
  # 验证下载的文件
  if [ ! -f "/tmp/${GO_TARBALL}" ] || [ ! -s "/tmp/${GO_TARBALL}" ]; then
    error "Go下载文件不存在或为空"
  fi
  
  # 安装Go
  info "安装Go到/usr/local..."
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
  
  # 添加到PATH
  if ! grep -q "/usr/local/go/bin" /etc/profile; then
    echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee -a /etc/profile
  fi
  
  # 为当前会话设置PATH
  export PATH=$PATH:/usr/local/go/bin
  
  # 清理
  rm -f "/tmp/${GO_TARBALL}"
  
  # 验证安装
  if command -v go &> /dev/null; then
    info "Go安装成功: $(go version)"
  else
    error "Go安装失败"
  fi
}

# 显示环境变量（调试用）
info "环境变量检查："
if [ -n "$GITHUB_PROXY" ]; then
  info "  GITHUB_PROXY=$GITHUB_PROXY"
else
  info "  GITHUB_PROXY未设置"
fi
if [ -n "$GITHUB_API_PROXY" ]; then
  info "  GITHUB_API_PROXY=$GITHUB_API_PROXY"
else
  info "  GITHUB_API_PROXY未设置"
fi

# 开始安装流程
info "开始Mihomo + Go管理器安装..."

# 首先检测系统信息
get_system_info

# 检查并安装Go
install_go_if_needed

# 使用Go验证系统信息
verify_system_info_with_go

# 安装Mihomo核心
install_mihomo_core

# 安装UI  
install_ui

# 编译安装Go管理器
install_go_manager

info "安装完成！"

# 显示使用说明
echo ""
echo -e "${BLUE}=== 使用说明 ===${NC}"
echo "• 启动管理器: mihomo-manager"
echo "• 访问: http://localhost:8000"
echo "• 确保已正确配置.env文件和相关脚本"
