# Cloudflare DDNS - 中文终端管理面板

一个轻量级的 Cloudflare 动态 DNS 工具，支持 IPv4 / IPv6、自动同步和 systemd 后台运行。安装后直接输入 `cfddns`，通过中文数字菜单完成全部管理操作。

[![Go checks](https://github.com/Astro-y/Cloudflare-DDNS/actions/workflows/ci.yml/badge.svg)](https://github.com/Astro-y/Cloudflare-DDNS/actions/workflows/ci.yml)

[下载程序](https://github.com/Astro-y/Cloudflare-DDNS/releases) · [使用说明](#安装部署) · [问题反馈](https://github.com/Astro-y/Cloudflare-DDNS/issues) · [测试与验收](TESTING.md)

## 功能特性

### 核心功能

- 🖥️ **中文面板** - 安装、配置、同步、日志和卸载都在菜单内完成，无需记忆管理子命令。
- 🌐 **双栈支持** - 最多管理一条 A 和一条 AAAA，可分别设置域名、IP 来源及启用状态。
- 🔎 **灵活取址** - 支持公网查询、自定义 HTTPS 接口和指定网卡，适应不同网络环境。
- 🔄 **自动同步** - 地址变化时更新，定期核对并修复云端 IP、TTL 和代理状态。
- 🛡️ **记录保护** - 接管前检查多记录及 CNAME 冲突，探测失败时保留原 DNS。
- ⚙️ **后台管理** - 支持 systemd 启停、开机自启、崩溃重启及日志查看。
- 🆕 **程序更新** - 从面板检查最新稳定版本，下载校验后更新，保留配置、凭据和服务状态。
- 🔐 **凭据保护** - Token 隐藏输入、独立保存，面板及日志不显示完整凭据。
- 📦 **单文件部署** - 提供 Linux amd64 / arm64 程序，运行时无需 Go、Python、Node、curl 或 jq。

### 技术特点

- Go 编写，发布文件关闭 CGO，使用标准库及官方 `x/term`、`x/sys`。
- 所有受管理记录固定为 **仅 DNS**，不启用 Cloudflare 小黄云。
- HTTPS 使用系统 CA 并验证服务器证书，无需申请或配置域名证书。
- 原子保存配置，面板和后台共享跨进程锁，避免同步与修改相互覆盖。

## 安装部署

### 方式 1：一键下载安装（推荐）

在 **Linux 交互式 SSH 终端** 中复制执行：

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/Astro-y/Cloudflare-DDNS/main/install.sh)"
```

脚本会自动识别 amd64 / arm64，下载当前发布版 `v0.1.0`，校验 SHA-256，然后打开中文面板。首次使用选择 `1. 安装 / 初始化 DDNS`，按提示完成配置；普通账号会通过 `sudo` 请求管理员权限，root 账号直接运行。

首次下载需要 Bash、curl 和 `sha256sum`，安装完成后程序运行不依赖它们。重新执行该命令会再次进入面板，现有配置由面板保留；日常管理直接输入 `cfddns`（普通账号使用 `sudo cfddns`）。

### 方式 2：手动下载程序

支持 **Linux amd64 / arm64**。从 [Releases](https://github.com/Astro-y/Cloudflare-DDNS/releases) 下载对应架构的程序，并上传到目标主机：

| 架构 | 程序 | 常见设备 |
| --- | --- | --- |
| amd64 | [cfddns-linux-amd64](https://github.com/Astro-y/Cloudflare-DDNS/releases/download/v0.1.0/cfddns-linux-amd64) | 常见 x86_64 服务器、虚拟机 |
| arm64 | [cfddns-linux-arm64](https://github.com/Astro-y/Cloudflare-DDNS/releases/download/v0.1.0/cfddns-linux-arm64) | ARM64 服务器、部分单板电脑 |
| SHA-256 | [SHA256SUMS](https://github.com/Astro-y/Cloudflare-DDNS/releases/download/v0.1.0/SHA256SUMS) | 下载后核对文件完整性 |

下面的命令在目标 **Linux 主机** 的 shell 中执行。amd64 主机：

```sh
chmod +x ./cfddns-linux-amd64
sudo ./cfddns-linux-amd64
```

arm64 主机：

```sh
chmod +x ./cfddns-linux-arm64
sudo ./cfddns-linux-arm64
```

首次运行会直接打开面板，选择 `1. 安装 / 初始化 DDNS`，按提示完成 Token、域名、IP 来源和同步频率设置。向导会在提交前显示最终摘要。

安装完成后，直接输入：

```text
cfddns
```

普通账号读取受保护配置或执行管理操作时，使用 `sudo cfddns`；root 账号可直接运行。请在交互式 SSH 终端操作，程序不接受通过管道输入凭据。

有 systemd 时，首次安装会启用后台服务和开机自启，**退出面板后仍会继续同步**。没有 systemd 时，可从菜单 `8` 进入前台常驻；关闭会话或按 Ctrl+C 会停止运行，不自动适配 NAS 专有任务系统。

### Cloudflare Token 设置

从 **v0.1.1** 起支持用户级和账户级 API Token。两种类型都需要将资源范围限定到要管理的 Zone，并赋予以下权限：

| 权限 | 用途 |
| --- | --- |
| `Zone → Zone → Read` | 读取可访问的 Zone |
| `Zone → DNS → Edit` | 读取、创建和更新 DNS 记录 |

| Token 类型 | Cloudflare 创建位置 | Account ID | 验证接口 |
| --- | --- | --- | --- |
| 用户级 | My Profile → API Tokens | 不需要 | `/user/tokens/verify` |
| 账户级 | Manage Account → Account API Tokens | 需要，32 位十六进制 | `/accounts/{account_id}/tokens/verify` |

在面板中隐藏输入 Token 后，选择对应类型。以 `cfat_` 开头的 Token 默认选择账户级，继续填写它所属账户的 Account ID；这就是 Cloudflare 验证示例中 `/accounts/` 后的 ID，不能填 Zone ID 或 Token ID。程序会按指定账户筛选可访问的 Zone。两种类型的区别见 [Cloudflare 官方说明](https://developers.cloudflare.com/fundamentals/api/get-started/account-owned-tokens/)，账户级验证路径见 [API 文档](https://developers.cloudflare.com/api/resources/accounts/subresources/tokens/methods/verify/)。

Token 输入不回显，只支持 API Token，不支持全局 API Key。本程序验证当前 Token 并管理 DNS，不需要额外授予账户枚举或 `Account API Tokens Read / Write` 等 Token 管理权限。

菜单 `4` 可同时更换 Token 类型、Token 和 Account ID，验证失败保留原凭据。HTTP 401 表示身份验证失败：除类型和验证接口不匹配外，也要检查 Token 是否复制完整、是否过期或撤销，以及 Account ID 是否属于该 Token；补上 ID 并不能修复所有 401。

域名可填写 `@`（根域名）、`home`（单级子域）或 `home.example.com`（完整域名）；多级子域填写完整域名，国际化域名使用 ASCII/Punycode。

已有记录可以接管；同名同类型记录超过一条时必须明确选择，存在 CNAME 时先处理冲突。**接管已开启小黄云的记录会关闭代理**，向导会提前说明并要求确认。

## 面板预览

以下为菜单示意，实际面板还会显示安装状态、服务状态、地址和同步结果：

```text
╭──────────────────────────────────────╮
│          CFDDNS 管理面板              │
╰──────────────────────────────────────╯

 1. 安装 / 初始化 DDNS
 2. 查看详细状态
 3. 修改域名 / 启用或关闭 IPv4、IPv6
 4. 更新 Cloudflare API Token
 5. 设置检查间隔
 6. 设置 IP 来源 / 网卡
 7. 立即同步
 8. 管理后台服务
 9. 查看运行日志
10. 网络与配置诊断
11. 设置 DNS TTL
12. 卸载 DDNS
13. 检查 / 更新程序
 0. 退出
```

普通操作完成后按回车返回菜单；一般输入步骤可用 `q` 取消，Token 输入留空可取消。面板兼容普通 SSH 终端，不依赖颜色、鼠标或全屏 TUI。

## 配置说明

| 设置 | 默认值 / 范围 | 说明 |
| --- | --- | --- |
| 检查间隔 | `60s`，允许 `30s` 至 `24h` | 支持 `5m`、`1h` 或秒数 |
| DNS TTL | `Auto`，或 60 至 86400 秒 | 控制 DNS 缓存时间，与检查间隔独立 |
| 公网查询 | ipify → Cloudflare trace | 按记录类型强制使用 IPv4 / IPv6 |
| 自定义来源 | HTTPS 纯文本 IP 接口 | 优先查询自定义接口，失败后尝试公共备用来源 |
| 网卡来源 | 指定网卡名称 | 每轮重新读取地址，过滤非公网及不可用地址 |
| 代理状态 | 固定仅 DNS | 同步会关闭受管理记录的小黄云 |

公网 IP 查询不使用环境 HTTP 代理，也不会携带 Cloudflare Token。自定义接口 URL 不允许用户名、密码或片段，错误信息不输出查询参数。

网卡模式优先沿用仍有效的上一地址，否则优先非临时 IPv6，并按稳定顺序选择。Linux IPv6 会排除未就绪、DAD 失败、弃用和过期地址。

### 同步与记录保护

- 后台启动立即同步，每轮任务串行处理；单个协议失败不阻止另一个协议处理。
- IP 变化时更新；无变化时每 10 分钟核对云端，修复 IP、TTL 和仅 DNS 状态。
- 已有记录只 PATCH 必要字段，保留备注等无关属性；记录缺失时创建，ID 失效时重新查找。
- 创建请求结果不明确时先查询云端，避免盲目重复创建；API 请求设置超时、响应限制和退避重试，429 遵守服务端重试时间。
- 断网或没有有效公网 IP 时保留旧记录，允许先保存配置并等待恢复。修改域名会保留旧域名记录，旧记录不再自动管理。

> 检测到公网出口不代表入站可达。实际访问还取决于路由器端口映射、防火墙、运营商限制及服务监听；较短检查间隔也不能保证 DNS 缓存立即刷新。

## 服务与数据

修改配置后，运行中的服务会重启应用设置，已停止的服务保持停止。开关自启不改变当前运行状态，重复安装保留已有配置及服务启停、自启状态。

菜单 `2` 默认读取本地状态，可选择只读刷新云端；菜单 `7` 执行同步；菜单 `9` 查看 journald 最近 100 行日志；菜单 `10` 只读检查网络和配置。

### 检查和更新程序

从 **v0.1.1** 起，在面板选择 `13. 检查 / 更新程序`，程序会检查 GitHub 最新稳定版并显示当前版本和目标版本。确认后自动下载对应架构的程序及 SHA-256 校验文件，验证通过后更新已安装程序，保留配置、凭据和原有服务启停、自启状态。

下载或校验失败不会替换当前程序；安装失败会尝试恢复旧程序、配置、凭据和服务状态。更新器本身不发起云端 DNS 写入。更新成功后当前面板退出，重新输入 `cfddns`（普通账号用 `sudo cfddns`）进入新版面板。

**从 v0.1.0 首次升级：**旧面板没有菜单 `13`，请先执行上方一键命令下载 v0.1.1 或更高版本，在新面板选择 `1. 安装 / 初始化 DDNS`，保留现有配置重新安装。也可选择新版面板的菜单 `13`，即使面板已是最新版本，仍可确认重新安装，以替换系统中的旧程序。仅打开下载后的新版面板不会自动完成系统升级；先完成重装或更新，再从菜单 `4` 配置账户级 Token。后续升级直接使用菜单 `13`。

<details>
<summary>本地文件、权限和卸载说明</summary>

| 路径 | 用途 / Linux 权限 |
| --- | --- |
| `/usr/local/bin/cfddns` | 程序，`0755` |
| `/etc/cfddns/config.json` | 带版本号的 JSON 配置，`0600` |
| `/etc/cfddns/token` | 独立凭据文件，包含 Token 和可选 Account ID，`0600` |
| `/etc/cfddns/operation.lock` | 跨进程操作锁，`0600` |
| `/var/lib/cfddns/state.json` | 分协议运行状态，`0600` |
| `/etc/systemd/system/cfddns.service` | systemd 单元，`0644` |

配置及状态目录为 `0700`。Token 不写入服务参数或普通状态；文件通过同目录临时文件、同步和原子替换保存。v0.1.1 将 Token 和 Account ID 一起写入带版本号的 JSON 凭据文件，避免更换凭据时两项不一致；仍兼容 v0.1.0 的纯文本 Token，重新保存时使用新格式，无需手工编辑 `config.json`。本地安装失败会尝试回滚文件及服务状态，首次同步结果单独显示，云端变更不会自动撤销。

卸载请从菜单 `12` 操作，会停止并禁用服务，删除程序和服务文件，**保留云端 DNS 记录**。本地配置、Token 和状态可以选择保留。选择删除时仍保留操作锁及配置目录，确保等待锁的进程继续共享同一把锁。卸载成功后面板退出。

服务管理及日志读取使用系统提供的 `systemctl`、`journalctl`。HTTPS 使用系统 CA，精简系统需先补齐 CA 证书库。程序无须配置域名证书，也不会关闭 HTTPS 证书验证。

</details>

## 从源码构建

需要 Go 1.24 或更高版本，依赖由 `go.mod` / `go.sum` 固定。运行发布程序的目标主机不需要 Go。

### Windows PowerShell 7

在项目根目录执行：

```powershell
go mod download
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
.\scripts\check.ps1
.\scripts\build.ps1 -Version '0.1.0'
```

`check.ps1` 检查格式、测试及 `go vet`；`build.ps1` 默认执行主机测试和静态检查，再关闭 CGO 构建双架构程序，输出到 `dist`，附 `SHA256SUMS`。可用 `-OutputDirectory` 指定目录；已完成测试时可使用 `-SkipTests`。

<details>
<summary>Linux 原生构建与受限环境缓存设置</summary>

以下命令专用于 Linux shell，每一步成功后再继续：

```sh
go mod download
go test -mod=readonly ./...
go vet -mod=readonly ./...
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -ldflags '-s -w -X main.version=0.1.0' -o dist/cfddns-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=readonly -trimpath -ldflags '-s -w -X main.version=0.1.0' -o dist/cfddns-linux-arm64 .
cd dist
sha256sum cfddns-linux-amd64 cfddns-linux-arm64 > SHA256SUMS
```

Linux 已安装 PowerShell 7 时也可运行 `pwsh ./scripts/build.ps1 -Version 0.1.0`。核对下载文件时，Windows 使用 `Get-FileHash -Algorithm SHA256`，Linux 使用 `sha256sum`；下载了两个架构及校验文件后可运行 `sha256sum -c SHA256SUMS`。

受限 Windows 环境若无法写入默认 Go 缓存，可先指定项目内缓存，再运行构建命令：

```powershell
$env:GOPATH = Join-Path $PWD '.cache\gopath'
$env:GOMODCACHE = Join-Path $PWD '.cache\gomod'
$env:GOCACHE = Join-Path $PWD '.cache\go-build'
```

</details>

## 测试与贡献

欢迎通过 [GitHub Issues](https://github.com/Astro-y/Cloudflare-DDNS/issues) 反馈问题或提交 Pull Request。报告问题时请附系统、架构、程序版本和已脱敏日志，勿提交 Token。

自动化测试覆盖同步、IP 筛选、Token 脱敏、原子保存、进程锁、安装回滚和菜单输入。一键脚本检查、Linux 竞态检测、静态检查和双架构构建已通过 [GitHub Actions](https://github.com/Astro-y/Cloudflare-DDNS/actions/runs/42027880426)，公开一键命令也已在 Linux 中验证下载、校验及打开面板。真实 systemd 与 Cloudflare Token 仍待验收，详细记录见 [TESTING.md](TESTING.md)。

## 更新日志

### v0.1.1

- 🆕 新增面板程序更新入口，检查最新稳定版、下载校验并保留配置和服务状态。
- 🔑 支持账户级 API Token，按 Account ID 使用对应验证接口并筛选 Zone。
- 🔐 将 Token 与 Account ID 原子保存，验证失败保留原凭据，兼容已有用户级 Token 配置。
- 🧭 安装与更换 Token 时可选择类型，身份验证失败时提示检查类型及 Account ID。

### v0.1.0

- ✨ 提供单入口中文数字面板及完整安装、配置、日志和卸载流程。
- 🌐 支持独立 IPv4 / IPv6 同步、外部 IP 查询及网卡取址。
- 🔐 提供安全凭据存储、记录冲突检查、配置锁及本地安装回滚。
- 📦 提供 Linux amd64 / arm64 程序和 SHA-256 校验文件。
