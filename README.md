# Mihomo Systemd 管理工具

一款专为Linux系统设计的Mihomo代理服务管理工具，提供完整的systemd集成、直观的Web控制面板以及透明代理功能，让代理服务的部署和管理变得简单高效。

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

| 组件 | curl | grep | awk | jq | sudo | tar | nft |
|------|------|------|-----|----|----- |-----|-----|

### Debian/Ubuntu系统
```bash
apt update && apt install -y curl jq sudo tar nftables
```

### CentOS/RHEL/Fedora系统
```bash
yum install -y curl jq sudo tar nftables
```

### 安装 yq 工具
```bash
wget https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 -O /usr/local/bin/yq &&\
    chmod +x /usr/local/bin/yq
```

## 快速开始

### 1. 在开发机准备项目与工具链
```bash
git clone https://github.com/cjahv/mihomo-systemd.git
cd mihomo-systemd
# 开发机需先安装 mise：https://mise.jdx.dev/getting-started.html
mise trust
mise install
cp .env.template .env
```

在 `.env` 中填写订阅、管理密钥以及 `REMOTE_USER`、`REMOTE_HOST`、`REMOTE_DIR`。项目使用 Go 1.26.8，`mise.toml` 与 `go.mod` 声明相同版本，`GOTOOLCHAIN=local` 固定使用 mise 提供的编译器。

### 2. 探测、构建与远端部署
```bash
mise run publish -- --build-only  # 仅 SSH 只读探测与本地构建，不上传或修改远端
mise run publish                # 探测、本地构建、上传、安装、更新并重启管理器
mise run publish -- --force      # 保留旧恢复包，跳过未完成发布的自动恢复并重新安装
```

发布脚本先检查远端 Linux 系统、`uname -m` 架构、内核版本、运行中的 systemd 以及必要的传输命令，随后通过 `mise exec` 在本地交叉编译。支持 `amd64`、`arm64`、ARMv6、ARMv7 与 `386`，要求 Linux 内核 3.2 或更新版本；未知架构在上传前拒绝。amd64 使用 `GOAMD64=v1`，arm64 使用 `GOARM64=v8.0`，32 位 ARM 与 x86 使用软件浮点，避免依赖未经探测的 CPU 扩展。产物保存在 `.tmp/mihomo-manager-linux-*`。

生产机接收管理器二进制与运行包，在私有暂存目录先完成下载和候选配置预检。`CGO_ENABLED=0` 使管理器不依赖目标机的 C 工具链或动态 glibc；系统仍需上述运行依赖与 yq。传输后检查 SHA256，并执行无副作用的 `--version` 确认可运行，再安装管理器。替换 `/usr/local/bin/mihomo-manager` 使用同目录临时文件与原子重命名，随后重启服务加载新程序。已有远端 `.env` 保留，仅首次部署上传本地配置。

`publish` 是部署入口。`deploy/install.sh --prepare /absolute/manager` 只准备核心、面板和运行依赖；`--apply` 安装已准备的程序和服务文件。发布流程在预检成功后才执行安装。缺少产物、无法执行或架构不匹配时，安装在系统修改前终止。配置更新与 cron 仅使用已经部署的管理器和 Mihomo，缺少运行依赖时直接报错，由开发机重新发布。

### 本地开发与检查
```bash
mise run build       # 构建本机产物
mise run dev         # 启动开发服务
mise run test        # go test -race ./...
mise run vet         # go vet ./...
mise run build-all   # Linux/macOS 的 amd64、arm64 产物
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
0 1 * * * /root/mihomo/scripts/update.sh >> /root/mihomo/log.txt 2>&1
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
```text
mihomo-systemd/
├── cmd/mihomo-manager/main.go  # 进程入口，仅处理退出码
├── internal/manager/          # 管理 API、更新事务及运行时实现
│   ├── *_test.go              # 与实现同包的测试及可选验收
│   └── web/index.html         # 编译时嵌入的管理页面
├── scripts/publish.sh         # 本地探测、交叉编译、上传与发布
├── deploy/                    # 生产运行包的唯一文件来源
│   ├── install.sh            # 分阶段准备与安装运行包
│   ├── release.sh            # 发布锁、切换、运行验收及失败恢复
│   ├── scripts/update.sh     # 配置更新入口及内部准备阶段
│   ├── scripts/entrypoint.sh # Mihomo 启动与透明代理规则
│   ├── lib/                  # dotenv、目标环境契约及 HTTP 下载
│   └── systemd/*.service.in  # 由安装器渲染的服务模板
├── tests/deployment/          # 独立发布流程与安装器测试
├── docs/images/               # README 截图
├── .env.template              # 配置示例
├── go.mod
└── mise.toml                  # 工具版本及开发、验证、发布任务
```

`cmd` 依赖 `internal/manager`，管理器内部实现不作为公共库暴露。管理页面通过 `go:embed` 编译进二进制，版本与后端一起发布，无需上传独立 HTML，也不依赖启动目录中的页面文件。Mihomo 的 MetaCubeXD 外部面板仍由运行时安装器下载到 `ui/`。

本地发布脚本以仓库根目录为工作目录，运行脚本以运行包根目录为工作目录。生产包由 `deploy/` 与本地编译产物组成；测试、源码和文档不进入生产包。systemd 服务模板只由安装器渲染、安装、启用，配置更新不创建服务。

生产目录（`REMOTE_DIR`）中的 `.env`、`config.yaml`、`cn_cidr.txt`、`.update-state/` 和下载的 `ui/` 是现场配置与状态，发布不覆盖已有 `.env`，不搬迁这些数据。配置更新事务继续管理配置与恢复点。cron 入口使用 `REMOTE_DIR/scripts/update.sh`；目录调整后须更新已有 cron 路径。

### 下载与 DNS 故障恢复

安装和更新统一通过 `deploy/lib/http.sh` 下载。没有成功记录时先使用系统 DNS，失败后依次使用 AliDNS、Cloudflare 的 DNS over HTTPS（DoH，通过 HTTPS 传输 DNS 查询）。成功的解析方式保存在生产目录 `.update-state/download-dns`，后续安装、订阅及 provider 下载跨进程优先复用；优选方式失败时尝试其余方式，新的成功结果替换记录。移除或修改的 DoH 配置自动使旧记录失效。缓存只保存解析方式，不保存域名到 IP 的结果，因此每次请求仍由 curl 正常解析，不固定使用旧 IP。

`DOWNLOAD_PROGRESS=auto` 在交互终端显示 curl 进度条，SSH 发布会传递本地终端状态，远端无 PTY 也能显示。cron 和服务日志默认保持普通文本；设置 `bar` 强制显示，`off` 关闭。已知下载长度时显示百分比，未知长度时显示 curl 的传输活动条；瞬间完成的小文件仍有完成日志，不伪造百分比。进度输出只转发 curl 的进度帧，失败由统一日志报告错误码。

下载日志以当前来源的完整 URL 为主，打印开始和完成字节数；切换来源时显示新的 URL。DNS 优选记录正常复用时保持静默，只有请求失败才在对应 URL 后附上解析方式和错误码。项目所有者的公开下载、订阅及 provider 地址统一完整显示，包括路径、查询参数、账号密码和片段，不做脱敏。日志只去除终端控制字符以保持单行，实际请求地址及 header 不变。

DoH 服务自身由固定 IP 引导，因此不依赖 `/etc/resolv.conf`，也不发送可能再次被 Mihomo 重定向的 UDP 53 查询。curl 的原生 DoH 同时解析重定向后的域名，目标站点与 DoH 服务均保留 HTTPS 证书校验；需要支持 DoH 的 curl（7.62.0 或更新版本）。

GitHub API、核心发布包和 MetaCubeXD 面板在配置的代理前缀失败后尝试官方来源；订阅只请求原始地址，避免将私有订阅发送到其他下载服务。各来源和 DNS 尝试共用总时限，临时文件下载成功后才替换目标。下载不读取 `.curlrc`，不使用环境中的 HTTP 代理；宿主机透明代理仍可能影响 HTTPS，网络完全中断时 DoH 也无法恢复连接。

`DOWNLOAD_DOH_SERVERS` 在未配置时默认使用 `dns.alidns.com:223.5.5.5 cloudflare-dns.com:1.1.1.1`，格式为以空格分隔的 `host:bootstrap-IP`，支持 IPv4/IPv6 引导地址，服务路径为 `/dns-query`。客户内网可指定自有 DoH 并安装受信任的根证书；设置空值禁用外部 DoH。DoH 只发送待解析域名，不包含订阅路径或令牌。

版本查询或核心下载全部失败时，安装器仅在 `mihomo -v` 已成功的情况下保留原核心并继续，日志明确说明本次未更新核心。首次安装或现有核心不可执行则终止，不宣称安装成功。现有面板可直接使用；首次面板下载失败仍须修复网络或离线提供 `ui/`。订阅下载失败沿用原有事务行为，活动配置保持不变；CIDR 下载失败可使用已有缓存。

### 发布切换与失败恢复

发布期间持有与配置更新、设置保存相同的进程锁。暂存包保留远端环境配置，优先检查服务使用的 `/usr/local/bin/mihomo -v`，并查询最新稳定发布版本；普通发布在版本相同时复制现有核心用于配置预检，跳过核心包下载。安装阶段再比较预检核心与目标文件内容，相同且可执行时保留原文件，跳过重复安装；无法解析版本时按正常下载流程处理。需要更新时准备 amd64-v1（其他架构按明确目标匹配）的 gzip 核心，使用 GitHub 所选资产的 `digest` 强制校验 SHA256，再用待安装核心预检旧配置和候选配置。没有校验值的远端包不会安装；网络故障时可以保留当前已验证可运行的核心。

项目直接管理 `/usr/local/bin/mihomo` 与 `/usr/local/bin/mihomo-manager`，通过同目录临时文件和重命名替换，不调用 deb/rpm 安装器。原有系统包不参与新发布的核心选择；程序版本以运行中的可执行文件为准。安装前保存运行脚本、服务文件、状态及旧进程实际持有的可执行文件，以处理之前已经出现的“磁盘新版本、进程旧版本”。配置更新在发布时强制重启核心，随后检查 Google、provider 初始化和管理器 HTTP 就绪，并比对进程与安装文件的 inode。

安装或运行验收失败时，发布流程停止服务，恢复程序、配置和依赖快照，再恢复原来的服务启用/运行状态。旧核心原来处于运行状态时，恢复后检查控制接口与 provider 初始化；发布前 Google 可达时，还要求恢复该可达性。恢复失败保留私有恢复包并返回失败；中断记录位于 `.update-state/deployment.pending`，下次发布先恢复未完成切换，普通配置更新在完成发布恢复之前拒绝执行。尚未完成的配置更新则须先通过 `mihomo-manager update --recover-only` 恢复，发布预检不覆盖它。安装前重新核对活动文件，发现手动变化时终止。安装日志只报告安装阶段完成，整个发布只有在运行验收结束后才报告成功。传输使用不包含 macOS 扩展属性的 ustar 归档。

旧恢复包包含损坏的 unit 等情况可能使普通发布反复停在自动恢复阶段。此时使用 `mise run publish -- --force`，保留旧包在原路径，跳过旧发布的恢复，按当前现场状态重新预检、备份并安装核心、管理器、脚本及服务文件；即使核心版本相同也重新下载可验证发布包并安装。网络故障时仍可沿用经过运行验证的旧核心，日志会明确说明。强制模式仍保留发布锁、SHA256 校验、配置预检及运行验收，也不会跳过未完成配置更新的门禁。预检失败时旧恢复记录不变；安装后失败则恢复本次安装前状态，恢复成功后重新指向原恢复包，恢复失败则保留本次恢复包及其 `previous-deployment.pending` 对原包的引用。成功后清除发布门禁，旧包仍保留供排查。`--force` 与 `--build-only` 不能组合。

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
/usr/local/bin/mihomo -t -d /root/mihomo
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
cd /root/mihomo
./scripts/update.sh

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
![管理界面入口](docs/images/1.png)

### 代理状态面板
![METACUBEXD面板](docs/images/2.png)

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

## 配置更新验收与自动回滚

网页“更新”、保存设置后更新以及 `scripts/update.sh` 共用同一个配置更新事务。

1. 通过本机 `127.0.0.1:7890` mixed-port 检查 `https://www.google.com/generate_204`，更新前检测窗口为 5 秒。期望 HTTPS 204，保留证书验证，不接受重定向。
2. 当前 Google 可达且磁盘配置与运行进程对应时，将当前配置确认为可用恢复点。更新前不可达时仍允许更新修复问题，但不会覆盖已有的可用恢复点。
3. 下载到私有候选目录，完成本地覆写，准备 HTTP/file provider 缓存，并进行 `mihomo -t` 原生配置预检。原生 `-t` 检查配置结构与引用；运行就绪阶段另检查 provider 成功初始化。
4. 切换 provider 文件之前停止核心，避免后台刷新与写入竞争。配置、CIDR 和 provider 字节共同保存在事务快照中。
5. 重启 Mihomo，最多等待 15 秒就绪。控制 API、代理端口与新进程就绪后，开始独立的 **5 秒 Google 验收窗口**，策略选择恢复、请求和重试共用截止时间。
6. 新配置成功后才确认版本及写入已应用 hash；加载或验收失败则恢复本地旧订阅，重新应用当前 `.env` 的本地覆写，并检查恢复结果。

5 秒是新配置就绪后的检测和触发回滚窗口，不包括下载、启动和回滚耗时。Google 检测验证服务器通过当前 Mihomo 配置的访问路径；局域网 DNS、TProxy 和 DNS 泄露仍须单独验收。

HTTP、file、inline 类型的 rule/proxy provider 均支持。候选 provider 路径统一归入 `.update-state/providers/`，HTTP 类型的 URL、header、interval 和运行时自动更新语义保持不变。预取使用独立 DNS 和宿主网络，不调用候选配置中尚未启动的具名代理（`proxy`）；来源须在该网络可达，或具有相同来源及选项的现有缓存以供下载失败时复用。文件来源必须位于部署目录内，不允许符号链接、越界或覆盖程序和事务文件；单文件上限 16 MiB，快照资源总量上限 64 MiB。回滚使用本地快照，不依赖再次下载；恢复启动后，HTTP provider 继续按其原有 interval 刷新。

管理器外层界面使用与 MetaCubeXD 默认 `sunset` 主题一致的深色表面、暖橙色强调和内嵌 Tabler 图标；不依赖在线字体或图标 CDN。品牌使用独立 Logo，操作按钮通过鼠标悬停、键盘聚焦及可访问名称说明用途；页面只保留标题、字段名称与实时状态。密钥、设置、日志连接及更新结果统一使用公共浮动提示组件：同来源提示替换、最多同时显示 3 条、自动消退或手动关闭，鼠标悬停、键盘聚焦及展开详情时暂停计时。更新按钮位于日志页，进度仅在日志页观察正在执行的任务时显示；结束后关闭进度并提示一次结果，历史已结束任务不会在刷新或切页时重放。此样式对齐默认主题，跨端口嵌入的面板切换主题不会自动同步到外层。

网页日志主视图展示 `mihomo.service` 的日志，按完整事件倒序排列，最新日志在顶部；单条多行日志保持原有顺序。后端解析 Mihomo 的 Logrus 日志格式，提取应用时间、等级和解码后的消息，支持普通 `time=… level=… msg=…` 格式及 ANSI 彩色格式；无法识别或损坏的记录保留原文与 journal 时间，不伪造等级。前端按紧凑等宽的时间、等级、消息列呈现，不重复打印日志包装字段，不使用逐条卡片或分隔线。进入日志页读取最近 1000 条并持续跟随，离开页面或退出时关闭连接。页面最多保留 1000 条且约 200 万字符的消息文本（单条大消息保留完整），阅读旧日志时保留滚动位置。更新进度独立于核心日志，终态诊断放在浮动提示的可展开详情中。`GET /logs` 使用 NDJSON 事件流，包含 `type`、`message` 和可选的 `time`、`level`；journal 诊断使用 `error` 类型，由全局提示组件显示。

日志区域的原生选中复制输出纯文本：时间、等级和消息之间使用空格，不同记录之间使用换行；消息原有的换行、缩进和选中的部分字符保持不变。仅在整个选区位于日志区域内时应用此格式，跨出日志区域的选区及其他页面内容沿用浏览器默认复制行为。

网页通过后台任务展示阶段及结果，关闭页面不会取消事务，刷新后可重新查看。`POST /reload` 返回 HTTP 202 和任务 ID，`GET /update_status` 返回当前/最近一次任务。旧的 `GET /reload` 入口保留，但同样返回任务 JSON，不再输出脚本日志流。详细准备日志可查看 `journalctl -u mihomo-manager`；cron 输出仍写入其重定向日志。 更新脚本、Mihomo 原生校验、YAML 解析、provider 下载和服务操作失败时，任务消息保留命令失败原因及末尾最多 32 KiB 的诊断输出；超出上限会明确标注截断。原始脚本输出仍写入管理器 journal，页面消息去除 ANSI 颜色码。回滚完成后的最终状态同时保留触发更新失败的原因。

最终结果包括 `updated`、`unchanged`、`rejected`、`rolled_back` 和 `recovery_failed`。回滚成功仍代表本次更新失败；恢复文件后 Google 仍不可达会明确报告，避免反复切换新旧配置。

### 命令行与 cron

```bash
./scripts/update.sh                    # 自动更新语义，同一失败候选冷却 5 分钟
./scripts/update.sh --retry            # 人工立即重试
mihomo-manager update --recover-only  # 在项目工作目录中恢复未完成事务
```

退出码：`0` 更新成功/无变化，`1` 准备或校验拒绝，`2` 更新失败并执行恢复（含恢复异常），`3` 更新锁被占用。Google 不可达时的人工修复更新不受基线失败阻断。

### 恢复点与适用范围

`.update-state/` 保存私有恢复点、事务记录和任务状态，目录权限为 0700、文件为 0600；这些文件包含订阅配置，禁止提交或公开。配置快照原子替换并同步磁盘，管理器启动及下一次更新会恢复未确认的事务。快照损坏或恢复文件写入失败时停止更新并保留事务记录供检查，不删除恢复点。

恢复以订阅 YAML 为主；同次更新修改的 `cn_cidr.txt` 及下载时间戳也恢复到更新前，保证透明代理规则重新启动时使用原数据。`.env`、核心二进制和项目代码不回滚。原有手动选择策略尽量按名称保留；新配置已删除的组或节点采用新配置默认选择。

HTTP provider 按缓存文件的修改时间与 `interval`（秒）判断是否过期；同名称、同来源及内容选项的未过期缓存直接复用，仅缺失、过期或来源变化时下载，最多并行 4 个任务。仅调整 `interval` 或缓存路径时，按新的周期判断有效期，不因此重新下载。`interval: 0` 或省略该项沿用 Mihomo 的“不定期刷新”语义，有可用同来源缓存时不重复下载。下载失败可使用同来源的过期缓存，但不会刷新其时间；任务取消或整体超时不会被缓存兜底转换为成功。所有资源只写入候选目录，完整准备与校验通过后才切换配置；任一无法兜底的失败会取消其余下载，并等待下载进程退出。日志保留 URL，同时区分缓存命中与过期缓存兜底。

恢复点同时保存配置、其引用的 provider 文件及更新时间；暂存复制、发布安装和回滚保留原修改时间，避免把旧资源误认为刚更新。过期资源重新下载后即使内容相同，也记录新的更新时间，无需为此重启核心。旧恢复记录缺少更新时间时按未知时间恢复，不伪造新的下载时间。内嵌节点、规则及 HTTP/file provider 都进入上述准备和验收流程。初次安装没有旧配置时，验收失败会撤销候选并停止 Mihomo；已有未验证旧配置时保留其备份，恢复后通过 Google 检测才确认为可用版本。

运行 `mise run test`、`mise run vet` 验证事务和接口，Linux 构建可用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 mise exec -- go build -o .tmp/mihomo-manager-linux-amd64 ./cmd/mihomo-manager`。真实 systemd、nftables 和 Google 网络验收需在 Linux 测试服务器进行。

可选真实内核验收：指定 `MIHOMO_TEST_BINARY` 与 `YQ_TEST_BINARY` 为本机二进制绝对路径，再执行 `mise run test`。该测试通过真实 Mihomo 访问本地 HTTPS 测试端点，不修改防火墙或访问公网。

可选浏览器验收：设置 `MIHOMO_UI_ACCEPTANCE=1`、`MIHOMO_UI_ACCEPTANCE_STOP` 为一个尚不存在的停止标记文件绝对路径，运行 `mise exec -- go test ./internal/manager -run '^TestUIAcceptanceServer$' -v -timeout 4m`，打开打印的本机 URL。使用测试密钥 `example`，点击更新并在过程中刷新；预期 5 秒检测失败后显示回滚成功。确认结果后创建停止标记文件，测试会核对旧配置恢复并退出。此测试使用临时文件和受控运行时，不操作真实系统服务。设置 `MIHOMO_UI_ACCEPTANCE_FAILURE=1` 可改为验证准备脚本失败：预期更新结束后关闭进度，并以浮动提示报告更新被拒绝；展开提示详情可查看完整测试 URL、curl 错误和脚本失败消息。
