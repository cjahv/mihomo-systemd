#!/bin/bash
# 发布与安装共用的 Linux 目标契约；source 本文件不会执行探测或修改系统。
# 返回值由调用脚本读取。
# shellcheck disable=SC2034
configure_target() {
    local os="$1" machine="$2" kernel="$3"
    TARGET_GOOS=linux
    TARGET_GOARCH=""
    TARGET_GOARM=5,softfloat
    [ "$os" = Linux ] || { echo "仅支持 Linux/systemd，实际系统: $os" >&2; return 1; }
    if [[ ! "$kernel" =~ ^([0-9]+)\.([0-9]+) ]]; then
        echo "无法解析 Linux 内核版本: $kernel" >&2
        return 1
    fi
    if (( BASH_REMATCH[1] < 3 || (BASH_REMATCH[1] == 3 && BASH_REMATCH[2] < 2) )); then
        echo "Go 管理器需要 Linux 内核 3.2 或更新版本: $kernel" >&2
        return 1
    fi
    case "$machine" in
        x86_64|amd64) TARGET_GOARCH=amd64 ;;
        aarch64|arm64) TARGET_GOARCH=arm64 ;;
        armv7l) TARGET_GOARCH=arm; TARGET_GOARM=7,softfloat ;;
        armv6l) TARGET_GOARCH=arm; TARGET_GOARM=6,softfloat ;;
        i386|i486|i586|i686) TARGET_GOARCH=386 ;;
        *) echo "不支持的目标架构: $machine" >&2; return 1 ;;
    esac
}
