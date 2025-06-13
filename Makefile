# 应用信息
APP_NAME = mihomo-manager
BINARY_NAME = mihomo-manager

# 构建
.PHONY: build
build:
	@echo "构建 $(APP_NAME)..."
	@go build -ldflags "-w -s" -o $(BINARY_NAME) .
	@echo "构建完成: $(BINARY_NAME)"

# 运行
.PHONY: run
run: build
	@echo "启动服务..."
	@./$(BINARY_NAME)

# 开发模式
.PHONY: dev
dev:
	@echo "开发模式启动..."
	@go run .

# 清理
.PHONY: clean
clean:
	@echo "清理构建文件..."
	@rm -f $(BINARY_NAME)

# 多平台构建
.PHONY: build-all
build-all: clean
	@echo "构建多平台二进制文件..."
	@GOOS=linux GOARCH=amd64 go build -ldflags "-w -s" -o $(BINARY_NAME)-linux-amd64 .
	@GOOS=linux GOARCH=arm64 go build -ldflags "-w -s" -o $(BINARY_NAME)-linux-arm64 .
	@GOOS=darwin GOARCH=amd64 go build -ldflags "-w -s" -o $(BINARY_NAME)-darwin-amd64 .
	@GOOS=darwin GOARCH=arm64 go build -ldflags "-w -s" -o $(BINARY_NAME)-darwin-arm64 .
	@echo "多平台构建完成"

# 安装到系统
.PHONY: install
install: build
	@echo "安装到系统..."
	@sudo cp $(BINARY_NAME) /usr/local/bin/
	@sudo chmod +x /usr/local/bin/$(BINARY_NAME)
	@echo "安装完成，可通过 'mihomo-manager' 命令运行"

# 帮助
.PHONY: help
help:
	@echo "可用的命令："
	@echo "  build      - 构建二进制文件"
	@echo "  run        - 构建并运行"
	@echo "  dev        - 开发模式运行"
	@echo "  clean      - 清理构建文件"
	@echo "  build-all  - 构建多平台二进制文件"
	@echo "  install    - 安装到系统"
	@echo "  help       - 显示此帮助信息" 