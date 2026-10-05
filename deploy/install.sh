#!/bin/bash
# 所有下载与检查在 --prepare 中完成；--apply 仅安装已准备的运行包。
set -euo pipefail
umask 077
cd "$(dirname "$0")"
source ./lib/target.sh
source ./lib/env.sh
source ./lib/http.sh
source ./lib/release.sh

info() { printf '[信息] %s\n' "$1"; }
warn() { printf '[警告] %s\n' "$1" >&2; }
error() { printf '[错误] %s\n' "$1" >&2; exit 1; }
[ "$#" -eq 2 ] && [[ "$2" == /* ]] || error '用法: install.sh --prepare|--apply /absolute/path/to/prebuilt-manager'
PREPARED_DIR="${MIHOMO_PREPARED_DIR:-$PWD/.release}"
MODE="$1"
MANAGER_BINARY="$2"
FORCE_CORE_INSTALL="${MIHOMO_FORCE_CORE_INSTALL:-false}"
case "$FORCE_CORE_INSTALL" in true|false) ;; *) error '无效核心强制安装参数' ;; esac
case "$MODE" in --prepare|--apply) ;; *) error '无效安装阶段' ;; esac
[ -x "$MANAGER_BINARY" ] && [ -f "$MANAGER_BINARY" ] || error '缺少可执行管理器，请重新运行 mise run publish'
configure_target "$(uname -s)" "$(uname -m)" "$(uname -r)" || error '目标环境不受支持'
version=$("$MANAGER_BINARY" --version) || error '管理器不能在目标环境运行'
[[ "$version" == mihomo-manager\ go*\ "$TARGET_GOOS/$TARGET_GOARCH" ]] || error '管理器架构或版本不匹配'
info "管理器验证成功: $version"
for cmd in curl jq sudo tar nft install mktemp systemctl gzip cmp; do
    command -v "$cmd" >/dev/null || error "缺少运行依赖: $cmd"
done
command -v sha256sum >/dev/null || command -v shasum >/dev/null || error '缺少 SHA256 校验工具'
load_env_file .env || error '运行包缺少 .env'
for file in scripts/update.sh scripts/entrypoint.sh systemd/mihomo.service.in systemd/mihomo-manager.service.in; do
    [ -f "$file" ] || error "运行包缺少 $file"
done
chmod +x scripts/update.sh scripts/entrypoint.sh

prepare_core() {
    local current="/usr/local/bin/mihomo" current_version="" current_tag="" metadata="$PREPARED_DIR/latest.json" latest name expected
    # The service uses this exact path; a PATH shadow must not decide its version.
    if [ ! -x "$current" ]; then current=$(command -v mihomo || true); fi
    if [ -n "$current" ] && current_version=$("$current" -v); then
        info "当前磁盘核心: $current_version"
        current_tag=$(core_release_version "$current_version" || true)
    else current=""
    fi
    info '正在获取 Mihomo 最新版本...'
    if ! github_download "$metadata" 90 4194304 "${GITHUB_API_PROXY:-}" https://api.github.com/repos/MetaCubeX/mihomo/releases/latest ||
       ! latest=$(jq -er '.tag_name | select(type == "string" and test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))' "$metadata"); then
        [ -n "$current" ] || error '发布信息不可用或缺少可验证的基线包，且没有可运行核心'
        warn '无法取得可验证的发布包，保留已验证可运行的核心；本次未更新核心'
        cp "$current" "$PREPARED_DIR/mihomo"
        chmod 0755 "$PREPARED_DIR/mihomo"
        return
    fi
    if [ "$FORCE_CORE_INSTALL" = false ] && [ -n "$current" ] && [ "$current_tag" = "$latest" ]; then
        info "Mihomo 已是最新版本 ${latest}，复用现有核心，跳过下载"
        cp "$current" "$PREPARED_DIR/mihomo"
        chmod 0755 "$PREPARED_DIR/mihomo"
        return
    fi
    if ! name=$(select_core_asset "$metadata" "$latest" "$TARGET_GOOS" "$TARGET_GOARCH" "$TARGET_GOARM") ||
       ! expected=$(asset_sha256 "$metadata" "$name"); then
        [ -n "$current" ] || error '发布信息缺少可验证的基线包，且没有可运行核心'
        warn '无法取得可验证的发布包，保留已验证可运行的核心；本次未更新核心'
        cp "$current" "$PREPARED_DIR/mihomo"
        chmod 0755 "$PREPARED_DIR/mihomo"
        return
    fi
    info "选择 CPU 基线发布包: ${name}，版本 $latest"
    if ! github_download "$PREPARED_DIR/core.gz" 600 268435456 "${GITHUB_PROXY:-}" "https://github.com/MetaCubeX/mihomo/releases/download/$latest/$name"; then
        [ -n "$current" ] || error '核心下载失败，首次安装无法继续'
        warn '所有核心下载来源失败，保留当前可运行核心；本次未更新核心'
        cp "$current" "$PREPARED_DIR/mihomo"
        chmod 0755 "$PREPARED_DIR/mihomo"
        return
    fi
    verify_asset "$expected" "$PREPARED_DIR/core.gz" || error '核心发布包 SHA256 校验失败，未执行安装'
    info '核心发布包 SHA256 校验成功'
    gzip -dc "$PREPARED_DIR/core.gz" > "$PREPARED_DIR/mihomo" || error '核心发布包解压失败'
    chmod 0755 "$PREPARED_DIR/mihomo"
    "$PREPARED_DIR/mihomo" -v || error '核心不能在目标环境运行'
}

prepare_ui() {
    if [ -f "${MIHOMO_SOURCE_DIR:-$PWD}/ui/index.html" ]; then
        info '已有可用 UI，保留现有面板'
        return
    fi
    info '准备 MetaCubeXD 面板...'
    github_download "$PREPARED_DIR/ui.tar.gz" 300 134217728 "${GITHUB_PROXY:-}" https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.tar.gz || error '面板下载失败'
    mkdir "$PREPARED_DIR/ui"
    tar -xzf "$PREPARED_DIR/ui.tar.gz" --strip-components=1 -C "$PREPARED_DIR/ui" || error '面板解压失败'
    [ -f "$PREPARED_DIR/ui"/index.html ] || error '面板发布包无效'
}

install_binary() {
    local source="$1" name="$2" pending
    sudo mkdir -p "/usr/local/bin"
    pending=$(sudo mktemp "/usr/local/bin/.$name.XXXXXX")
    if ! sudo install -m 0755 "$source" "$pending" || ! sudo mv -f "$pending" "/usr/local/bin/$name"; then
        sudo rm -f "$pending"
        error "安装 $name 失败"
    fi
}

render_service() {
    local template="$1" line
    # WorkingDirectory is a path value, not a command word: quoting/backslash
    # escaping would become part of the path. Only unit specifiers are escaped.
    local working_dir="${PWD//%/%%}"
    # ExecStart separately parses quoted command words and expands $ variables.
    local exec_dir="${PWD//\\/\\\\}"
    exec_dir="${exec_dir//\"/\\\"}"
    exec_dir="${exec_dir//%/%%}"
    exec_dir="${exec_dir//\$/\$\$}"
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in
            WorkingDirectory=@DEPLOY_DIR@) printf 'WorkingDirectory=%s\n' "$working_dir" ;;
            # systemd restricts executable names containing quotes/backslashes;
            # a fixed Bash executable accepts the deployment path as an argument.
            ExecStart=@ENTRYPOINT@) printf 'ExecStart=/bin/bash "%s/scripts/entrypoint.sh"\n' "$exec_dir" ;;
            *) printf '%s\n' "$line" ;;
        esac
    done < "$template"
}

if [ "$MODE" = --prepare ]; then
    mkdir "$PREPARED_DIR"
    prepare_core
    prepare_ui
    # Local integrity manifest covers expanded executables and the prepared UI.
    (cd "$PREPARED_DIR" && if command -v sha256sum >/dev/null; then sha256sum mihomo; else shasum -a 256 mihomo; fi) > "$PREPARED_DIR/mihomo.sha256"
    info '运行包准备完成；尚未替换系统程序或切换服务'
else
    [ -x "$PREPARED_DIR/mihomo" ] || error '缺少预检核心'
    expected=$(awk '{print $1}' "$PREPARED_DIR/mihomo.sha256")
    verify_asset "$expected" "$PREPARED_DIR/mihomo" || error '预检核心完整性检查失败'
    "$PREPARED_DIR/mihomo" -v || error '预检核心无法运行'
    if [ "$FORCE_CORE_INSTALL" = false ] && [ -x "/usr/local/bin/mihomo" ] && cmp -s "$PREPARED_DIR/mihomo" "/usr/local/bin/mihomo"; then
        info 'Mihomo 核心内容未变化，跳过重复安装'
    else
        install_binary "$PREPARED_DIR/mihomo" mihomo
    fi
    install_binary "$MANAGER_BINARY" mihomo-manager
    if [ -d "$PREPARED_DIR/ui" ]; then
        touch "$PREPARED_DIR/ui-installed"
        [ ! -e ui ] || mv ui "$PREPARED_DIR/previous-ui"
        mv "$PREPARED_DIR/ui" ui
    fi
    sudo mkdir -p /etc/systemd/system
    for unit in mihomo mihomo-manager; do
        rendered=$(mktemp)
        render_service "systemd/$unit.service.in" > "$rendered"
        sudo install -m 0644 "$rendered" "/etc/systemd/system/$unit.service"
        rm -f "$rendered"
    done
    sudo systemctl daemon-reload
    sudo systemctl enable mihomo.service mihomo-manager.service
    info '运行文件安装完成；等待配置切换及运行验收'
fi
