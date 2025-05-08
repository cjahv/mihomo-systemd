# Mihomo Systemd

透明代理（TProxy）部署与管理工具，基于 **Mihomo**（MetaCubeX Clash 核心）。

## 功能

* 下载并安装最新 Mihomo
* 透明代理 (TProxy) 与 NFTables 转发
* 可选跳过中国 IP、启用/禁用 QUIC
* 自动获取中国 IP 段
* 自动生成并管理 systemd 服务
* 可选 cron 定时更新
* 友好日志与错误处理

## 快速开始

### 本地安装

```bash
git clone https://github.com/your-username/mihomo-systemd.git
cd mihomo-systemd && chmod +x auto_task.sh
./auto_task.sh          # 安装 + 配置 + 启动
```

## 定时更新（可选）

```bash
crontab -e
# 每天 01:00 自动更新
0 1 * * * /root/mihomo-systemd/auto_task.sh >> /root/mihomo-systemd/log.txt 2>&1
```

## 许可证

MIT

## 致谢

* [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo)
* [MetaCubeX/metacubexd](https://github.com/MetaCubeX/metacubexd)
