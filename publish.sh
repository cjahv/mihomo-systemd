#!/bin/bash
#
# 发布脚本 - 将Mihomo TProxy Docker部署到远程服务器
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

# 读取环境变量
load_env_file() {
    local env_file="$1"
    if [ ! -f "$env_file" ]; then
        return 1
    fi

    while IFS= read -r line || [ -n "$line" ]; do
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        [ -z "$line" ] && continue
        [[ "$line" == \#* ]] && continue
        local key=""
        local value=""
        if [[ "$line" =~ ^export[[:space:]]+([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
            key="${BASH_REMATCH[1]}"
            value="${BASH_REMATCH[2]}"
        elif [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
            key="${BASH_REMATCH[1]}"
            value="${BASH_REMATCH[2]}"
        fi
        if [ -n "$key" ]; then
            value="${value#"${value%%[![:space:]]*}"}"
            value="${value%"${value##*[![:space:]]}"}"
            if [[ ( "$value" == \"*\" && "$value" == *\" ) || ( "$value" == \'*\' && "$value" == *\' ) ]]; then
                value="${value:1:${#value}-2}"
            fi
            printf -v "$key" '%s' "$value"
            export "$key"
        fi
    done < "$env_file"

    return 0
}

if ! load_env_file ".env"; then
    log_error "未找到.env文件，请先创建.env文件"
    exit 1
fi

# 配置变量
LOCAL_FILES=("auto_task.sh" "install.sh" "entrypoint_mihomo.sh" ".env" "main.go" "go.mod" "ui.html" "Makefile")

remote="${REMOTE_USER}@${REMOTE_HOST}"
REMOTE_DIR_ESCAPED=$(printf "%q" "$REMOTE_DIR")

escape_for_shell() {
    printf "%q" "$1"
}

# 错误处理函数
handle_error() {
    log_error "$1"
    exit 1
}

# 1. 创建远程目录
log_info "创建远程目录: $REMOTE_DIR"
ssh "$remote" "mkdir -p $REMOTE_DIR_ESCAPED" || handle_error "无法创建远程目录"

# 2. 检查远程服务器上是否已存在.env文件
log_info "检查远程服务器上是否已存在.env文件..."
if ssh "$remote" "[ -f $REMOTE_DIR_ESCAPED/.env ]"; then
    log_info "远程服务器上已存在.env文件，将不上传本地.env文件"
    # 从LOCAL_FILES数组中移除.env
    filtered=()
    for file in "${LOCAL_FILES[@]}"; do
        if [ "$file" != ".env" ]; then
            filtered+=("$file")
        fi
    done
    LOCAL_FILES=("${filtered[@]}")
fi

# 3. 复制文件到远程服务器
log_info "复制文件到远程服务器..."
scp "${LOCAL_FILES[@]}" "$remote:$REMOTE_DIR_ESCAPED" || handle_error "文件复制失败"

# 4. 在远程服务器上先安装Go管理器，然后执行部署脚本
log_info "在远程服务器上安装Go管理器..."
# 传递GitHub代理环境变量到远程服务器
REMOTE_ENV_VARS=""
if [ -n "$GITHUB_PROXY" ]; then
    REMOTE_ENV_VARS="GITHUB_PROXY=$(escape_for_shell "$GITHUB_PROXY") "
fi
if [ -n "$GITHUB_API_PROXY" ]; then
    REMOTE_ENV_VARS="${REMOTE_ENV_VARS}GITHUB_API_PROXY=$(escape_for_shell "$GITHUB_API_PROXY") "
fi

ssh "$remote" "cd $REMOTE_DIR_ESCAPED && chmod +x install.sh && ${REMOTE_ENV_VARS}./install.sh"

log_info "在远程服务器上执行部署脚本..."
ssh "$remote" "chmod +x $REMOTE_DIR_ESCAPED/auto_task.sh && $REMOTE_DIR_ESCAPED/auto_task.sh && systemctl restart mihomo-manager.service && journalctl -n 1000 -fu mihomo-manager.service"
exit 0
