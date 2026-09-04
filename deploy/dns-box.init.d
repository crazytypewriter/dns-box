#!/bin/sh /etc/rc.common
# OpenWrt init script for dns-box (binary in /tmp)

AUTO_UPDATE=0

START=99
USE_PROCD=1

TMPDIR=/tmp/dns-box
PROG=${TMPDIR}/dns-box
CONF=/data/dns-box/config.json
BIN_URL="https://github.com/crazytypewriter/rd15tool/releases/latest/download/dns-box"
API_URL="https://api.github.com/repos/crazytypewriter/rd15tool/releases/latest"
VER_FILE=${TMPDIR}/version.txt

wait_for_tmp() {
    while [ ! -d /tmp ]; do
        echo "[dns-box] Waiting for /tmp..."
        sleep 1
    done
    mkdir -p "$TMPDIR"
}

wait_for_network() {
    . /lib/functions/network.sh
    network_flush_cache
    network_get_ipaddr ip wan
    while [ -z "$ip" ]; do
        echo "[dns-box] Waiting for network (wan)..."
        sleep 2
        network_flush_cache
        network_get_ipaddr ip wan
    done
    echo "[dns-box] Network is ready: $ip"
}

download_binary() {
    mkdir -p "$TMPDIR"

    latest_version=$(curl -s --max-time 10 "$API_URL" \
        | grep '"tag_name"' | head -n1 \
        | sed 's/.*"tag_name": "\([^"]*\)".*/\1/' \
        | sed 's/^v//')

    local_version=""
    [ -x "$PROG" ] && local_version=$("$PROG" -version 2>/dev/null | head -n1 | awk '{print $NF}' | sed 's/^v//')
    [ -z "$local_version" ] && [ -f "$VER_FILE" ] && local_version=$(cat "$VER_FILE")

    if [ -z "$latest_version" ]; then
        echo "[dns-box] Offline mode. Using local version: ${local_version:-unknown}"
        [ -x "$PROG" ] || { echo "[dns-box] No binary available"; return 1; }
        return 0
    fi

    if [ ! -x "$PROG" ]; then
        echo "[dns-box] Binary not found, downloading v${latest_version}..."
    elif [ "$local_version" != "$latest_version" ]; then
        echo "[dns-box] Updating: ${local_version:-none} -> ${latest_version}"
        rm -f "$PROG"
    else
        echo "[dns-box] dns-box v$local_version (up to date)"
        return 0
    fi

    if command -v curl >/dev/null 2>&1; then
        curl -L --max-time 60 -o "$PROG" "$BIN_URL" || return 1
    elif command -v wget >/dev/null 2>&1; then
        wget --no-check-certificate -O "$PROG" "$BIN_URL" || return 1
    else
        echo "[dns-box] Neither curl nor wget found!"
        return 1
    fi

    chmod +x "$PROG"
    echo "$latest_version" > "$VER_FILE"
    echo "[dns-box] Installed dns-box v$latest_version"
}

start_service() {
    wait_for_tmp
    wait_for_network
    if [ "$AUTO_UPDATE" = "1" ]; then
        download_binary || { echo "[dns-box] Failed to download binary!"; return 1; }
    fi

    procd_open_instance
    procd_set_param command ${PROG} -config ${CONF}
    procd_set_param env DNS_BOX_GITHUB_TOKEN="${DNS_BOX_GITHUB_TOKEN}"
    procd_set_param limits core="unlimited"
    procd_set_param limits nofile="1000000 1000000"
    procd_set_param respawn
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_close_instance

    ver=$("$PROG" -version 2>/dev/null | head -n1 | awk '{print $NF}')
    echo "[dns-box] Started (v${ver:-unknown})"
}

stop_service() {
    pid=$(ps w | grep "$PROG -config" | grep -v grep | awk '{print $1}')
    if [ -n "$pid" ]; then
        kill -TERM "$pid" 2>/dev/null
        echo "[dns-box] Stopped"
    else
        echo "[dns-box] Not running"
    fi
}

service_triggers() {
    procd_add_reload_trigger "dns-box"
}
