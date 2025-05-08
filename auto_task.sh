#!/bin/bash
#
# 自动化部署脚本 - 下载所需配置并启动Mihomo服务
#

# 强制切换到脚本所在目录
cd "$(dirname "$0")" || exit 1

# 读取环境变量
if [ -f ".env" ]; then
    source .env
else
    log_error "未找到.env文件，请先创建.env文件"
    exit 1
fi

# 配置变量
CN_CIDR_URL="${GITHUB_PROXY}https://raw.githubusercontent.com/herrbischoff/country-ip-blocks/refs/heads/master/ipv4/cn.cidr"
CURRENT_DIR="$(pwd)"
CIDR_FILE="${CURRENT_DIR}/cn_cidr.txt"
CONFIG_FILE="${CURRENT_DIR}/config.yaml"
SERVICE_FILE="/etc/systemd/system/mihomo.service"
ENTRYPOINT_SCRIPT="${CURRENT_DIR}/entrypoint_mihomo.sh"

# hash缓存文件
CONFIG_HASH_FILE="${CURRENT_DIR}/.config_hash"
CIDR_HASH_FILE="${CURRENT_DIR}/.cidr_hash"

# 配置覆写规则列表 - 格式: "路径=值"
# 添加新的覆写规则只需在此数组中添加新的项
CONFIG_OVERRIDES=(
    "dns.listen=0.0.0.0:1053"
    "dns.ipv6=true"
    "bind-address=*"
    "iptables.enable=false"
    "routing-mark=255"
    "external-ui=ui"
    "external-controller=0.0.0.0:80"
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

# 错误处理函数
handle_error() {
    log_error "$1"
    exit 1
}

# 检查mihomo命令是否存在
check_mihomo() {
    if ! command -v mihomo &> /dev/null; then
        log_warn "mihomo命令未找到，准备安装..."
        chmod +x ./install.sh
        log_info "执行安装脚本..."
        ./install.sh || handle_error "mihomo安装失败"
    else
        log_info "mihomo已安装"
    fi
}

# 确保入口脚本有执行权限
ensure_entrypoint_executable() {
    if [ -f "$ENTRYPOINT_SCRIPT" ]; then
        log_info "确保入口脚本有执行权限..."
        sudo chmod +x "$ENTRYPOINT_SCRIPT" || handle_error "设置入口脚本执行权限失败"
    else
        handle_error "入口脚本 $ENTRYPOINT_SCRIPT 不存在"
    fi
}

# 检查并安装mihomo服务
check_mihomo_service() {
    if ! systemctl --quiet is-enabled mihomo.service 2>/dev/null; then
        log_warn "mihomo服务未安装或未启用，准备安装..."
        log_info "动态创建mihomo服务文件..."
        
        # 创建服务文件内容
        sudo tee $SERVICE_FILE > /dev/null << EOF
[Unit]
Description=mihomo Daemon, Another Clash Kernel.
After=network.target NetworkManager.service systemd-networkd.service iwd.service

[Service]
Type=simple
LimitNPROC=500
LimitNOFILE=1000000
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE CAP_SYS_TIME CAP_SYS_PTRACE CAP_DAC_READ_SEARCH CAP_DAC_OVERRIDE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE CAP_SYS_TIME CAP_SYS_PTRACE CAP_DAC_READ_SEARCH CAP_DAC_OVERRIDE
Restart=always
WorkingDirectory=${CURRENT_DIR}
ExecStartPre=/usr/bin/sleep 1s
ExecStart=${ENTRYPOINT_SCRIPT}
ExecReload=/bin/kill -HUP \$MAINPID

[Install]
WantedBy=multi-user.target
EOF
        
        sudo systemctl daemon-reload || handle_error "systemd重载失败"
        sudo systemctl enable mihomo.service || handle_error "mihomo服务启用失败"
        log_info "mihomo服务安装并启用成功"
    else
        log_info "mihomo服务已安装"
    fi
}

# 1. 检查并安装mihomo
check_mihomo

# 2. 确保入口脚本有执行权限
ensure_entrypoint_executable

# 3. 检查并安装mihomo服务
check_mihomo_service

# 4. 下载中国IP段列表
log_info "下载中国IP段列表..."
if sudo curl -o $CIDR_FILE -L $CN_CIDR_URL; then
    log_info "中国IP段列表下载成功"
else
    if [ -f "$CIDR_FILE" ]; then
        log_warn "中国IP段列表下载失败，但使用已存在的本地文件继续执行"
    else
        handle_error "无法下载中国IP段列表且本地不存在该文件"
    fi
fi

# 5. 下载配置文件
log_info "下载配置文件..."
if sudo curl -o $CONFIG_FILE $CONFIG_URL; then
    log_info "配置文件下载成功"
else
    if [ -f "$CONFIG_FILE" ]; then
        log_warn "配置文件下载失败，但使用已存在的本地文件继续执行"
    else
        handle_error "无法下载配置文件且本地不存在该文件"
    fi
fi

# 5.1 覆写配置文件
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
    path=$(echo $override | cut -d= -f1)
    value=$(echo $override | cut -d= -f2-)
    
    log_info "覆写配置: $path=$value"
    
    # 检查值类型并相应处理
    if [[ "$value" == "true" || "$value" == "false" ]]; then
        # 布尔值不加引号
        yq_cmd=".$path = $value"
    elif [[ "$value" =~ ^[0-9]+$ ]]; then
        # 纯数字不加引号
        yq_cmd=".$path = $value"
    else
        # 其他类型作为字符串处理
        yq_cmd=".$path = \"$value\""
    fi
    
    # 使用Go版本的yq (mikefarah/yq)命令语法
    if yq eval "$yq_cmd" -i $CONFIG_FILE; then
        log_info "配置 $path 覆写成功"
    else
        log_error "配置 $path 覆写失败"
    fi
done

# 6. 检查hash变化决定是否重启mihomo服务
log_info "检查配置文件和IP段文件是否有变化..."

# 计算当前hash
current_config_hash=$(sha256sum "$CONFIG_FILE" | awk '{print $1}')
current_cidr_hash=$(sha256sum "$CIDR_FILE" | awk '{print $1}')

# 读取上次hash
last_config_hash=""
last_cidr_hash=""
[ -f "$CONFIG_HASH_FILE" ] && last_config_hash=$(cat "$CONFIG_HASH_FILE")
[ -f "$CIDR_HASH_FILE" ] && last_cidr_hash=$(cat "$CIDR_HASH_FILE")

need_restart=false
if [[ "$current_config_hash" != "$last_config_hash" ]]; then
    log_info "配置文件有变化"
    need_restart=true
else
    log_info "配置文件无变化"
fi
if [[ "$current_cidr_hash" != "$last_cidr_hash" ]]; then
    log_info "IP段文件有变化"
    need_restart=true
else
    log_info "IP段文件无变化"
fi

# 保存最新hash
echo "$current_config_hash" > "$CONFIG_HASH_FILE"
echo "$current_cidr_hash" > "$CIDR_HASH_FILE"

if [ "$need_restart" = true ]; then
    log_info "重启mihomo服务..."
    sudo systemctl restart mihomo || handle_error "mihomo服务启动失败"
    log_info "mihomo服务已重启"
else
    log_info "配置和IP段文件均无变化，跳过重启"
fi

log_info "部署完成！"
exit 0
