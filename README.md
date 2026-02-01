# Mihomo Systemd 管理工具

一款专为Linux系统设计的Mihomo代理服务管理工具，提供完整的systemd集成、直观的Web控制面板以及强大的透明代理功能，让代理服务的部署和管理变得简单高效。

## 项目特色

* **一键部署** - 自动下载并部署最新版Mihomo核心
* **透明代理** - 支持TProxy与NFTables流量转发配置
* **旁路由模式** - 可直接配置为网关服务器，为局域网设备提供代理服务
* **智能分流** - 支持中国IP地址段绕过，灵活控制QUIC协议
* **Web管理界面** - 直观的控制面板，实时查看状态和日志
* **自动更新** - 支持cron定时任务，自动更新配置和IP地址段
* **服务集成** - 完整的systemd服务管理，开机自启动

## 系统要求

运行前请确保系统已安装以下必要组件：

| 组件 | curl | grep | awk | jq | sudo | git | nft |
|------|------|------|-----|----|----- |-----|-----|

### Debian/Ubuntu系统
```bash
apt update && apt install -y curl jq sudo git nftables
```

### CentOS/RHEL/Fedora系统
```bash
yum install -y curl jq sudo git nftables
```

### 安装 yq 工具
```bash
wget https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 -O /usr/local/bin/yq &&\
    chmod +x /usr/local/bin/yq
```

## 快速开始

### 1. 下载项目
```bash
git clone https://github.com/cjahv/mihomo-systemd.git
cd mihomo-systemd
```

### 2. 执行安装
```bash
chmod +x auto_task.sh
./auto_task.sh          # 一键完成安装、配置与启动
```

### 3. 访问管理界面
部署完成后，直接通过浏览器访问：
```
http://<服务器IP>:8000
```

## 核心功能

### Web管理控制台
项目提供了一个功能完整的Web管理界面，包含以下核心功能：

- **实时监控** - 查看代理服务运行状态和连接统计
- **配置管理** - 在线编辑和重载配置文件
- **日志查看** - 实时查看系统日志和错误信息
- **规则更新** - 一键更新代理规则和IP地址库
- **系统设置** - 调整服务参数和网络配置

### 代理模式支持

#### 直连模式
仅对匹配规则的流量进行代理，其他流量直接连接：
- 适合轻量级使用场景
- 系统资源占用低
- 配置简单直观

#### 透明代理模式
拦截所有网络流量并智能分流：
- 无需配置客户端应用
- 支持所有协议和应用
- 自动分流国内外流量

#### 旁路由模式
将服务器配置为局域网代理网关：
- 为整个局域网提供代理服务
- 设备无需单独配置
- 支持混合网络环境

### 智能分流特性

- **中国IP绕过** - 自动识别国内IP地址，直连访问
- **DNS分流** - 智能DNS解析，避免DNS污染
- **规则匹配** - 支持域名、IP、端口等多种匹配规则
- **自定义规则** - 可根据需求添加自定义分流规则

## 高级配置

### 自动更新任务
设置定时任务，自动更新配置和规则：

```bash
crontab -e
# 添加以下行：每日01:00自动更新
0 1 * * * /root/mihomo-systemd/auto_task.sh >> /root/mihomo-systemd/log.txt 2>&1
```

### 旁路由网关配置
将服务器配置为局域网代理网关的步骤：

1. 在Web管理界面中启用透明代理模式
2. 配置客户端设备的网关为服务器IP地址
3. 设置DNS服务器为代理服务器IP（可选）
4. 客户端设备将自动通过代理服务器访问网络

### 防火墙配置
系统会自动配置NFTables规则，如需手动调整：

```bash
# 查看当前规则
nft list table inet mihomo

# 重载规则
systemctl reload mihomo
```

## 技术架构

### 项目结构
```
mihomo-systemd/
├── auto_task.sh         # 主安装和配置脚本
├── entrypoint_mihomo.sh # Mihomo服务启动脚本
├── main.go             # Web管理后端服务
├── ui.html             # Web管理界面前端
├── install.sh          # 系统安装脚本
├── go.mod              # Go模块依赖
└── Makefile            # 构建配置
```

### 核心组件

- **Mihomo核心** - 提供代理服务的核心引擎
- **Systemd服务** - 系统级服务管理和自启动
- **NFTables规则** - 透明代理的流量拦截和转发
- **Go Web服务** - 管理界面的后端API服务
- **Web前端** - 直观的用户操作界面

## 故障排除

### 服务状态检查
```bash
# 检查服务运行状态
systemctl status mihomo

# 查看详细日志
journalctl -u mihomo -f

# 检查配置文件
/opt/mihomo/mihomo -t -d /opt/mihomo
```

### 网络连接问题
```bash
# 检查监听端口
ss -tlnp | grep mihomo

# 测试代理连接
curl -x socks5://127.0.0.1:7890 http://www.google.com
```

### 规则更新问题
```bash
# 手动更新规则
cd /root/mihomo-systemd
./auto_task.sh

# 检查规则文件
ls -la /opt/mihomo/
```

## 安全说明

### 访问控制
- Web管理界面支持密钥验证
- 未设置 MIHOMO_SECRET 时，仅允许本机访问管理服务（127.0.0.1/::1）
- 建议配置防火墙限制管理端口访问
- 定期更新系统和依赖组件

### 网络安全
- 透明代理模式会拦截所有网络流量
- 建议定期检查和更新分流规则
- 监控异常流量和连接

## 性能优化

### 系统调优
```bash
# 增加文件描述符限制
echo "* soft nofile 65536" >> /etc/security/limits.conf
echo "* hard nofile 65536" >> /etc/security/limits.conf

# 优化网络参数
echo "net.core.rmem_max = 16777216" >> /etc/sysctl.conf
echo "net.core.wmem_max = 16777216" >> /etc/sysctl.conf
sysctl -p
```

### 配置优化
- 合理设置并发连接数
- 选择合适的代理协议
- 优化规则匹配顺序

## 开源许可

MIT License

## 相关项目

本项目使用了以下优秀的开源组件：

* [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) - 高性能代理核心
* [MetaCubeX/metacubexd](https://github.com/MetaCubeX/metacubexd) - 现代化管理界面

## 界面展示

### 管理界面主页
![管理界面入口](image/1.png)

### 代理状态面板
![METACUBEXD面板](image/2.png)

## 贡献指南

欢迎提交Issue和Pull Request来改进项目：

1. Fork本项目
2. 创建功能分支
3. 提交更改
4. 发起Pull Request

## 支持

如果您在使用过程中遇到问题：

1. 查看本文档的故障排除章节
2. 搜索现有的Issues
3. 创建新的Issue并提供详细信息
