# 测试与真实环境验收

本项目将自动化单元测试、Linux 交叉编译与真实部署验收分开记录。模拟 API 测试不使用真实 Cloudflare Token，也不更改真实 DNS。编译出 Linux 文件不代表已经在 Linux 或 systemd 主机运行成功。

## 当前验证记录

| 项目 | 当前可确认结果 |
| --- | --- |
| Windows IP 模块测试 | 已通过，包含本地 HTTPS 测试服务器及强制协议测试 |
| Linux amd64 全项目测试及竞态检测 | GitHub Actions 的 Ubuntu runner 实际执行 `go test -race -mod=readonly -coverprofile=coverage.out ./...`，通过 |
| Linux arm64 全项目测试程序 | `CGO_ENABLED=0 go test -mod=readonly -c` 交叉编译通过，未在 Linux 执行 |
| Windows 格式与全项目测试 | 最终源码经 `scripts/check.ps1` 检查通过，`go test -mod=readonly ./...` 通过（5.150 秒） |
| Windows / Linux amd64 / Linux arm64 静态检查 | 三个构建目标的 `go vet -mod=readonly ./...` 均通过 |
| Linux 双架构发布构建与校验文件 | `scripts/build.ps1 -Version 0.1.1` 构建通过；最终源码重建产物位于 `dist`，`CGO_ENABLED=0`，附 `SHA256SUMS` |
| Linux CI 构建 | Ubuntu runner 上的静态检查、PowerShell 双架构构建及产物上传均通过 |
| 真实 systemd、开机自启、异常恢复与卸载 | 无真实 systemd 主机，未验收 |
| 真实 Cloudflare Token 与 Zone | 未提供测试凭据，未验收 |
| 一键入口实际启动 | 公开 `bash -c "$(curl -fsSL …/main/install.sh)"` 命令在 Linux CI 中下载真实 Release、核对 SHA-256 并打开面板后退出，通过；未执行系统安装或 DNS 修改 |
| GitHub Actions | [Go checks #42030554525](https://github.com/Astro-y/Cloudflare-DDNS/actions/runs/42030554525) 已完成，结论 `success`；含 `bash -n`、ShellCheck、Linux 集成测试及公开入口启动验证 |
| v0.1.1 账户级 Token 回归 | Windows 测试及 [Linux CI](https://github.com/Astro-y/Cloudflare-DDNS/actions/runs/42030554525) 通过；验证路由、账户筛选、凭据原子保存和旧版保护均使用模拟服务，未连接用户真实账户 |
| v0.1.1 面板更新回归 | 版本比较、最新正式版查询、下载与摘要校验、保留本地设置及失败回滚的自动化测试和 Linux 竞态检测通过，真实 systemd 更新仍待验收 |

以上记录对应 2026-11-05 的交付。本地构建主机为 Windows，Go 版本为 `go1.25.12 windows/amd64`，发布文件的架构及关闭 CGO 已通过 `go version -m` 核对。上传 GitHub 后，另在 Ubuntu runner 实际执行了 Linux amd64 全项目测试和竞态检测，测试集包含 netlink 地址过滤及 PTY 隐藏输入/中断恢复测试；arm64 测试程序仍仅完成交叉编译。

## 本地检查

在项目根目录的 PowerShell 7 中执行。先安装 Go 1.24 或更高版本并下载依赖：

```powershell
go mod download
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
.\scripts\check.ps1
.\scripts\build.ps1 -Version '0.1.1'
```

`check.ps1` 执行格式检查、`go test -mod=readonly ./...` 和 `go vet -mod=readonly ./...`。`build.ps1` 默认再次运行主机测试和静态检查，并生成 Linux amd64、arm64 的单文件程序及 `SHA256SUMS`。更换代码后应重新运行；无变更时不需要为同一结果反复执行。

交叉编译的发布文件关闭 CGO。Linux 竞态检测需要在 Linux 原生运行，使用支持的架构、C 编译器及启用 CGO 的测试环境。下面仅为 Linux shell 中的命令：

```sh
go test -mod=readonly ./...
CGO_ENABLED=1 go test -race -mod=readonly -coverprofile=coverage.out ./...
go vet -mod=readonly ./...
```

`.github/workflows/ci.yml` 配置了 Linux 竞态测试、静态检查、双架构构建及产物上传。本次实际运行记录见上表链接；后续修改的检查结果以对应提交的 Actions 运行记录为准。

## 自动化测试范围

| 模块 | 测试覆盖 |
| --- | --- |
| Cloudflare 客户端 | 用户级/账户级 Token 验证接口与状态、Account ID 格式拒绝、Zone 按账户过滤及分页、DNS 分页、查询编码、必要字段 PATCH、仅 DNS 创建、429 与取消、网络/5xx 重试、禁止盲目重发 POST、权限错误脱敏、响应上限、阻止重定向、TLS 证书校验 |
| 同步引擎 | 创建缺失记录、无变化不写入、10 分钟核对及修复、ID 失效重新查找、CNAME/多记录冲突、显式选中多记录、创建超时后核对、防重复创建、协议独立失败、无有效 IP 保留旧记录、禁用状态、域名变更和串行同步 |
| IP 来源 | 保留及特殊地址过滤、来源 URL 校验、上一地址沿用、前缀变化后的稳定选择、优先非临时地址、自定义接口备用回退、trace 解析、错误协议拒绝、响应上限、重定向降级拒绝、CA 校验、不使用代理、不带凭据、取消和 URL 查询参数脱敏 |
| Linux IPv6 | netlink 地址消息解析、接口索引、临时标志、tentative/DAD/deprecated 标志、有效及首选生存期；此类测试仅在 Linux 执行 |
| Linux 凭据终端 | PTY 模拟不回显输入、终端设置恢复、取消时清理输入、EOF 和读取前取消；此类测试仅在 Linux 执行 |
| 本地存储 | 带版本 JSON、非法配置拒绝、Token 与 Account ID 成对原子保存、旧纯文本 Token 兼容、非法凭据拒绝与脱敏、原配置保护、域名规范化、符号链接拒绝、并发读取原子保存、锁等待取消、释放及持锁进程退出后恢复 |
| 本地安装 | 注入模拟 systemd 操作，验证初装启用/启动、重装保留停止及自启状态、运行中重启、安装失败回滚文件、原凭据对与原服务状态、非 systemd 安装 |
| 一键启动脚本 | Linux PTY 中模拟下载，验证架构识别、严格 HTTPS、SHA-256 异常拒绝、下载失败、sudo 启动、终端传递、退出码保留及临时文件清理；CI 另执行公开一键命令，下载真实 Release 并打开面板后退出 |
| 面板程序更新 | 检查最新稳定版和版本比较、对应架构资源选择、下载及 SHA-256 校验失败拒绝替换、更新保留配置/凭据/服务状态、安装失败回滚；更新器本身不执行云端 DNS 写入 |
| 面板输入 | 检查间隔边界、未安装提示、空输入/无效菜单/取消/EOF、等待输入时取消、拒绝非终端凭据、Token 类型和 Account ID 选择、凭据验证失败保留原值、多记录选择意图保存、CNAME 冲突、只读云端刷新、非交互命令行提示 |

模拟服务命令可以验证参数和回滚顺序，不能证明实际 systemd 的权限、沙箱、网络命名空间或启动时序正常。不回显输入、真实 SSH 终端信号行为、多网卡拓扑和权限位也需要下面的真实环境检查。

## Linux 部署验收清单

选择可撤销 DNS 变更的测试域名，准备目标 Zone 的 Zone Read、DNS Edit 用户级或账户级 API Token；账户级还需其所属账户的 32 位 Account ID。在独立测试主机记录系统、架构、Go 版本（若执行测试）、程序校验值及每项结果。以下项目在本次交付中均不应默认视为通过。

### 面板、安装与权限

- [ ] 分别在 Linux amd64、arm64 运行对应文件，首次直接打开面板，安装后 `cfddns` 位于预期路径。
- [ ] 普通用户打开时，受保护文件及管理员操作有明确提示；通过 `sudo cfddns` 能完成所有管理操作。
- [ ] 空输入、无效编号、`q`、Token 空输入取消、输入结束及 Ctrl+C 均有可接受行为；取消不提交当前修改。
- [ ] Token 输入不在终端回显，不出现在菜单、状态、错误文本、服务参数或 journald 中。
- [ ] 非交互输入被拒绝；普通 SSH、无颜色环境和窄终端仍能完成各菜单操作。
- [ ] 安装前摘要正确；接管小黄云记录时明确确认；重复安装保留设置与原有启停、自启状态。
- [ ] v0.1.1 面板菜单 `13` 显示当前及目标版本，确认后下载并校验最新稳定版；更新成功退出面板，重新运行 `cfddns` 进入新版。
- [ ] 从 v0.1.0 执行一键命令获取新版，通过菜单 `1` 保留配置重装，随后可从菜单 `4` 设置账户级 Token，并在后续升级中使用菜单 `13`。
- [ ] 人为制造安装失败后，只回滚本地变更，并清楚报告未完成回滚与已发生的云端变更。

### 网络与记录同步

- [ ] 分别验证 IPv4-only、IPv6-only 和双栈；单协议失败不阻止另一个协议成功。
- [ ] 验证默认来源、Cloudflare trace 备用、自定义 HTTPS 主来源及其公共回退；IP 查询不走设置的环境代理。
- [ ] 验证多网卡、网卡临时消失、IPv6 前缀变化、temporary/tentative/deprecated 地址和私网地址筛选。
- [ ] IP 未变化时不写 DNS；达到核对周期后修复云端地址、代理开启和 TTL 偏差，保留备注等其他属性。
- [ ] 删除受管理记录后可以创建；Record ID 失效后可以重新查询；未选择的多记录和 CNAME 冲突阻止更新。
- [ ] 明确选择多记录中的一条后只更新该条；选择的 ID 丢失后不自动接管不明确的其他记录。
- [ ] 断网或来源无有效地址时原 DNS 保留；恢复网络后继续同步；创建结果丢失不会产生重复记录。
- [ ] 修改域名后旧记录保留；关闭协议后不继续更新该协议；立即同步显示两协议各自的结果。
- [ ] 新 Token 无效、缺少 Zone 范围或 DNS 读取权限时保留旧凭据；缺少 DNS Edit 时实际写入给出错误。
- [ ] 分别验证用户级和账户级 Token；账户级使用正确 Account ID 验证并筛选 Zone，类型选错、Account ID 错误、Token 过期或撤销时失败且保留原凭据。
- [ ] 系统 CA 缺失或服务端证书无效时 HTTPS 失败，程序不会关闭证书验证。

### 本地保存、后台与卸载

- [ ] 核对配置/状态目录 `0700`、敏感文件 `0600`，Token 独立保存；服务文件不含 Token。
- [ ] 升级后可读取旧纯文本 Token；重新保存后 Token 与 Account ID 位于同一份 JSON 凭据文件，更换或安装失败后两项同时保留或回滚。
- [ ] 同时打开两个面板并触发后台同步，验证配置修改、手动同步及自动同步通过同一操作锁串行。
- [ ] 修改运行中配置会重启应用；修改已停止服务配置仍保持停止；开关自启不改变当前运行状态。
- [ ] 面板更新保留原配置、凭据与服务启停、自启状态；下载或校验失败不替换旧程序，安装失败恢复旧程序及本地设置。
- [ ] 退出面板后后台继续工作，重启主机后启用的服务自动运行，进程异常终止后自动恢复。
- [ ] 从菜单查看日志，能定位每个协议失败原因，未出现完整 Token 或自定义接口密钥。
- [ ] 无 systemd 环境可从面板进入前台运行，关闭会话后停止；不生成 NAS 专有任务配置。
- [ ] 卸载后服务停止且禁用，程序和服务单元删除，面板退出，云端记录保留。
- [ ] 分别验证保留和删除本地配置；选择删除时保留操作锁及配置目录，已有锁等待者不会换用另一把锁。

完整验收应附实际执行记录、失败项和环境限制，不能将待验证清单勾选结果或 CI 定义当作测试证据。
