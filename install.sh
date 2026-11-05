#!/usr/bin/env bash
set -euo pipefail
umask 077

# Only bootstrap the verified binary. Configuration and installation stay in
# the Chinese panel so re-running this script preserves the existing workflow.
CFDDNS_RELEASE='v0.1.1'
CFDDNS_REPOSITORY='Astro-y/Cloudflare-DDNS'
work_dir=''
asset=''

fail() {
    printf 'CFDDNS：%s\n' "$*" >&2
    exit 1
}

cleanup() {
    local status=$?
    trap - EXIT
    if [[ -n "$work_dir" && -d "$work_dir" ]]; then
        # Delete only files created here; never recursively remove a path.
        rm -f -- "$work_dir/$asset" "$work_dir/SHA256SUMS" || true
        rmdir -- "$work_dir" 2>/dev/null || true
    fi
    exit "$status"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

download() {
    curl -q --fail --show-error --location \
        --proto '=https' --proto-redir '=https' \
        --connect-timeout 15 --max-time 180 --retry 3 --retry-delay 2 \
        --output "$2" "$1"
}

main() {
    local command_name platform architecture uid base_url expected actual line
    local matches=0 status=0
    for command_name in uname curl sha256sum mktemp chmod rm rmdir id; do
        command -v "$command_name" >/dev/null 2>&1 || fail "缺少 $command_name，请先通过系统包管理器安装。"
    done

    platform="$(uname -s)"
    [[ "$platform" == 'Linux' ]] || fail '仅支持 Linux，请在 Linux SSH 终端中执行此命令。'
    architecture="$(uname -m)"
    case "$architecture" in
        x86_64|amd64) architecture='amd64' ;;
        aarch64|arm64) architecture='arm64' ;;
        *) fail "不支持的架构：$architecture；目前支持 amd64 / arm64。" ;;
    esac
    asset="cfddns-linux-$architecture"

    # bash -c and curl-pipe invocations must hand an actual terminal to the
    # program; a pipe must never be mistaken for credential input.
    if ! { exec 3<>/dev/tty; } 2>/dev/null; then
        fail '需要交互式终端，请通过 SSH 登录后执行；不要在无终端的任务中运行。'
    fi
    [[ -t 3 ]] || fail '当前会话没有可用的交互式终端。'

    uid="$(id -u)"
    [[ "$uid" =~ ^[0-9]+$ ]] || fail '无法确定当前用户权限。'
    if [[ "$uid" != '0' ]]; then
        command -v sudo >/dev/null 2>&1 || fail '当前账号不是 root 且没有 sudo，请切换 root 后重新运行。'
    fi

    work_dir="$(mktemp -d "${TMPDIR:-/tmp}/cfddns.XXXXXXXX")" || fail '无法创建临时下载目录。'
    base_url="https://github.com/$CFDDNS_REPOSITORY/releases/download/$CFDDNS_RELEASE"
    printf 'CFDDNS %s · Linux %s\n正在下载程序和 SHA-256 校验文件……\n' "$CFDDNS_RELEASE" "$architecture"
    download "$base_url/$asset" "$work_dir/$asset" || fail '程序下载失败，请检查到 GitHub Releases 的 HTTPS 连接后重试。'
    download "$base_url/SHA256SUMS" "$work_dir/SHA256SUMS" || fail '校验文件下载失败，未运行程序。'

    expected=''
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line%$'\r'}"
        if [[ "$line" =~ ^([[:xdigit:]]{64})[[:blank:]]+\*?([^[:blank:]]+)$ ]]; then
            if [[ "${BASH_REMATCH[2]}" == "$asset" ]]; then
                matches=$((matches + 1))
                expected="${BASH_REMATCH[1],,}"
            fi
        fi
    done < "$work_dir/SHA256SUMS"
    [[ "$matches" -eq 1 ]] || fail '校验文件没有唯一、有效的对应架构条目，未运行程序。'
    actual="$(sha256sum -- "$work_dir/$asset")" || fail '无法计算程序 SHA-256，未运行程序。'
    actual="${actual%% *}"
    [[ "${actual,,}" == "$expected" ]] || fail 'SHA-256 校验不匹配，未运行程序，请重新下载。'
    chmod 700 -- "$work_dir/$asset"

    printf '校验通过，正在打开中文面板；首次使用请选择 1 安装 / 初始化 DDNS。\n'
    if [[ "$uid" == '0' ]]; then
        if "$work_dir/$asset" <&3 >&3 2>&3; then status=0; else status=$?; fi
    else
        printf '安装和管理需要管理员权限，接下来可能需要输入 sudo 密码。\n'
        if sudo -- "$work_dir/$asset" <&3 >&3 2>&3; then status=0; else status=$?; fi
    fi
    if [[ "$status" -eq 126 ]]; then
        printf '临时目录可能禁止执行程序。请设置 TMPDIR 为可执行的临时目录，或从 Releases 手动下载后运行。\n' >&2
    fi
    return "$status"
}

main
