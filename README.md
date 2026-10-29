# qswitch

本机多账号配额轮换器：把 Codex / Grok / Cursor / Devin CLI / Kimi Code 的官方登录收进加密仓库，当前号用尽后自动切到下一个有余量的号。 Devin 展示套餐及适用的日/周额度；ZCode 桌面 Coding Plan 支持独立的订阅监控。

硬约束：

- 正在生成不杀。切号时：空闲后结束对应 CLI；关掉并重启 ChatGPT.app / Cursor.app / Grok Bot.app；写入官方登录文件。Cursor 只改 `cursorAuth/*`，不碰聊天和 13GB 状态库。普通 CLI 不自动重启；通过 `qswitch run claude` 启动的受管会话保存工作目录和会话 ID，在受控切号后恢复原对话。
- 关闭桌面前先确认 CLI 可以切换，`--cli-only --kill-cli` 同样必须等待空闲。关闭桌面后若切换失败，会尝试重新打开原应用，并保留切换和重启错误。已排队的耗尽切换若遇到账号恢复、当前账号变化或自动切换关闭，会取消。
- 保活、探测、切号和删除按工具互斥；等待锁后重新读取凭据，避免重复使用轮换的 refresh token。保活临时失败保留已有配额并等待 2 分钟再试；永久失效标记需要重新登录，并使用较长的认证退避。保活与配额查询使用独立退避，查询接口故障不阻断安全续期。daemon 每 2 秒重载配置，关闭自动切换不必重启服务。
- 本地日志采用配额事件的时间，旧事件不会覆盖更新的探测结果或刚切换的账号。没有事件时间的旧格式日志只在尚未探测或切号时作为初始线索。
- 配额接口认证成功但无法按工具协议识别额度时，将历史登录失效改为用量未知；已有明确配额仍保留。网络失败保留已知状态并退避。
- 自动切号只使用最近一次有效的配额观测；历史“正常”不能覆盖后续查询失败。候选号的证据过期时先核验，待切任务执行前也会复查，失败退避期间不反复请求。页面用量条的颜色和百分比只表示已知用量；查询失败、历史记录及记录时间单独说明。当前账号超过查询上限、闲置账号超过 30 分钟，另留一个后台调度周期后，旧记录会隐藏“正常”等状态并标为历史数据；跨过额度重置时间也须重新确认。
- Grok 通过官方 `GET /v1/billing?format=credits` 读取当前共享额度，并显示周/月周期和重置时间。与官方 `/usage` 一致，完整有效的统一计费周期省略百分比时按 0% 处理；空配置、无效周期或错误字段仍未知。只使用当前账期，不把历史用量或按量付费比例当作套餐比例；达到 100% 才判套餐耗尽，99.9% 不提前切号。有可用预付费余额或按量额度时仍保留可用状态。
- Codex/Grok 的 `--cli-only` 不恢复桌面快照。完整恢复先验证快照，桌面写入或最后的 CLI 写入失败时回滚受影响文件；回滚失败会明确报错。这不包含进程或主机突然中断后的自动恢复。
- 查配额按工具适配，不把裸 429 当作用尽。Codex 看会话 JSONL 的 `rate_limits` / `usage_limit_reached`；Grok 看本地 `billing: fetched credits config` 和 HTTP 402 `usage balance exhausted`；Cursor 看 `GetCurrentPeriodUsage` 的 Auto（自家模型）与高级模型；Grok Bot 周额度走 `GetSandUsageStatus`，页面上和 Codex/Grok/Cursor 并列。忽略 `resource_exhausted`（那是容量）。当前账号按消耗速度在默认 5–15 分钟内查询，独立于本地日志更新；闲置账号约每 30 分钟更新，每个工具每轮最多查一个，优先最久未查询的账号，`probe_idle_accounts=false` 可关闭。ChatGPT 用尽号不按显示的重置时间死等（窗口可能提前恢复），大约每 30 分钟点探一次；其他工具耗尽后仍等到显示的重置时间再查。普通查询连续失败按 2、4、8、15 分钟退避，成功后清零；429/503 的有效 `Retry-After` 会作为最短等待时间。`http_backoff` 保留为认证失效的较长退避。
- Cursor 桌面和 cursor-agent 共用同一套 token：`switch cursor` 会把选中账号同时写进 IDE 和 CLI。
- 官方客户端登录第二个号时，`qswitchd` 会自己收进仓库，不必再跑 `qswitch capture`。
- ChatGPT 闲置号会保活。加号请用 `qswitch login codex`：优先拉起本机官方 `codex login --device-auth`（隔离 `CODEX_HOME`，强制 file 存储，不改当前 `auth.json` / 不 logout）。流量、轮询间隔和换票格式与官方 CLI 相同。在 ChatGPT.app 里切换账号等于官方 logout，会作废上一份 refresh。
- Grok 加号同样走 Device Code：`qswitch login grok` 优先拉起本机官方 `grok login --device-auth`（隔离 `GROK_HOME`，不改当前 `auth.json` / 不 logout）。页面加号走同一套 `auth.x.ai` device code。不要在当前 grok 里换号或 `grok logout`。
- 同一登录下的多个 workspace 共用当前 live 的 access token 去查用量；不同邮箱的 refresh_token 按官方 OAuth 刷新写回仓库。换号时若仍是同一 Gmail，会把 live token 合并进目标 workspace。
- Grok 闲置号同样保活：access 大约 6 小时过期，官方 CLI 会在到期前用 `refresh_token` 向 `auth.x.ai` 换票并轮换 refresh。仓库里的闲置号按同一条 OIDC 刷新写回；当前 Grok CLI 运行时由官方客户端续期；CLI 已退出时，qswitch 持有官方 `auth.json.lock` 并重读凭据后续期，轮换结果同时写回登录文件和加密仓库。Grok Bot.app 运行不阻断这项 CLI 保活。Cursor 桌面/CLI 存的是约 60 天的 session JWT，access 与 refresh 是同一张票，没有可安全调用的续期接口，过期或登出作废后只能重新登录。

## 新增订阅

- **ZCode**：读取桌面客户端 `~/.zcode/v2/credentials.json` 中当前 BigModel / Z.ai 个人 Coding Plan，支持官方加密格式。使用官方 `/api/monitor/usage/quota/limit`，显示套餐、5 小时/周模型额度及月度 MCP 额度。MCP 用尽不误判模型额度耗尽；切换套餐后删除旧窗口。当前仅监控订阅，账号在 ZCode 中切换；不修改独立的 ZCode CLI 配置。
- **Kimi Code**：读取 `~/.kimi-code/credentials/kimi-code.json`（支持 `KIMI_CODE_HOME`），用 `/coding/v1/me` 确认账号身份，优先显示接口返回的手机号，缺省时显示昵称或账号 ID；用 `/coding/v1/usages` 展示 5 小时、7 天、月度总额度及编程分项；`usages` 缺少某窗口时从响应的 `limits` 数组按窗口时长补齐（含月度窗口）。月度总额度另从桌面版 Kimi.app 订阅页同款的会员网关读取：只读复用桌面 App localStorage 里的网页会话（要求会话属于同一账号且未过期），不刷新、不轮换桌面登录，桌面 App 未运行或会话过期时自动跳过。编程分项不作为独立耗尽门限。切号只原子替换凭据文件，保留 config/hooks；有 Kimi 会话运行时阻断切换。闲置账号按官方 OAuth 协议续期写入加密仓库；当前 CLI 运行时由官方客户端续期，退出后由 qswitch 持有官方 OAuth 目录锁续期，并同时更新登录文件和加密仓库。续期门限跟随官方客户端，至少预留 5 分钟。身份无法确认时不收录、不抢用 refresh token。
- **Claude Code**：管理 `~/.claude/.credentials.json` 中的官方 OAuth 登录（Linux/macOS 文件布局，支持 `CLAUDE_CONFIG_DIR`；macOS Keychain 存储暂不支持）。收录前用官方 `/api/oauth/profile` 确认账号 uuid、邮箱和套餐（Pro/Max/Team），确认失败不收录。用量走官方状态栏同款 `GET /api/oauth/usage`：5 小时与 7 天窗口决定是否耗尽（达到 100% 才切号），Sonnet/Opus 专项周额度和按量付费余额只展示、不作为切换门限。切号原子替换凭据文件并同步 `~/.claude.json` 的 oauthAccount 显示信息；未受管的 Claude 会话运行时阻断自动切换。refresh token 一次性轮换：闲置账号续期只写加密仓库；当前账号在 CLI 退出后由 qswitch 复核文件内容续期，同时写回登录文件和仓库。

  收录以官方 profile 返回的身份为准，同时修正快照中的账号显示信息；切号也会修正旧快照的显示信息，并在缺少 `.claude.json` 时创建该文件。凭据写入失败会回滚账号元数据，回滚失败单独报错。

  **续期恢复**：换票前在加密仓库记录待恢复事务，收到新凭据后先持久化，再验证身份和写回。profile 临时故障不会反复消费 refresh token；身份未确认的凭据隔离保存，不按旧元数据 UUID 收录。文件写回失败可在重启后恢复；若外部已登录另一个账号，不覆盖新登录。换票结果无法确定时停止复用旧 refresh token，明确要求重新登录。

  **加号（Linux）**：在同一个终端运行一次 `qswitch login claude`。qswitch 在数据目录下创建私有的 `claude-enrollments/session-*` 临时目录，设置 `CLAUDE_CONFIG_DIR` 和 `CLAUDE_SECURESTORAGE_CONFIG_DIR`，运行官方 `claude auth login --claudeai`；打开授权链接并把完整 code 粘贴回当前终端、按回车即可。官方登录成功退出后，qswitch 校验账号身份、写入加密仓库并打印 `enrolled claude ...`，不需要另开终端或手动 `capture`。整个过程不清除、覆盖或登出当前登录，不改变当前账号指针，也不调用新账号的 refresh token。正常退出、Ctrl+C、终端挂断、SIGTERM 或超时会结束直接子进程并清理临时目录；SIGKILL 时由 Linux 结束直接子进程，残留目录在下次使用同一数据目录登录时回收。仅回收带有效标记、属主和权限正确且租约已释放的目录，不扫描旧 `/tmp` 目录，也不结束其他 Claude 会话。身份校验失败不报收录成功，需要重新执行登录。

  **终端输入**：Claude Code 2.1.311 的 `auth login` 在 TTY 输入下会关闭回显，但其按行读取器不显示输入字符；这与普通 `claude` 登录 TUI 不同。qswitch 保留终端正常回显，通过系统 `cat` 子进程把输入送至官方登录程序的 stdin 管道，官方 OAuth、浏览器回调与换票流程保持由 Claude Code 执行。登录结束或取消时一并结束输入转发进程，不在后台等待用户敲回车。

  **macOS 限制**：macOS 上凭据存系统 Keychain，qswitch 暂不支持直接管理。请在 Linux 服务器（如 novix）上运行 qswitch 管理 Claude 账号。

  **tmux 无人值守续接（Claude Code 2.1.312+）**：在项目目录通过 `qswitch run claude` 启动，启用 `qswitch enable-auto --tool claude`，并保持 `qswitchd` 运行。使用默认 OAuth 配置与同一个 qswitch 数据目录；不接受 API key、云厂商、独立凭据目录、无持久化或会绕过受管插件的启动选项。所有共用账号的 Claude 进程都必须受管；普通 `claude` 进程会阻断切换。

  ```sh
  tmux new-session -s work
  qswitch run claude
  # 离开时用 Ctrl+B D detach；tmux 仍保留会话
  qswitch sessions --json
  ```

  daemon 确认当前配额耗尽且候选号可用后，等待所有受管会话就绪，再经官方 `/exit` 退出。所有子进程退出确认后才写凭据，并在原目录用原会话 ID 恢复；仅因 `rate_limit` 中断的会话收到一次继续工作的指令，已完成或用户取消的会话保持等待输入。初次接管历史会话必须显式指定状态：`qswitch run claude --resume-continuation=idle --resume <session-id>`，或对额度中断会话使用 `--resume-continuation=continue`。初次启动不会自动发送续接指令，该状态只用于下一次成功切号。

  tmux 服务器有客户端连接、文本草稿、运行中的工具/代理、待处理输入或已知后台任务时都阻断自动切号；同一服务器的其他连接及链接窗口也算连接。会话内 `/qswitch-status` 可查看具体阻断原因。保留的是已提交对话；官方接口不能保证未提交附件和所有临时弹窗状态。无客户端连接也不是原子输入锁，使用本模式即接受这项无人值守重启边界。

  用户正常 `/exit` 不会被重新拉起。运行器中断、参与者消失或退出状态不确定时停止自动恢复，`qswitch sessions` 显示恢复原因和原会话恢复命令；确认旧进程已退出后再手动恢复。若凭据已写入但 daemon 未能确认提交，不自动发送续接指令。
- **Devin CLI**：按官方 `billingStrategy` 识别订阅额度，Max 仅显示周额度；总体重置时间跟随实际限制窗口。CLI 与 Devin.app 使用独立凭据，CLI 切号不关闭或改写 Devin.app。

首次收录 Kimi 需要先在官方客户端登录；未安装或未登录时保持空状态。

Kimi 的网页、命令行列表与状态优先显示手机号，也可用该手机号或昵称选择账号；若有重复，以稳定账号 ID 为准。

## 数据目录（敏感信息不进 git）

默认全部落在用户主目录下的标准位置，可用环境变量覆盖：

| 用途 | 默认路径 | 覆盖 |
|---|---|---|
| 配置、状态库、加密 vault、锁 | `~/.qswitch/` | `QSWITCH_DIR` |
| 加密账号快照 | `~/.qswitch/vault/<tool>/<id>.enc` | 随 `QSWITCH_DIR` |
| 包装密钥 | macOS Keychain `qswitch.vault` / `wrap-key-v1`；Linux 为 `~/.qswitch/wrap.key`（0600，权限不对会拒绝读取） | — |
| 日志 | `~/Library/Logs/qswitch.log` | launchd plist |
| 生成的 launchd | `~/.qswitch/com.qswitch.plist` | `qswitch init` |

`~/.qswitch/` 权限应为 `0700`。仓库、token、cookie 只出现在上述路径，不要提交。

Keychain 读取失败或密钥格式异常时直接报错，不重新生成或覆盖包装密钥。首次创建若遇到另一进程同时初始化，会回读已有密钥。

## 命令

```
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -o bin/qswitch ./cmd/qswitch
CGO_ENABLED=0 go build -o bin/qswitchd ./cmd/qswitchd

qswitch init
qswitch version
qswitch capture
qswitch login codex
qswitch login grok
qswitch login claude                              # Linux，同终端授权并自动收录
qswitch run claude [Claude arguments...]           # 受管会话，保留真实终端
qswitch sessions [--json]                         # 会话状态与异常恢复提示
qswitch list
qswitch status
qswitch probe [--tool codex|grok|cursor|devin|zcode|kimi|claude]
qswitch switch codex <id-or-email>
qswitch switch cursor <id-or-email>                # 会关掉并重启 Cursor.app
qswitch doctor
qswitch serve [--addr 127.0.0.1:7432]
```

本机页面默认 `http://127.0.0.1:7432`（只绑 loopback）。`qswitchd` 起来后也能开；这时再跑 `qswitch serve` 会打印现成地址然后退出，不会跟 daemon 抢端口。用来看余量、收录当前登录、ChatGPT / Grok Device Code 加号、删除仓库里的号。页面上不会出现 token。

`GET /api/health` 返回服务存活状态和构建版本；配额与账号状态以 `/api/overview` 为准。网页写入要求同源请求，命令行无 Origin 的本地请求仍可使用。

普通 Git 工作区构建自动从 Go 嵌入的 VCS 信息读取提交号，未提交改动带 `-dirty`；发布构建可通过 `-ldflags "-X qswitch/internal/buildinfo.Version=<release>"` 显式指定版本。无 VCS 信息且未指定版本时显示 `dev`。CLI 的 `version`、daemon 的 `--version`、健康接口与页面标题使用同一个版本标识。

页面显示阻塞切换的 PID、待切换目标账号及排队状态。Claude/Kimi 有会话运行时必须先退出会话，仅等待 30 秒空闲不会解除阻塞；排队目标仍需在执行前通过最新用量和安全检查。

公网暴露（仅服务器部署）：`[web] addr` 设为非 loopback 地址时必须同时设置 `[web] token`，否则拒绝启动。此时所有请求要求 token —— 打开 `http://<addr>/?token=<token>` 一次种下 HttpOnly Cookie，之后同浏览器直接访问；脚本可用 `Authorization: Bearer <token>`。Origin 同源校验在 token 之上仍然生效。明文 HTTP 意味着 token 会经过链路，公网环境建议套一层反向代理 TLS。

Cursor 切号会同时写入 IDE 和 CLI 的登录字段。

```
cp ~/.qswitch/com.qswitch.plist ~/Library/LaunchAgents/com.qswitch.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.qswitch.plist
```

launchd 样例见 `contrib/launchd/`（占位符，须用 `qswitch init` 生成带本机路径的 plist）。默认 `KeepAlive=false`，先确认 Keychain 无弹窗。

Linux 服务器（如只跑 Claude Code 的 VPS）用 systemd，样例见 `contrib/systemd/`：

```
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/qswitch-linux ./cmd/qswitch
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/qswitchd-linux ./cmd/qswitchd
install -d -m 0755 ~/.local/bin ~/.config/systemd/user
install -m 0755 bin/qswitch-linux ~/.local/bin/qswitch.next
install -m 0755 bin/qswitchd-linux ~/.local/bin/qswitchd.next
mv ~/.local/bin/qswitch.next ~/.local/bin/qswitch
mv ~/.local/bin/qswitchd.next ~/.local/bin/qswitchd
install -m 0644 contrib/systemd/qswitchd.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable qswitchd
systemctl --user restart qswitchd
~/.local/bin/qswitch version
~/.local/bin/qswitchd --version
```
