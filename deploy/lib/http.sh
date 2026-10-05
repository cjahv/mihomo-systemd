#!/bin/bash
# HTTP 下载的唯一实现：候选来源、独立 DNS、总时限和原子落盘。
# DOWNLOAD_DOH_SERVERS 为以空格分隔的 host:bootstrap-IP；空值禁用外部 DoH。
# 不读取 curlrc，不使用环境 HTTP 代理；透明代理仍由宿主网络规则决定。
http_download() (
    local output="$1" budget="$2" max_bytes="$3"
    shift 3
    [[ "$budget" =~ ^[1-9][0-9]*$ && "$max_bytes" =~ ^[1-9][0-9]*$ ]] && [ "$#" -gt 0 ] || return 2
    local resolvers=() resolver host ip
    read -r -a resolvers <<< "${DOWNLOAD_DOH_SERVERS-dns.alidns.com:223.5.5.5 cloudflare-dns.com:1.1.1.1}"
    for resolver in "${resolvers[@]}"; do
        host="${resolver%%:*}"; ip="${resolver#*:}"
        if [[ ! "$host" =~ ^[a-zA-Z0-9.-]+$ || "$resolver" != *:* || ! "$ip" =~ ^[0-9a-fA-F:.]+$ ]]; then
            printf '[下载] DoH 引导配置无效\n' >&2
            return 2
        fi
    done
    local pending
    pending=$(mktemp "${output}.http.XXXXXX") || return 1
    trap 'rm -f "$pending"' EXIT
    local deadline=$((SECONDS + budget)) attempts_left=$(($# * (${#resolvers[@]} + 1)))
    local remaining attempt_timeout url source=0 mode rc bytes redirect_protocols
    local dns_args=()
    for url in "$@"; do
        case "$url" in
            https://*) redirect_protocols='=https' ;;
            http://*) redirect_protocols='=http,https' ;;
            *) printf '[下载] 仅支持 HTTP/HTTPS 来源\n' >&2; return 2 ;;
        esac
        source=$((source + 1))
        # 空字符串表示系统解析；DoH 主机本身通过 --resolve 固定引导。
        for resolver in "" "${resolvers[@]}"; do
            remaining=$((deadline - SECONDS))
            [ "$remaining" -gt 0 ] || return 1
            attempt_timeout=$((remaining / attempts_left))
            [ "$attempt_timeout" -gt 0 ] || attempt_timeout=1
            attempts_left=$((attempts_left - 1))
            dns_args=()
            mode="系统 DNS"
            if [ -n "$resolver" ]; then
                host="${resolver%%:*}"; ip="${resolver#*:}"
                [[ "$ip" != *:* ]] || ip="[$ip]"
                dns_args=(--doh-url "https://${host}/dns-query" --resolve "${host}:443:${ip}")
                mode="DoH $host"
            fi
            # libcurl 对重定向后的主机也使用 DoH；两层 TLS 均保持默认校验。
            # 隐藏原始错误文本，避免订阅 URL 的凭据进入日志。
            if curl --disable --noproxy '*' --fail --silent --location --max-redirs 5 \
                --proto '=http,https' --proto-redir "$redirect_protocols" --compressed \
                --connect-timeout 5 --max-time "$attempt_timeout" \
                --speed-limit 1024 --speed-time 15 --max-filesize "$max_bytes" \
                "${dns_args[@]}" --output "$pending" --url "$url" 2>/dev/null; then
                bytes=$(wc -c < "$pending") || return 1
                if [ "$bytes" -gt 0 ] && [ "$bytes" -le "$max_bytes" ]; then
                    mv -f "$pending" "$output" || return 1
                    if [ "$source" -gt 1 ] || [ -n "$resolver" ]; then
                        printf '[下载] 来源 %s，%s 下载成功\n' "$source" "$mode" >&2
                    fi
                    return 0
                fi
                rc="文件为空或超过大小限制"
            else
                rc="curl $?"
            fi
            printf '[下载] 来源 %s，%s 失败（%s）\n' "$source" "$mode" "$rc" >&2
        done
    done
    return 1
)

# 代理前缀仅适用于公开的 GitHub URL；订阅不切换来源。
github_download() {
    local output="$1" budget="$2" max_bytes="$3" prefix="$4" url="$5"
    if [ -n "$prefix" ]; then
        http_download "$output" "$budget" "$max_bytes" "${prefix%/}/$url" "$url"
    else
        http_download "$output" "$budget" "$max_bytes" "$url"
    fi
}
