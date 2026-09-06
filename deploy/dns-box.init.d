#!/bin/sh /etc/rc.common
# OpenWrt init script for dns-box.
#
# Бинарь живёт в /tmp (быстрый tmpfs, не изнашивает флеш), а постоянная
# копия — в /data: /tmp очищается при ребуте, и без копии роутер, поднявшийся
# без интернета, остался бы без DNS вообще.
#
# Порядок при старте: положить бинарь из /data -> дождаться сети ->
# при AUTO_UPDATE=1 сверить версию с последним релизом и обновить.

# 0 — не ходить за релизами; бинарь берётся из /data через try_local.
# На боевом роутере включено (1) прямо в установленной копии скрипта.
AUTO_UPDATE=0

START=99
USE_PROCD=1

TMPDIR=/tmp/dns-box
PROG=${TMPDIR}/dns-box
CONF=/data/dns-box/config.json
KEEP=/data/dns-box/dns-box

REPO=crazytypewriter/dns-box
REL_URL="https://github.com/${REPO}/releases/latest/download"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"
VER_FILE=${TMPDIR}/version.txt

# Ассет выбираем по архитектуре. Имена задаёт .github/workflows/test.yml;
# отдельно там же публикуется легаси-ассет "dns-box" (копия softfloat) —
# его качают старые версии этого скрипта, ломать имя нельзя.
asset_name() {
    case "$(uname -m)" in
        aarch64|arm64)     echo "dns-box-linux-arm64" ;;
        x86_64|amd64)      echo "dns-box-linux-amd64" ;;
        armv7l|armv6l|arm) echo "dns-box-linux-arm-softfloat" ;;
        *)                 echo "dns-box-linux-arm-softfloat" ;;
    esac
}

wait_for_tmp() {
    while [ ! -d /tmp ]; do
        echo "[dns-box] Waiting for /tmp..."
        sleep 1
    done
    mkdir -p "$TMPDIR"
}

# Восстанавливаем бинарь из /data только если в /tmp его нет: иначе каждый
# restart затирал бы свежескачанный бинарь старой копией.
try_local() {
    [ -x "$PROG" ] && return 0
    [ -x "$KEEP" ] || return 1
    cp "$KEEP" "$PROG" && chmod +x "$PROG"
    echo "[dns-box] Restored binary from ${KEEP}"
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

    BIN_URL="${REL_URL}/$(asset_name)"

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
    else
        echo "[dns-box] dns-box v$local_version (up to date)"
        return 0
    fi

    # Качаем во временный файл и проверяем, что он вообще запускается.
    # Старый вариант писал curl'ом прямо в $PROG: оборванная закачка
    # оставляла битый бинарь, и роутер уходил в respawn-цикл без DNS.
    rm -f "${PROG}.new"
    if command -v curl >/dev/null 2>&1; then
        curl -fL --max-time 60 -o "${PROG}.new" "$BIN_URL" || { rm -f "${PROG}.new"; return 1; }
    elif command -v wget >/dev/null 2>&1; then
        wget -O "${PROG}.new" "$BIN_URL" || { rm -f "${PROG}.new"; return 1; }
    else
        echo "[dns-box] Neither curl nor wget found!"
        return 1
    fi

    chmod +x "${PROG}.new"
    new_version=$("${PROG}.new" -version 2>/dev/null | head -n1 | awk '{print $NF}' | sed 's/^v//')
    if [ -z "$new_version" ]; then
        echo "[dns-box] Downloaded binary is not runnable, keeping current"
        rm -f "${PROG}.new"
        [ -x "$PROG" ] || return 1
        return 0
    fi

    mv "${PROG}.new" "$PROG"
    echo "$new_version" > "$VER_FILE"

    # Обновляем постоянную копию, чтобы следующий ребут без интернета
    # поднялся уже на новой версии.
    # rm перед cp обязателен: прерванный cp оставляет недокачанный огрызок
    # в /data, а сам KEEP так и остаётся от прошлой версии.
    rm -f "${KEEP}.new"
    cp "$PROG" "${KEEP}.new" && mv "${KEEP}.new" "$KEEP" || rm -f "${KEEP}.new"

    echo "[dns-box] Installed dns-box v$new_version"
}

start_service() {
    wait_for_tmp
    try_local
    wait_for_network
    if [ "$AUTO_UPDATE" = "1" ]; then
        download_binary || echo "[dns-box] Update failed, starting existing binary"
    fi

    [ -x "$PROG" ] || { echo "[dns-box] No binary at ${PROG}, refusing to start"; return 1; }

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
    echo "[dns-box] Started (${ver:-unknown})"
}

# stop_service намеренно отсутствует.
#
# Раньше здесь был свой kill -TERM по pid из ps. С procd_set_param respawn
# это не остановка, а перезапуск: procd видит смерть процесса и поднимает
# его заново через пару секунд. Внешне `/etc/init.d/dns-box stop` отрабатывал
# ("Stopped"), сервис возвращался сам, и правка config.json между stop и start
# молча терялась — дальше её затирал уже поднявшийся старый процесс.
#
# procd сам умеет останавливать инстанс и при штатной остановке respawn не
# применяет, поэтому просто не мешаем ему.

service_triggers() {
    procd_add_reload_trigger "dns-box"
}
