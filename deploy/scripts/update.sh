#!/bin/bash
#
# 配置更新脚本 - 使用已部署的管理器与运行依赖
#

# 固定工作目录为运行包根目录
cd "$(dirname "$0")/.." || exit 1
umask 077

# 普通入口与 cron 共用 Go 事务；内部阶段仅由持锁的更新器调用。
case "${1:-}" in
    --prepare|--render) MODE="$1"; shift ;;
    *)
        if [ ! -x /usr/local/bin/mihomo-manager ]; then
            echo "缺少已部署的 mihomo-manager，请在开发机运行 mise run publish" >&2
            exit 1
        fi
        exec /usr/local/bin/mihomo-manager update "$@"
        ;;
esac
[ "${MIHOMO_UPDATE_INTERNAL:-}" = "1" ] || { echo "内部阶段不能直接调用" >&2; exit 1; }
STAGING_DIR="$1"
[ -d "$STAGING_DIR" ] || exit 1

source ./lib/env.sh
source ./lib/http.sh

if ! load_env_file ".env"; then
    echo "未找到.env文件，请先创建.env文件"
    exit 1
fi

# 配置变量
CN_CIDR_URL="https://cdn.jsdelivr.net/gh/gaoyifan/china-operator-ip@ip-lists/china.txt"
CURRENT_DIR="$(pwd)"
CIDR_FILE="${STAGING_DIR}/cn_cidr.txt"
CONFIG_FILE="${STAGING_DIR}/config.yaml"
ENTRYPOINT_SCRIPT="${CURRENT_DIR}/scripts/entrypoint.sh"

# 中国IP段列表下载时间戳文件
CIDR_TIMESTAMP_FILE="${STAGING_DIR}/.cidr_timestamp"
# 设定下载间隔为1天（秒数）
DOWNLOAD_INTERVAL=86400

# 配置覆写规则列表 - 格式: "路径=值"
# 添加新的覆写规则只需在此数组中添加新的项
CONFIG_OVERRIDES=(
    "dns.listen=0.0.0.0:1053"
    "bind-address=*"
    "iptables.enable=false"
    "routing-mark=255"
    "external-ui=ui"
    "external-controller=0.0.0.0:9900"
    "secret=${MIHOMO_SECRET}"
    "tproxy-port=7893"
    "mixed-port=7890"
    "port=7895"
    "socks-port=7896"
    "allow-lan=true"
)

# 颜色定义
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
NC='\033[0m' # 无颜色

# 日志函数
log_info() {
    echo -e "${GREEN}[INFO]$(date '+[%Y-%m-%d %H:%M:%S]')${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]$(date '+[%Y-%m-%d %H:%M:%S]')${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]$(date '+[%Y-%m-%d %H:%M:%S]')${NC} $1"
}

# 特殊处理GITHUB_PROXY和GITHUB_API_PROXY，确保末尾有斜杠
if [ -n "$GITHUB_PROXY" ] && [[ "$GITHUB_PROXY" != */ ]]; then
    GITHUB_PROXY="${GITHUB_PROXY}/"
    log_info "已为GITHUB_PROXY添加末尾斜杠"
fi

if [ -n "$GITHUB_API_PROXY" ] && [[ "$GITHUB_API_PROXY" != */ ]]; then
    GITHUB_API_PROXY="${GITHUB_API_PROXY}/"
    log_info "已为GITHUB_API_PROXY添加末尾斜杠"
fi

# 错误处理函数
handle_error() {
    log_error "$1"
    exit 1
}

# 检查mihomo命令是否存在
check_mihomo() {
    mihomo -v &> /dev/null || handle_error "Mihomo 运行依赖缺失或不可用，请在开发机重新执行 mise run publish"
    log_info "mihomo已安装"
}

# 入口与权限由安装器准备；更新流程只检查运行依赖。
check_entrypoint() {
    [ -x "$ENTRYPOINT_SCRIPT" ] || handle_error "入口脚本缺失或不可执行，请从开发机重新发布"
}

if [ "$MODE" = "--prepare" ]; then
    for name in cn_cidr.txt .cidr_timestamp; do
        [ ! -f "${CURRENT_DIR}/$name" ] || cp "${CURRENT_DIR}/$name" "${STAGING_DIR}/$name" || exit 1
    done
    # 检查已部署的mihomo
    check_mihomo

    # 检查已部署的入口脚本
    check_entrypoint

    # 下载中国IP段列表
    log_info "检查中国IP段列表..."
    download_cidr=true

    # 检查是否跳过下载中国IP段列表
    if [ "${SKIP_CNIP}" != "true" ]; then
        log_info "SKIP_CNIP!=true，跳过下载中国IP段列表"
        download_cidr=false
    # 如果不跳过，则检查时间间隔
    elif [ -f "$CIDR_TIMESTAMP_FILE" ]; then
        last_download_time=$(cat "$CIDR_TIMESTAMP_FILE")
        current_time=$(date +%s)
        time_diff=$((current_time - last_download_time))

        if [ $time_diff -lt $DOWNLOAD_INTERVAL ]; then
            log_info "上次下载中国IP段列表时间小于1天，跳过下载"
            download_cidr=false
        else
            log_info "距离上次下载已超过1天，准备重新下载"
        fi
    else
        log_info "未找到下载时间记录，将下载中国IP段列表"
    fi

    if [ "$download_cidr" = true ]; then
        log_info "下载中国IP段列表..."
        if http_download "${CIDR_FILE}.download" 90 16777216 "$CN_CIDR_URL"; then
            mv "${CIDR_FILE}.download" "$CIDR_FILE" || handle_error "保存候选 CIDR 失败"
            log_info "中国IP段列表下载成功"
            # 更新下载时间戳
            date +%s > "$CIDR_TIMESTAMP_FILE"
        else
            if [ -f "$CIDR_FILE" ]; then
                log_warn "中国IP段列表下载失败，但使用已存在的本地文件继续执行"
            else
                handle_error "无法下载中国IP段列表且本地不存在该文件"
            fi
        fi
    else
        log_info "使用已缓存的中国IP段列表"
    fi

    # 下载到独立文件，失败时不会复用部分下载或覆盖活动配置。
    log_info "下载候选订阅..."
    http_download "${STAGING_DIR}/subscription.yaml" 120 16777216 "$CONFIG_URL" || handle_error "订阅下载失败，当前配置保持不变"
    cp "${STAGING_DIR}/subscription.yaml" "$CONFIG_FILE" || handle_error "准备候选配置失败"
else
    cp "$2" "$CONFIG_FILE" || handle_error "准备恢复配置失败"
fi

# 覆写配置文件
log_info "开始覆写配置文件..."

# 检查依赖：yq
if ! command -v yq &> /dev/null; then
    log_error "未找到yq工具，请先安装yq后再运行此脚本"
    exit 1
fi

# 检测yq版本
YQ_VERSION=$(yq --version 2>&1)
if [[ $YQ_VERSION != *"mikefarah"* ]]; then
    log_error "请安装Go版本的yq (https://github.com/mikefarah/yq)，其他版本不受支持"
    exit 1
fi

# 处理所有覆写规则
for override in "${CONFIG_OVERRIDES[@]}"; do
    # 分割路径和值
    path="${override%%=*}"
    value="${override#*=}"

    log_info "覆写配置: $path"

    # 检查值类型并相应处理
    if [[ "$value" == "true" || "$value" == "false" ]]; then
        # 布尔值不加引号
        yq_cmd=".$path = $value"
    elif [[ "$value" =~ ^[0-9]+$ ]]; then
        # 纯数字不加引号
        yq_cmd=".$path = $value"
    else
        # 其他类型作为字符串处理
        escaped_value="${value//\\/\\\\}"
        escaped_value="${escaped_value//\"/\\\"}"
        yq_cmd=".$path = \"$escaped_value\""
    fi

    # 使用Go版本的yq (mikefarah/yq)命令语法
    if yq eval "$yq_cmd" -i "$CONFIG_FILE"; then
        log_info "配置 $path 覆写成功"
    else
        handle_error "配置 $path 覆写失败"
    fi
done

# 校验和提交由 Go 更新事务负责，内部阶段不修改活动文件或 hash。
log_info "候选配置准备完成"
