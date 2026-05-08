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

arm-build:
	GOOS=linux GOARCH=arm GOMIPS=softfloat $(GO) build  -ldflags "-s -w" -o $(PACKAGE_NAME) ./cmd/dns-box/main.go

test:
	$(GO) test ./...

clean:
	rm -rf $(OUTPUT_DIR) *.so *.a *.h


.PHONY: all build arm-build pack copy test clean
