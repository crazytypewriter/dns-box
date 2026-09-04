# Go parameters
GO ?= go
PACKAGE_NAME := dns-box

all: build

build: arm-build pack copy copyToRouter setRights restart

pack:
	upx --best --lzma ./$(PACKAGE_NAME)

copy:
	cp ./$(PACKAGE_NAME) /usr/local/var/www/

config:
	cp ./config.json /usr/local/var/www/

#copyConfig:
#	ssh be curl 192.168.31.115:8060/config.json  --output /data/dns-box/config.json

copyToRouter:
	scp -O -o HostKeyAlgorithms=+ssh-rsa ./$(PACKAGE_NAME)  be:/tmp/dns-box/

setRights:
	ssh be chmod +x /tmp/dns-box/$(PACKAGE_NAME)

restart:
	ssh be /etc/init.d/$(PACKAGE_NAME) restart

# VERSION должна совпадать с тегом релиза: init.d-скрипт на роутере
# сравнивает `dns-box -version` с tag_name из GitHub API. В CI её передают
# явно (make arm-build VERSION=v1.2.3), локально берём из git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

arm-build:
	GOOS=linux GOARCH=arm GOMIPS=softfloat $(GO) build -ldflags "$(LDFLAGS)" -o $(PACKAGE_NAME) ./cmd/dns-box/main.go

# Сборка под текущую платформу — с теми же ldflags, чтобы `-version`
# работал и локально.
local-build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(PACKAGE_NAME)-local ./cmd/dns-box

version:
	@echo $(VERSION)

test:
	$(GO) test ./...

clean:
	rm -rf $(OUTPUT_DIR) *.so *.a *.h


.PHONY: all build arm-build local-build pack copy test clean version
