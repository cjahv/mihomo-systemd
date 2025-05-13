# Mihomo Systemd

一款轻量级的Mihomo管理工具，基于systemctl实现，提供直观的控制面板及集成式metacubexd界面，简化配置与维护流程。

## 核心功能

* 自动下载并部署最新版Mihomo
* 支持透明代理(TProxy)与NFTables流量转发
* 可直接配置为旁路由网关服务器
* 智能网络控制：可选择性绕过中国IP、灵活启停QUIC协议
* 自动获取并更新中国IP地址段
* 智能生成与管理systemd服务配置
* 支持cron定时自动更新

## 快速部署指南

### 本地安装

#### 环境依赖

运行前请确保系统已安装以下必要组件：

| 组件 | curl | grep | awk | jq | sudo | git | nft |
|------|------|------|-----|----|----- |-----|-----|

Debian/Ubuntu系统安装命令：

```bash
apt update && apt install -y curl jq sudo git nftables
```

CentOS/RHEL/Fedora系统安装命令：

```bash
yum install -y curl jq sudo git nftables
```

#### mikefarah/yq

安装 [mikefarah/yq](https://github.com/mikefarah/yq)：

```bash
wget https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 -O /usr/local/bin/yq &&\
    chmod +x /usr/local/bin/yq
```

#### 部署流程

```bash
git clone https://github.com/cjahv/mihomo-systemd.git
cd mihomo-systemd && chmod +x auto_task.sh
./auto_task.sh          # 一键完成安装、配置与启动

# 部署完成后，直接通过浏览器访问 http://<服务器IP> 进入管理界面
```

#### 配置自动更新（可选）

```bash
crontab -e
# 设置每日凌晨01:00自动更新配置
0 1 * * * /root/mihomo-systemd/auto_task.sh >> /root/mihomo-systemd/log.txt 2>&1
```

## 开源许可

MIT

## 特别鸣谢

* [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo)
* [MetaCubeX/metacubexd](https://github.com/MetaCubeX/metacubexd)

## 系统界面预览

### 管理界面入口
![管理界面入口](image/1.png)

### METACUBEXD面板
![METACUBEXD面板](image/2.png)

### 系统设置
![系统设置](image/3.png)

### 配置更新
![配置更新](image/4.png)
