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
    echo "未找到.env文件，请先创建.env文件"
    exit 1
fi

# 特殊处理GITHUB_PROXY和GITHUB_API_PROXY，确保末尾有斜杠
if [ -n "$GITHUB_PROXY" ] && [[ "$GITHUB_PROXY" != */ ]]; then
    GITHUB_PROXY="${GITHUB_PROXY}/"
    log_info "已为GITHUB_PROXY添加末尾斜杠: $GITHUB_PROXY"
fi

if [ -n "$GITHUB_API_PROXY" ] && [[ "$GITHUB_API_PROXY" != */ ]]; then
    GITHUB_API_PROXY="${GITHUB_API_PROXY}/"
    log_info "已为GITHUB_API_PROXY添加末尾斜杠: $GITHUB_API_PROXY"
fi

# 配置变量
CN_CIDR_URL="${GITHUB_PROXY}https://raw.githubusercontent.com/herrbischoff/country-ip-blocks/refs/heads/master/ipv4/cn.cidr"
CURRENT_DIR="$(pwd)"
CIDR_FILE="${CURRENT_DIR}/cn_cidr.txt"
CONFIG_FILE="${CURRENT_DIR}/config.yaml"
SERVICE_FILE="/etc/systemd/system/mihomo.service"
ENTRYPOINT_SCRIPT="${CURRENT_DIR}/entrypoint_mihomo.sh"
MANAGER_SERVICE_FILE="/etc/systemd/system/mihomo-manager.service"
GO_MANAGER_BINARY="/usr/local/bin/mihomo-manager"
DNS_LIST="223.5.5.5,1.1.1.1"

# hash缓存文件
CONFIG_HASH_FILE="${CURRENT_DIR}/.config_hash"
CIDR_HASH_FILE="${CURRENT_DIR}/.cidr_hash"
ENV_HASH_FILE="${CURRENT_DIR}/.env_hash"
# 中国IP段列表下载时间戳文件
CIDR_TIMESTAMP_FILE="${CURRENT_DIR}/.cidr_timestamp"
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

# 错误处理函数
handle_error() {
    log_error "$1"
    exit 1
}

# 使用DNS解析下载文件的函数
download_with_dns_resolution() {
    local output_file=$1
    local url=$2
    shift 2
    local extra_args=("$@")
    
    # 从URL中提取主机名（可能包含端口号）
    local host_with_port=$(echo "$url" | sed -E 's|^https?://([^/]+).*|\1|')
    
    # 分离主机名和端口号
    local host=$(echo "$host_with_port" | cut -d: -f1)
    local url_port=$(echo "$host_with_port" | grep -o ':[0-9]\+' | cut -d: -f2)
    
    # 检查主机名是否为IP地址
    if [[ $host =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        log_info "主机名是IP地址，直接使用curl下载"
        curl -o "$output_file" "${extra_args[@]}" "$url"
        return $?
    fi
    
    # 确定URL使用的协议和端口
    local protocol=$(echo "$url" | grep -oP "^https?")
    local port=80
    if [[ "$protocol" == "https" ]]; then
        port=443
    fi
    
    # 如果URL中指定了端口，则使用URL中的端口
    if [ -n "$url_port" ]; then
        port="$url_port"
    fi
    
    # 设置DNS服务器列表
    local dns_servers=()
    local ip=""
    
    # 确保我们获取到的是有效IP地址的函数
    is_valid_ip() {
        local ip=$1
        # 检查是否符合IPv4格式 (x.x.x.x)
        if [[ $ip =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
            return 0
        else
            return 1
        fi
    }
    
    # 如果用户配置了DNS服务器列表，则使用指定的DNS服务器
    if [ -n "$DNS_LIST" ]; then
        # 将DNS_LIST按逗号分割成数组
        IFS=',' read -ra dns_servers <<< "$DNS_LIST"
        log_info "使用自定义DNS服务器: $DNS_LIST"
        
        # 尝试每一个DNS服务器进行解析
        for dns in "${dns_servers[@]}"; do
            log_info "尝试使用DNS服务器 $dns 解析域名 $host..."
            
            # 明确要求A记录并确保获取的是IPv4地址
            local result=$(dig @"$dns" +short "$host" A | grep -v "CNAME" | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1)
            
            # 如果没有直接获取到有效IP，尝试递归查询
            if [ -z "$result" ]; then
                log_info "没有直接获取到A记录，尝试递归查询..."
                # 尝试通过递归方式解析，跟踪CNAME记录直到找到A记录
                result=$(dig @"$dns" +trace "$host" | grep -E "^$host.*IN.*A" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | head -n 1)
            fi
            
            # 如果仍然没有获得结果，尝试最后一种方法
            if [ -z "$result" ]; then
                log_info "尝试全面查询所有记录..."
                result=$(dig @"$dns" "$host" +short | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1)
            fi
            
            if [ -n "$result" ] && is_valid_ip "$result"; then
                ip="$result"
                log_info "使用DNS服务器 $dns 成功解析域名: $host -> $ip"
                break
            else
                log_warn "使用DNS服务器 $dns 解析域名 $host 失败或没有获取到有效IP地址"
            fi
        done
    else
        # 使用系统默认DNS服务器
        log_info "使用系统默认DNS服务器解析域名 $host..."
        
        # 明确要求A记录并确保获取的是IPv4地址
        local result=$(dig +short "$host" A | grep -v "CNAME" | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1)
        
        # 如果没有直接获取到有效IP，尝试递归查询
        if [ -z "$result" ]; then
            log_info "没有直接获取到A记录，尝试递归查询..."
            result=$(dig +trace "$host" | grep -E "^$host.*IN.*A" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | head -n 1)
        fi
        
        # 如果仍然没有获得结果，尝试最后一种方法
        if [ -z "$result" ]; then
            log_info "尝试全面查询所有记录..."
            result=$(dig "$host" +short | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1)
        fi
        
        if [ -n "$result" ] && is_valid_ip "$result"; then
            ip="$result"
            log_info "系统DNS成功解析域名: $host -> $ip"
        fi
    fi
    
    if [ -z "$ip" ]; then
        log_error "无法解析域名 $host 为有效IP地址，尝试直接使用curl下载"
        curl -o "$output_file" "${extra_args[@]}" "$url"
        return $?
    fi
    
    log_info "域名 $host 解析为IP: $ip，端口: $port，使用--resolve参数下载"
    # 使用--resolve参数指定域名解析
    curl -o "$output_file" "${extra_args[@]}" --resolve "${host}:${port}:${ip}" "$url"
    return $?
}

# 检查mihomo命令是否存在
check_mihomo() {
    if ! mihomo -v &> /dev/null; then
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
        chmod +x "$ENTRYPOINT_SCRIPT" || handle_error "设置入口脚本执行权限失败"
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
        tee $SERVICE_FILE > /dev/null << EOF
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
TimeoutStopSec=10
KillMode=process
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
EOF
        
        systemctl daemon-reload || handle_error "systemd重载失败"
        systemctl enable mihomo.service || handle_error "mihomo服务启用失败"
        log_info "mihomo服务安装并启用成功"
    else
        log_info "mihomo服务已安装"
    fi
}

# 检查Go管理器并安装manager服务
check_manager_service() {
    # 检查Go管理器是否存在
    if [ ! -f "$GO_MANAGER_BINARY" ]; then
        handle_error "Go管理器 $GO_MANAGER_BINARY 不存在，请先运行 install.sh 安装"
    fi
    
    # 确保Go管理器有执行权限
    chmod +x "$GO_MANAGER_BINARY" || handle_error "设置Go管理器执行权限失败"
    log_info "Go管理器检查完成: $GO_MANAGER_BINARY"
    
    # 检查manager服务是否已启用
    if ! systemctl --quiet is-enabled mihomo-manager.service 2>/dev/null; then
        log_warn "mihomo-manager服务未安装或未启用，准备安装..."
        log_info "动态创建mihomo-manager服务文件..."
        
        # 创建服务文件内容
        tee $MANAGER_SERVICE_FILE > /dev/null << EOF
[Unit]
Description=Mihomo Manager Service (Go Version)
After=network.target

[Service]
Type=simple
Restart=always
RestartSec=1
WorkingDirectory=${CURRENT_DIR}
ExecStart=${GO_MANAGER_BINARY}
ExecStartPre=/usr/bin/sleep 1s
ExecStop=/bin/kill -SIGTERM \$MAINPID
KillMode=process
KillSignal=SIGTERM
TimeoutStopSec=10

[Install]
WantedBy=multi-user.target
EOF
        
        systemctl daemon-reload || handle_error "systemd重载失败"
        systemctl enable mihomo-manager.service || handle_error "mihomo-manager服务启用失败"
        systemctl start mihomo-manager.service
        log_info "mihomo-manager服务安装并启用成功"
    else
        log_info "mihomo-manager服务已安装"
    fi
}

# 检查并安装mihomo
check_mihomo

# 确保入口脚本有执行权限
ensure_entrypoint_executable

# 检查并安装mihomo服务
check_mihomo_service

# 检查并安装manager服务
check_manager_service

# 下载中国IP段列表
log_info "检查中国IP段列表..."
download_cidr=true

# 检查是否跳过下载中国IP段列表
if [ "${SKIP_CNIP}" = "true" ]; then
    log_info "SKIP_CNIP=true，跳过下载中国IP段列表"
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
    if download_with_dns_resolution "$CIDR_FILE" "$CN_CIDR_URL" -L; then
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

# 下载配置文件
log_info "下载配置文件..."
if download_with_dns_resolution "$CONFIG_FILE" "$CONFIG_URL"; then
    log_info "配置文件下载成功"
else
    if [ -f "$CONFIG_FILE" ]; then
        log_warn "配置文件下载失败，但使用已存在的本地文件继续执行"
    else
        handle_error "无法下载配置文件且本地不存在该文件"
    fi
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

# 检查hash变化决定是否重启mihomo服务
log_info "检查配置文件和IP段文件是否有变化..."

# 计算当前hash
current_config_hash=$(sha256sum "$CONFIG_FILE" | awk '{print $1}')
current_cidr_hash=$(sha256sum "$CIDR_FILE" | awk '{print $1}')
current_env_hash=$(sha256sum ".env" | awk '{print $1}')

# 读取上次hash
last_config_hash=""
last_cidr_hash=""
last_env_hash=""
[ -f "$CONFIG_HASH_FILE" ] && last_config_hash=$(cat "$CONFIG_HASH_FILE")
[ -f "$CIDR_HASH_FILE" ] && last_cidr_hash=$(cat "$CIDR_HASH_FILE")
[ -f "$ENV_HASH_FILE" ] && last_env_hash=$(cat "$ENV_HASH_FILE")

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
if [[ "$current_env_hash" != "$last_env_hash" ]]; then
    log_info ".env文件有变化"
    need_restart=true
else
    log_info ".env文件无变化"
fi

# 保存最新hash
echo "$current_config_hash" > "$CONFIG_HASH_FILE"
echo "$current_cidr_hash" > "$CIDR_HASH_FILE"
echo "$current_env_hash" > "$ENV_HASH_FILE"

if [ "$need_restart" = true ]; then
    log_info "重启mihomo服务..."
    systemctl stop mihomo || true
    
    # 等待mihomo进程完全退出
    while pgrep -x "mihomo" > /dev/null; do
        log_info "等待mihomo进程退出..."
        sleep 1
    done
    
    systemctl start mihomo || handle_error "mihomo服务启动失败"
    log_info "mihomo服务已重启"
else
    log_info "配置和IP段文件均无变化，跳过重启"
fi

log_info "部署完成！"
exit 0
