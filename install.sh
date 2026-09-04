#!/bin/sh
# Установка dns-box на Linux-роутер (OpenWrt/Keenetic и т.п.).
# Качает бинарь из последнего релиза GitHub, кладёт init-скрипт.
#
# Использование (запускать на роутере):
#   curl -fsSL https://raw.githubusercontent.com/crazytypewriter/dns-box/main/install.sh | sh
#
# Переменные окружения:
#   DNS_BOX_DIR   — куда класть бинарь (дефолт /tmp/dns-box)
#   DNS_BOX_ARCH  — суффикс артефакта релиза (дефолт linux-arm-softfloat;
#                   для arm64 — linux-arm64, для x86_64 — linux-amd64)
#   DNS_BOX_REPO  — owner/repo (дефолт crazytypewriter/dns-box)

set -eu

DNS_BOX_DIR="${DNS_BOX_DIR:-/tmp/dns-box}"
DNS_BOX_ARCH="${DNS_BOX_ARCH:-linux-arm-softfloat}"
DNS_BOX_REPO="${DNS_BOX_REPO:-crazytypewriter/dns-box}"

REPO_URL="https://github.com/${DNS_BOX_REPO}"

echo ">> dns-box installer"
echo "   repo:  ${REPO_URL}"
echo "   arch:  ${DNS_BOX_ARCH}"
echo "   dir:   ${DNS_BOX_DIR}"

# Требуем root для init.d и прав
if [ "$(id -u)" != "0" ]; then
    echo "!! Запустите от root (su / sudo)" >&2
    exit 1
fi

mkdir -p "${DNS_BOX_DIR}"

# Последний тег релиза
TAG=$(curl -fsSL "https://api.github.com/repos/${DNS_BOX_REPO}/releases/latest" \
    | grep -o '"tag_name": *"[^"]*"' | head -1 | sed 's/.*"tag_name": *"//;s/"//')
if [ -z "${TAG}" ]; then
    echo "!! Не удалось узнать последний релиз" >&2
    exit 1
fi
echo ">> Устанавливаю ${TAG}"

BIN_URL="${REPO_URL}/releases/download/${TAG}/dns-box-${DNS_BOX_ARCH}"
if ! curl -fsSL -o "${DNS_BOX_DIR}/dns-box.new" "${BIN_URL}"; then
    echo "!! Не удалось скачать ${BIN_URL}" >&2
    echo "   Проверьте DNS_BOX_ARCH: linux-arm-softfloat / linux-arm64 / linux-amd64" >&2
    exit 1
fi

chmod +x "${DNS_BOX_DIR}/dns-box.new"

# Атомарная замена: старый бинарь продолжает работать, пока не перезапустим
[ -f "${DNS_BOX_DIR}/dns-box" ] && mv "${DNS_BOX_DIR}/dns-box" "${DNS_BOX_DIR}/dns-box.old"
mv "${DNS_BOX_DIR}/dns-box.new" "${DNS_BOX_DIR}/dns-box"

echo ">> Установлено: $(${DNS_BOX_DIR}/dns-box -version 2>/dev/null || echo "${TAG}")"

# init-скрипт, если его ещё нет
if [ ! -f /etc/init.d/dns-box ]; then
    echo ">> Качаю init-скрипт"
    curl -fsSL -o /etc/init.d/dns-box \
        "${REPO_URL}/raw/main/deploy/dns-box.init.d" \
        || echo "!! init-скрипт не скачался — положите deploy/dns-box.init.d вручную" >&2
    chmod +x /etc/init.d/dns-box 2>/dev/null || true
fi

echo ">> Готово."
echo "   Конфиг: ${DNS_BOX_DIR}/config.json (или правьте /etc/init.d/dns-box)"
echo "   Запуск: /etc/init.d/dns-box restart"
echo "   Не забудьте DNS_BOX_GITHUB_TOKEN, если используете GitHub-бэкап."
