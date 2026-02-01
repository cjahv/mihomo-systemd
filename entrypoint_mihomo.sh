#!/usr/bin/env bash
# Reference documentation:
# https://www.kernel.org/doc/Documentation/networking/tproxy.txt
# https://guide.v2fly.org/app/tproxy.html

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
    echo "未找到.env文件，请先创建.env文件"
    exit 1
fi

SKIP_CNIP=${SKIP_CNIP:-true}
QUIC=${QUIC:-true}
LOCAL_LOOPBACK_PROXY=${LOCAL_LOOPBACK_PROXY:-false}

WORK_DIR="$(cd "$(dirname "$0")" && pwd)"

setup_nftables() {
    set -e
    if nft list table clash >/dev/null 2>&1; then
        nft flush table clash
        nft delete table clash
    fi
    # Create a new table
    nft add table clash
    nft add chain clash PREROUTING { type filter hook prerouting priority 0 \; }

    # Skip packets to local/private address
    nft add rule clash PREROUTING ip daddr {0.0.0.0/8, 10.0.0.0/8, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 240.0.0.0/4} return
    # Skip CN IP address
    if [ "$SKIP_CNIP" = "true" ]; then
        CN_IP=$(awk '!/^#/ {ip=ip $1 ", "} END {sub(/, $/, "", ip); print ip}' $WORK_DIR/cn_cidr.txt)
        nft add rule clash PREROUTING ip daddr {$CN_IP} return
    fi

    # Avoid circular redirect
    nft add rule clash PREROUTING mark 0xff return
    # Mark all other packets as 1 and forward to port 7893
    nft add rule clash PREROUTING meta l4proto {tcp, udp} mark set 1 tproxy to :7893 accept

    # DNS
    nft add chain clash PREROUTING_DNS { type nat hook prerouting priority -100 \; }
    nft add rule clash PREROUTING_DNS meta mark 0xff return
    nft add rule clash PREROUTING_DNS udp dport 53 redirect to :1053

    # Disable QUIC (UDP 443)
    if [ "$QUIC" = "false" ]; then
        nft add chain clash INPUT { type filter hook input priority 0 \; }
        nft add rule clash INPUT udp dport 443 reject
    fi

    # Forward local traffic
    if [ "$LOCAL_LOOPBACK_PROXY" = "true" ]; then
        nft add chain clash OUTPUT { type route hook output priority 0 \; }
        nft add rule clash OUTPUT ip daddr {0.0.0.0/8, 10.0.0.0/8, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 240.0.0.0/4} return
        nft add rule clash OUTPUT mark 0xff return
        nft add rule clash OUTPUT meta l4proto {tcp, udp} mark set 1 accept
        # DNS
        nft add chain clash OUTPUT_DNS { type nat hook output priority -100 \; }
        nft add rule clash OUTPUT_DNS meta mark 0xff return
        nft add rule clash OUTPUT_DNS udp dport 53 redirect to :1053
    fi

    # Redirect connected requests to optimize TPROXY performance
    nft add chain clash DIVERT { type filter hook prerouting priority -150 \; }
    nft add rule clash DIVERT meta l4proto tcp socket transparent 1 meta mark set 1 accept
}

if [[ "$SKIP_CNIP" != "true" && "$SKIP_CNIP" != "false" ]]; then
    echo "Error: '\$SKIP_CNIP' Must be 'true' or 'false'."
    exit 1
fi

if [[ "$QUIC" != "true" && "$QUIC" != "false" ]]; then
    echo "Error: '\$QUIC' Must be 'true' or 'false'."
    exit 1
fi

if [[ "$LOCAL_LOOPBACK_PROXY" != "true" && "$LOCAL_LOOPBACK_PROXY" != "false" ]]; then
    echo "Error: '\$LOCAL_LOOPBACK_PROXY' Must be 'true' or 'false'."
    exit 1
fi

# Add policy routing to packets marked as 1 delivered locally
if ! ip rule show | grep -Eq 'fwmark 0x0*1 .* lookup 100'; then
    ip rule add fwmark 1 lookup 100
fi

if ! ip route show table 100 | grep -Eq '^local 0\.0\.0\.0/0 dev lo'; then
    ip route add local 0.0.0.0/0 dev lo table 100
fi

setup_nftables

echo "*** Starting Mihomo ***"

if [ $# -eq 0 ]; then
    exec mihomo -d $WORK_DIR
else
    exec "$@"
fi
