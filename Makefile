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

copyConfig:
	ssh be curl 192.168.31.1:8060/config.json  --output /data/dns-box/config.json

copyToRouter:
	ssh be curl 192.168.31.1:8060/$(PACKAGE_NAME) --output /data/dns-box/$(PACKAGE_NAME)

setRights:
	ssh be chmod +x /data/dns-box/$(PACKAGE_NAME)

restart:
	ssh be /etc/init.d/$(PACKAGE_NAME) restart

arm-build:
	GOOS=linux GOARCH=arm GOMIPS=softfloat $(GO) build  -ldflags "-s -w" -o $(PACKAGE_NAME) ./cmd/dns-box/main.go

test:
	$(GO) test ./...

clean:
	rm -rf $(OUTPUT_DIR) *.so *.a *.h


.PHONY: all build arm-build pack copy test clean
