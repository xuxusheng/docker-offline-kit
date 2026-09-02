# docker-offline-kit
# 依赖: Go 1.21+（mise: go 1.26.5），联网
# 国内环境: GOPROXY 已在 Makefile 内设为 goproxy.cn

export PATH := $(HOME)/.local/share/mise/installs/go/1.26.5/bin:$(PATH)
export GOPROXY := https://goproxy.cn,direct
export CGO_ENABLED := 0

.PHONY: pack build test vet tidy vendor clean

## pack: 下载官方二进制并产出 universal 安装器到 release/
pack:
	go run ./cmd/pack

## build: 仅本地架构编译安装器（开发调试，无 payload）
build:
	go build -o dist/installer ./cmd/installer
	go build -o dist/pack ./cmd/pack
	go build -o dist/dok-deploy ./cmd/dok-deploy

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

## vendor: 依赖入库，构建离线可复现
vendor:
	go mod vendor

clean:
	rm -rf dist release
