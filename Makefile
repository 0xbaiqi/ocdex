# Makefile for CDEX Arbitrage Bot

# 二进制文件名称
BINARY_NAME=ocdex-serv
SCANNER_NAME=ocdex-scanner
NEWCOIN_NAME=ocdex-newcoin

# 编译参数
LDFLAGS=-ldflags "-s -w"

.PHONY: all build build-linux build-scanner build-scanner-linux build-newcoin build-newcoin-linux clean run scan newcoin

# 默认目标
all: build

# 编译本地版本
build:
	@echo "🔨 正在编译 $(BINARY_NAME)..."
	go build $(LDFLAGS) -o $(BINARY_NAME) cmd/oocdex/main.go
	@echo "✅ 编译完成: ./$(BINARY_NAME)"

# 编译 Linux 版本
build-linux:
	@echo "🐧 正在编译 Linux 版本 (amd64) $(BINARY_NAME)..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_NAME) cmd/oocdex/main.go
	@echo "✅ 编译完成: ./$(BINARY_NAME)"

# 编译扫描器
build-scanner:
	@echo "🔍 正在编译扫描器 $(SCANNER_NAME)..."
	go build $(LDFLAGS) -o $(SCANNER_NAME) cmd/scanner/main.go
	@echo "✅ 编译完成: ./$(SCANNER_NAME)"

# 编译扫描器 Linux 版本
build-scanner-linux:
	@echo "🐧 正在编译 Linux 版本 (amd64) $(SCANNER_NAME)..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(SCANNER_NAME) cmd/scanner/main.go
	@echo "✅ 编译完成: ./$(SCANNER_NAME)"

# 编译 NewCoin 工具
build-newcoin:
	@echo "🪙 正在编译 NewCoin 工具 $(NEWCOIN_NAME)..."
	go build $(LDFLAGS) -o $(NEWCOIN_NAME) cmd/newcoin/main.go
	@echo "✅ 编译完成: ./$(NEWCOIN_NAME)"

# 编译 NewCoin Linux 版本
build-newcoin-linux:
	@echo "🐧 正在编译 Linux 版本 (amd64) $(NEWCOIN_NAME)..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(NEWCOIN_NAME) cmd/newcoin/main.go
	@echo "✅ 编译完成: ./$(NEWCOIN_NAME)"

# 清理编译文件
clean:
	@echo "🧹 正在清理..."
	rm -f $(BINARY_NAME)
	rm -f $(SCANNER_NAME)
	rm -f $(NEWCOIN_NAME)
	@echo "✨ 清理完成"

# 运行主程序 (开发环境)
run:
	go run cmd/oocdex/main.go

# 运行扫描器 (开发环境)
scan:
	go run cmd/scanner/main.go

# 运行 NewCoin 工具 (开发环境)
newcoin:
	go run cmd/newcoin/main.go
