#!/bin/bash
# HTTP 下载的唯一实现：候选来源、独立 DNS、总时限和原子落盘。
# DOWNLOAD_DOH_SERVERS 为以空格分隔的 host:bootstrap-IP；空值禁用外部 DoH。
# 不读取 curlrc，不使用环境 HTTP 代理；透明代理仍由宿主网络规则决定。
# 只转发 curl 的进度帧；原始错误不混入进度条，由统一日志报告错误码。
_http_progress_filter() {
    local frame shown=false allowed='^[[:space:]0-9.#%=O<>-]+$'
    while IFS= read -r -d $'\r' frame || [ -n "$frame" ]; do
        frame="${frame//$'\n'/}"
        case "$frame" in
            *%*|*'#'*|*'=O'*|*'O='*)
                if [[ "$frame" =~ $allowed ]]; then
                    printf '\r%s' "$frame" >&2
                    shown=true
                fi ;;
        esac
    done
    [ "$shown" != true ] || printf '\n' >&2
    return 0
}

_http_transfer() {
    local progress="$1" rc=0
    shift
    if [ "$progress" = true ]; then
        # The pipeline drains progress before returning; PIPESTATUS preserves curl
        # failures even when pipefail is not enabled by the caller.
        if curl --disable --progress-bar "$@" 2>&1 | _http_progress_filter; then
            rc=${PIPESTATUS[0]}
        else
            rc=${PIPESTATUS[0]}
        fi
    else
        if curl --disable --silent "$@" 2>/dev/null; then rc=0; else rc=$?; fi
    fi
    return "$rc"
}

# A routing hint, not cached DNS answers. Cross-process reuse keeps DNS TTLs intact.
_http_remember_dns() (
    umask 077
    local state="$1" resolver="$2" pending
    mkdir -p "$state" && chmod 0700 "$state" || return 1
    pending=$(mktemp "$state/.download-dns.XXXXXX") || return 1
    trap 'rm -f "$pending"' EXIT
    printf '%s\n' "$resolver" > "$pending" && mv -f "$pending" "$state/download-dns"
)

http_download() (
    local output="$1" budget="$2" max_bytes="$3"
    shift 3
    [[ "$budget" =~ ^[1-9][0-9]*$ && "$max_bytes" =~ ^[1-9][0-9]*$ ]] && [ "$#" -gt 0 ] || return 2
    local configured=() resolvers=() modes=(system) resolver host ip
    read -r -a configured <<< "${DOWNLOAD_DOH_SERVERS-dns.alidns.com:223.5.5.5 cloudflare-dns.com:1.1.1.1}"
    # Bash 3.2 + nounset treats an empty array as unset; preserve zero arguments.
    for resolver in ${configured[@]+"${configured[@]}"}; do
        host="${resolver%%:*}"; ip="${resolver#*:}"
        if [[ ! "$host" =~ ^[a-zA-Z0-9.-]+$ || "$resolver" != *:* || ! "$ip" =~ ^[0-9a-fA-F:.]+$ ]]; then
            printf '[下载] DoH 引导配置无效\n' >&2
            return 2
        fi
        case " ${resolvers[*]-} " in *" $resolver "*) ;; *) resolvers+=("$resolver") ;; esac
    done
    local progress=false
    case "${DOWNLOAD_PROGRESS:-auto}" in
        auto) if [ -t "${MIHOMO_DOWNLOAD_TERMINAL_FD:-2}" ] || [ "${MIHOMO_DOWNLOAD_TERMINAL:-0}" = 1 ]; then progress=true; fi ;;
        bar) progress=true ;;
        off) ;;
        *) printf '[下载] DOWNLOAD_PROGRESS 必须为 auto、bar 或 off\n' >&2; return 2 ;;
    esac
    local state="${MIHOMO_SOURCE_DIR:-$PWD}/.update-state" cached="" preferred=""
    if [ -f "$state/download-dns" ]; then IFS= read -r -n 512 cached < "$state/download-dns" || true; fi
    for resolver in system ${resolvers[@]+"${resolvers[@]}"}; do
        if [ "$cached" = "$resolver" ]; then preferred="$cached"; break; fi
    done
    if [ -n "$preferred" ]; then
        modes=("$preferred")
        for resolver in system ${resolvers[@]+"${resolvers[@]}"}; do
            [ "$resolver" = "$preferred" ] || modes+=("$resolver")
        done
    else modes+=(${resolvers[@]+"${resolvers[@]}"})
    fi
    local pending
    pending=$(mktemp "${output}.http.XXXXXX") || return 1
    trap 'rm -f "$pending"' EXIT
    local deadline=$((SECONDS + budget)) attempts_left=$(($# * ${#modes[@]}))
    local remaining attempt_timeout url log_url mode rc bytes redirect_protocols
    local dns_args=() header_args=()
    if [ -n "${HTTP_DOWNLOAD_HEADERS_FILE:-}" ]; then header_args=(--header "@${HTTP_DOWNLOAD_HEADERS_FILE}"); fi
    for url in "$@"; do
        case "$url" in
            https://*) redirect_protocols='=https' ;;
            http://*) redirect_protocols='=http,https' ;;
            *) printf '[下载] 仅支持 HTTP/HTTPS 来源\n' >&2; return 2 ;;
        esac
        # Keep the owner's full URL visible; strip only terminal control characters.
        log_url="${url//[[:cntrl:]]/}"
        printf '[下载] %s（开始）\n' "$log_url" >&2
        # 优先复用已成功的方式；失败后仍尝试其余方式，且不重复优选项。
        for resolver in "${modes[@]}"; do
            remaining=$((deadline - SECONDS))
            [ "$remaining" -gt 0 ] || return 1
            if [ "$resolver" = "$preferred" ]; then
                # A known working route gets the remaining transfer budget. The
                # connection/low-speed limits still fail stale routes promptly.
                attempt_timeout="$remaining"
            else
                attempt_timeout=$((remaining / attempts_left))
            fi
            [ "$attempt_timeout" -gt 0 ] || attempt_timeout=1
            attempts_left=$((attempts_left - 1))
            dns_args=()
            mode="系统 DNS"
            if [ "$resolver" != system ]; then
                host="${resolver%%:*}"; ip="${resolver#*:}"
                [[ "$ip" != *:* ]] || ip="[$ip]"
                dns_args=(--doh-url "https://${host}/dns-query" --resolve "${host}:443:${ip}")
                mode="DoH $host"
            fi
            # libcurl 对重定向后的主机也使用 DoH；两层 TLS 均保持默认校验。
            # 原始错误不混入进度条，失败时统一报告 URL、错误码和解析方式。
            if _http_transfer "$progress" --noproxy '*' --fail --location --max-redirs 5 \
                --proto '=http,https' --proto-redir "$redirect_protocols" --compressed \
                --connect-timeout 5 --max-time "$attempt_timeout" \
                --speed-limit 1024 --speed-time 15 --max-filesize "$max_bytes" \
                ${dns_args[@]+"${dns_args[@]}"} ${header_args[@]+"${header_args[@]}"} --output "$pending" --url "$url"; then
                bytes=$(wc -c < "$pending") || return 1
                if [ "$bytes" -gt 0 ] && [ "$bytes" -le "$max_bytes" ]; then
                    mv -f "$pending" "$output" || return 1
                    printf '[下载] %s（完成，%d 字节）\n' "$log_url" "$bytes" >&2
                    if ! _http_remember_dns "$state" "$resolver" 2>/dev/null; then
                        printf '[下载] %s（警告：DNS 优选记录无法保存）\n' "$log_url" >&2
                    fi
                    return 0
                fi
                rc="文件为空或超过大小限制"
            else
                rc="curl $?"
            fi
            printf '[下载] %s（请求失败，%s；解析：%s）\n' "$log_url" "$rc" "$mode" >&2
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
