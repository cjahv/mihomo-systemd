#!/bin/bash

set -e

# 所有相对路径均以部署目录为准。
cd "$(dirname "$0")"
source ./lib/target.sh
source ./lib/env.sh
source ./lib/http.sh
if [ "$#" -ne 1 ] || [[ "$1" != /* ]]; then
  echo "用法: install.sh /absolute/path/to/prebuilt-mihomo-manager" >&2
  exit 1
fi
MANAGER_BINARY="$1"
# 关闭apt的交互
export DEBIAN_FRONTEND=noninteractive

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

has_cmd() {
  command -v "$1" &> /dev/null
}

sha256_file() {
  local target="$1"
  if has_cmd sha256sum; then
    sha256sum "$target" | awk '{print $1}'
    return 0
  fi
  if has_cmd shasum; then
    shasum -a 256 "$target" | awk '{print $1}'
    return 0
  fi
  if has_cmd openssl; then
    openssl dgst -sha256 "$target" | awk '{print $2}'
    return 0
  fi
  return 1
}

extract_sha256_for_filename() {
  local text="$1"
  local filename="$2"
  if [ -z "$text" ] || [ -z "$filename" ]; then
    return 1
  fi
  printf "%s\n" "$text" | awk -v name="$filename" '
    BEGIN {IGNORECASE=1}
    index($0, name) {
      match($0, /[a-fA-F0-9]{64}/)
      if (RLENGTH > 0) {
        print substr($0, RSTART, RLENGTH)
        exit
      }
    }
  '
}

verify_sha256_if_available() {
  local expected_hash="$1"
  local target="$2"
  if [ -z "$expected_hash" ]; then
    warn "发布页未提供SHA256，跳过校验"
    return 0
  fi
  if ! has_cmd sha256sum && ! has_cmd shasum && ! has_cmd openssl; then
    warn "缺少校验工具(sha256sum/shasum/openssl)，跳过校验"
    return 0
  fi
  local actual=""
  actual=$(sha256_file "$target") || return 1
  expected_hash=$(printf "%s" "$expected_hash" | tr 'A-F' 'a-f')
  actual=$(printf "%s" "$actual" | tr 'A-F' 'a-f')
  if [ "$expected_hash" != "$actual" ]; then
    warn "校验失败: $(basename "$target")"
    return 1
  fi
  return 0
}

# 使用已验证的目标契约选择 Mihomo 发布包。
get_system_info() {
  OS="$TARGET_GOOS"
  ARCH="$TARGET_GOARCH"
  info "系统信息: OS=$OS, ARCH=$ARCH"
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
install_mihomo_core() (
  local current_output current_version="" core_available=false release_file work_dir=""
  if command -v mihomo &>/dev/null && current_output=$(mihomo -v 2>&1); then
    core_available=true
    current_version=$(printf '%s\n' "$current_output" | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -n1 || true)
    info "当前可用核心: ${current_version:-版本未知}"
  fi
  info "正在获取 Mihomo 最新版本..."
  release_file=$(mktemp)
  trap 'rm -f "$release_file"; [ -z "$work_dir" ] || rm -rf "$work_dir"' EXIT
  if ! github_download "$release_file" 90 4194304 "${GITHUB_API_PROXY:-}" \
      "https://api.github.com/repos/MetaCubeX/mihomo/releases/latest"; then
    rm -f "$release_file"
    if [ "$core_available" = true ]; then
      warn "所有版本查询来源失败，保留已验证可运行的核心；本次未检查或更新核心版本"
      return 0
    fi
    error "版本查询失败且没有可运行的 Mihomo；请检查网络或先离线安装核心"
  fi
  RELEASE_JSON=$(cat "$release_file")
  rm -f "$release_file"
  if ! LATEST_VERSION=$(printf '%s' "$RELEASE_JSON" | jq -er '.tag_name | select(type == "string" and test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))'); then
    if [ "$core_available" = true ]; then
      warn "发布信息无效，保留当前可运行核心；本次未更新核心"
      return 0
    fi
    error "发布信息缺少有效版本号"
  fi
  info "最新版本: $LATEST_VERSION"
  if [ "$current_version" = "$LATEST_VERSION" ]; then
    info "当前版本已是最新版本，跳过安装"
    return 0
  fi

  RELEASE_BODY=$(echo "$RELEASE_JSON" | jq -r '.body // empty')

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
  DOWNLOAD_URL="https://github.com/MetaCubeX/mihomo/releases/download/${LATEST_VERSION}/${FILENAME}"
  MIHOMO_EXPECTED_HASH=$(extract_sha256_for_filename "$RELEASE_BODY" "$FILENAME" || true)

  info "选择的文件: ${FILENAME}"
  if [ -n "$MIHOMO_EXPECTED_HASH" ]; then
    info "使用发布页SHA256校验"
  else
    warn "发布页未提供SHA256，跳过校验"
  fi

  # 独立临时目录避免固定 /tmp 文件冲突；下载完成并校验后才安装。
  work_dir=$(mktemp -d)
  info "正在下载 Mihomo..."
  if ! github_download "$work_dir/$FILENAME" 600 268435456 "${GITHUB_PROXY:-}" "$DOWNLOAD_URL"; then
    rm -rf "$work_dir"
    if [ "$core_available" = true ]; then
      warn "所有核心下载来源失败，保留当前可运行核心；本次未更新核心"
      return 0
    fi
    error "Mihomo 下载失败，首次安装无法继续"
  fi
  if ! verify_sha256_if_available "$MIHOMO_EXPECTED_HASH" "$work_dir/$FILENAME"; then
    rm -rf "$work_dir"
    error "核心校验失败"
  fi

  info "正在安装 Mihomo..."
  case $FORMAT in
    deb) sudo dpkg -i "$work_dir/$FILENAME" ;;
    rpm) sudo rpm -i "$work_dir/$FILENAME" ;;
    gz)
      gunzip -c "$work_dir/$FILENAME" > "$work_dir/mihomo"
      sudo install -m 0755 "$work_dir/mihomo" /usr/local/bin/mihomo
      ;;
  esac
  rm -rf "$work_dir"

  # 验证安装
  info "正在验证安装..."
  if command -v mihomo &> /dev/null; then
    if VERSION_OUTPUT=$(mihomo -v 2>&1); then
      info "验证成功: ${VERSION_OUTPUT}"
    else
      error "mihomo命令存在但返回错误: ${VERSION_OUTPUT}"
    fi
  else
    error "无法执行mihomo命令，请检查安装路径是否在PATH中"
  fi

  info "Mihomo 安装成功！"
)

# 安装UI
install_ui() (
  if [ -d ./ui ]; then
    info "UI目录已存在，跳过安装"
    return 0
  fi
  info "正在下载UI..."
  local work_dir
  work_dir=$(mktemp -d ./.ui.XXXXXX)
  trap 'rm -rf "$work_dir"' EXIT
  if ! github_download "$work_dir/ui.tar.gz" 300 134217728 "${GITHUB_PROXY:-}" \
      "https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.tar.gz"; then
    rm -rf "$work_dir"
    error "UI下载失败，请检查网络或离线提供 ui 目录"
  fi
  mkdir "$work_dir/content"
  if ! tar -xzf "$work_dir/ui.tar.gz" --strip-components=1 -C "$work_dir/content" || \
      [ ! -f "$work_dir/content/index.html" ]; then
    rm -rf "$work_dir"
    error "UI发布包无效"
  fi
  mv "$work_dir/content" ./ui
  rm -rf "$work_dir"
  info "UI安装成功！"
)

# 安装前执行无副作用的版本命令，确认二进制可在目标机运行。
validate_manager_binary() {
  [ -f "$MANAGER_BINARY" ] && [ -x "$MANAGER_BINARY" ] || error "缺少可执行管理器: $MANAGER_BINARY；请通过 mise run publish 重新构建并发布"
  configure_target "$(uname -s)" "$(uname -m)" "$(uname -r)" || error "目标环境不受支持"
  local version
  version=$("$MANAGER_BINARY" --version) || error "管理器无法在当前系统运行，请重新探测目标并交叉编译"
  [[ "$version" == mihomo-manager\ go*\ "$TARGET_GOOS/$TARGET_GOARCH" ]] || error "管理器版本或架构不匹配: $version"
  info "管理器验证成功: $version"
}

# 同目录临时文件 + rename，允许替换正在运行的旧二进制。
install_manager_binary() {
  info "正在安装预编译管理器..."
  sudo mkdir -p /usr/local/bin
  local pending
  pending=$(sudo mktemp /usr/local/bin/.mihomo-manager.XXXXXX)
  if ! sudo install -m 0755 "$MANAGER_BINARY" "$pending"; then
    sudo rm -f "$pending"
    error "复制管理器失败"
  fi
  if ! sudo mv -f "$pending" /usr/local/bin/mihomo-manager; then
    sudo rm -f "$pending"
    error "替换管理器失败"
  fi
  info "预编译管理器安装成功！"
}

# 模板与服务安装仅归安装器所有，配置更新不创建或启用服务。
render_service() {
  local template="$1" line
  local systemd_dir="${PWD//\\/\\\\}"
  systemd_dir="${systemd_dir//\"/\\\"}"
  systemd_dir="${systemd_dir//%/%%}"
  local exec_dir="${systemd_dir//\$/\$\$}"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      WorkingDirectory=@DEPLOY_DIR@) printf 'WorkingDirectory="%s"\n' "$systemd_dir" ;;
      ExecStart=@ENTRYPOINT@) printf 'ExecStart="%s/scripts/entrypoint.sh"\n' "$exec_dir" ;;
      *) printf '%s\n' "$line" ;;
    esac
  done < "$template"
}

install_services() {
  local unit rendered
  sudo mkdir -p /etc/systemd/system
  for unit in mihomo mihomo-manager; do
    rendered=$(mktemp)
    if ! render_service "systemd/$unit.service.in" > "$rendered" || ! sudo install -m 0644 "$rendered" "/etc/systemd/system/$unit.service"; then
      rm -f "$rendered"
      error "安装 $unit systemd 服务失败"
    fi
    rm -f "$rendered"
  done
  sudo systemctl daemon-reload
  sudo systemctl enable mihomo.service mihomo-manager.service
}

# 下载或修改系统前验证发布流程提供的二进制。
validate_manager_binary
check_commands curl grep awk jq sudo tar nft install mktemp systemctl
for runtime_file in scripts/update.sh scripts/entrypoint.sh systemd/mihomo.service.in systemd/mihomo-manager.service.in; do
  [ -f "$runtime_file" ] || error "运行包缺少文件: $runtime_file"
done

load_env_file .env || error "运行包缺少 .env，请从开发机重新发布"
if [ -n "${GITHUB_PROXY:-}" ] && [[ "$GITHUB_PROXY" != */ ]]; then
  GITHUB_PROXY="${GITHUB_PROXY}/"
fi
if [ -n "${GITHUB_API_PROXY:-}" ] && [[ "$GITHUB_API_PROXY" != */ ]]; then
  GITHUB_API_PROXY="${GITHUB_API_PROXY}/"
fi
chmod +x scripts/update.sh scripts/entrypoint.sh

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

# 安装Mihomo核心
install_mihomo_core

# 安装UI
install_ui

# 安装预编译Go管理器
install_manager_binary
install_services

info "安装完成！"

# 显示使用说明
echo ""
echo -e "${BLUE}=== 使用说明 ===${NC}"
echo "• 启动管理器: mihomo-manager"
echo "• 访问: http://localhost:8000"
echo "• 确保已正确配置.env文件和相关脚本"
