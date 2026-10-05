#!/bin/bash
#
# 发布脚本 - 探测目标，在本地交叉编译并部署到远端 systemd
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
source ./deploy/lib/target.sh
source ./deploy/lib/env.sh
#

# 颜色定义
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m' # 无颜色

# 日志函数
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

usage() {
    printf '用法: mise run publish -- [--build-only | --force]\n'
    printf '  --build-only  仅探测目标并在本地构建\n'
    printf '  --force       保留旧恢复包，跳过未完成发布的自动恢复并重新安装\n'
}

BUILD_ONLY=false
FORCE_INSTALL=false
case "${1:-}" in
    "") ;;
    --build-only) BUILD_ONLY=true ;;
    --force) FORCE_INSTALL=true ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 1 ;;
esac
[ "$#" -le 1 ] || { log_error "参数过多"; exit 1; }

handle_error() {
    log_error "$1"
    exit 1
}

# 远端命令使用 POSIX 单引号，兼容登录 shell 为 sh 的服务器。
quote_shell() {
    printf "'%s'" "${1//\'/\'\\\'\'}"
}

load_env_file .env || handle_error "未找到 .env，请根据 .env.template 创建"
[[ "${REMOTE_USER:-}" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$ ]] || handle_error "REMOTE_USER 无效"
[[ "${REMOTE_HOST:-}" =~ ^[a-zA-Z0-9][a-zA-Z0-9.:-]*$ ]] || handle_error "REMOTE_HOST 无效"
[[ "${REMOTE_DIR:-}" == /* && "$REMOTE_DIR" != / && "$REMOTE_DIR" != *$'\n'* ]] || handle_error "REMOTE_DIR 必须为非根目录的绝对路径"
for cmd in ssh tar mise; do
    command -v "$cmd" >/dev/null || handle_error "本地缺少命令: $cmd"
done
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=10)
remote="${REMOTE_USER}@${REMOTE_HOST}"
remote_dir=$(quote_shell "$REMOTE_DIR")

# 只读探测在任何远端目录创建、文件上传或服务操作之前完成。
log_info "探测远端操作系统、CPU 架构、内核和 systemd..."
probe=$("${SSH[@]}" "$remote" 'sh -s' <<'REMOTE'
set -eu
for cmd in bash tar sha256sum systemctl flock sudo; do
    command -v "$cmd" >/dev/null || { echo "缺少远端命令: $cmd" >&2; exit 1; }
done
[ -d /run/systemd/system ] || { echo "远端未运行 systemd" >&2; exit 1; }
printf '%s|%s|%s\n' "$(uname -s)" "$(uname -m)" "$(uname -r)"
REMOTE
) || handle_error "远端环境探测失败"
IFS='|' read -r target_os target_machine target_kernel <<< "$probe"
configure_target "$target_os" "$target_machine" "$target_kernel" || handle_error "远端环境不受支持"
target_description="目标: ${TARGET_GOOS}/${TARGET_GOARCH}，内核 ${target_kernel}"
if [ "$TARGET_GOARCH" = arm ]; then target_description="${target_description}，GOARM=$TARGET_GOARM"; fi
log_info "$target_description"

mkdir -p .tmp
build_dir=$(mktemp -d "$PWD/.tmp/deploy.XXXXXX")
cleanup() { rm -rf "$build_dir"; }
trap cleanup EXIT
artifact="$PWD/.tmp/mihomo-manager-$TARGET_GOOS-$TARGET_GOARCH"
[ "$TARGET_GOARCH" != arm ] || artifact="$artifact-${TARGET_GOARM%%,*}"
log_info "使用 mise 管理的 Go 在本地交叉编译..."
# 显式限制指令集，避免继承开发机的性能优化参数。
mise exec -- env GOTOOLCHAIN=local CGO_ENABLED=0 GOOS="$TARGET_GOOS" GOARCH="$TARGET_GOARCH" \
    GOAMD64=v1 GOARM64=v8.0 GOARM="$TARGET_GOARM" GO386=softfloat \
    go build -trimpath -ldflags "-w -s" -o "$build_dir/mihomo-manager" ./cmd/mihomo-manager || handle_error "本地交叉编译失败"
cp "$build_dir/mihomo-manager" "$artifact"
log_info "构建完成: $artifact"
if [ "$BUILD_ONLY" = true ]; then
    exit 0
fi

# deploy/ 是唯一运行时文件清单；管理页面随二进制发布。
cp -R deploy/. "$build_dir/"
LOCAL_FILES=(install.sh release.sh scripts lib systemd mihomo-manager)
if command -v shasum >/dev/null; then
    (cd "$build_dir" && shasum -a 256 mihomo-manager > mihomo-manager.sha256)
else
    (cd "$build_dir" && sha256sum mihomo-manager > mihomo-manager.sha256)
fi
LOCAL_FILES+=(mihomo-manager.sha256)
# 远端已有的私有配置继续由远端维护。
if "${SSH[@]}" "$remote" "test -f $remote_dir/.env"; then
    log_info "保留远端已有 .env"
else
    env_check_rc=$?
    [ "$env_check_rc" -eq 1 ] || handle_error "检查远端 .env 失败"
    cp .env "$build_dir/.env"
    LOCAL_FILES+=(.env)
fi

log_info "上传运行时文件与预编译管理器..."
remote_stage=$("${SSH[@]}" "$remote" "mkdir -p $remote_dir && mktemp -d $remote_dir/.deploy.XXXXXX") || handle_error "无法创建远端暂存目录"
[[ "$remote_stage" == "$REMOTE_DIR"/.deploy.* && "$remote_stage" != *$'\n'* ]] || handle_error "远端返回无效暂存目录"
stage_dir=$(quote_shell "$remote_stage")
if ! COPYFILE_DISABLE=1 tar --format=ustar -C "$build_dir" -cf - "${LOCAL_FILES[@]}" | "${SSH[@]}" "$remote" "umask 077; tar -xf - -C $stage_dir"; then
    "${SSH[@]}" "$remote" "rm -rf $stage_dir" || true
    handle_error "上传失败，未执行安装"
fi

log_info "校验传输产物并安装..."
if [ "$FORCE_INSTALL" = true ]; then
    log_info "强制安装：保留旧恢复包，跳过未完成发布的自动恢复"
fi
download_terminal=0
[ ! -t 2 ] || download_terminal=1
"${SSH[@]}" "$remote" "bash -s -- $remote_dir $stage_dir $download_terminal $FORCE_INSTALL" <<'REMOTE'
set -euo pipefail
umask 077
deploy_dir="$1"
stage_dir="$2"
export MIHOMO_DOWNLOAD_TERMINAL="$3"
force_install="$4"
case "$force_install" in true|false) ;; *) exit 1 ;; esac
trap 'rm -rf "$stage_dir"' EXIT
cd "$stage_dir"
sha256sum -c mihomo-manager.sha256
# 先确认目标可执行，失败时保留现有运行时文件。
./mihomo-manager --version
if [ -f "$deploy_dir/.env" ]; then cp "$deploy_dir/.env" .env; fi
[ -f .env ] || { echo '缺少部署环境配置' >&2; exit 1; }
chmod +x install.sh release.sh scripts/update.sh scripts/entrypoint.sh
# release.sh owns cleanup and keeps the recovery package when compensation fails.
trap - EXIT
if [ "$force_install" = true ]; then
    ./release.sh "$deploy_dir" "$stage_dir" --force
else
    ./release.sh "$deploy_dir" "$stage_dir"
fi
REMOTE
log_info "远端部署完成"
