# qswitch

本机多账号配额轮换器：把 Codex / Grok / Cursor / Devin CLI / Kimi Code 的官方登录收进加密仓库，当前号用尽后自动切到下一个有余量的号。 Devin 展示套餐及适用的日/周额度；ZCode 桌面 Coding Plan 支持独立的订阅监控。

硬约束：

- 正在生成不杀。切号时：空闲后结束对应 CLI；关掉并重启 ChatGPT.app / Cursor.app / Grok Bot.app；写入官方登录文件。Cursor 只改 `cursorAuth/*`，不碰聊天和 13GB 状态库。不会擅自再拉起一条新的 CLI 会话（没有工作目录），新开的 CLI 才会用新号。
- 关闭桌面前先确认 CLI 可以切换，`--cli-only --kill-cli` 同样必须等待空闲。关闭桌面后若切换失败，会尝试重新打开原应用，并保留切换和重启错误。已排队的耗尽切换若遇到账号恢复、当前账号变化或自动切换关闭，会取消。
- 保活、探测、切号和删除按工具互斥；等待锁后重新读取凭据，避免重复使用轮换的 refresh token。保活临时失败保留已有配额，永久失效标记需要重新登录，两者均退避。daemon 每 2 秒重载配置，关闭自动切换不必重启服务。
- 本地日志采用配额事件的时间，旧事件不会覆盖更新的探测结果或刚切换的账号。没有事件时间的旧格式日志只在尚未探测或切号时作为初始线索。
- 配额接口认证成功但无法按工具协议识别额度时，将历史登录失效改为用量未知；已有明确配额仍保留。网络失败保留已知状态并退避。
- Grok 通过官方 `GET /v1/billing?format=credits` 读取当前共享额度，并显示周/月周期和重置时间。与官方 `/usage` 一致，完整有效的统一计费周期省略百分比时按 0% 处理；空配置、无效周期或错误字段仍未知。只使用当前账期，不把历史用量或按量付费比例当作套餐比例；达到 100% 才判套餐耗尽，99.9% 不提前切号。有可用预付费余额或按量额度时仍保留可用状态。
- Codex/Grok 的 `--cli-only` 不恢复桌面快照。完整恢复先验证快照，桌面写入或最后的 CLI 写入失败时回滚受影响文件；回滚失败会明确报错。这不包含进程或主机突然中断后的自动恢复。
- 查配额按工具适配，不把裸 429 当作用尽。Codex 看会话 JSONL 的 `rate_limits` / `usage_limit_reached`；Grok 看本地 `billing: fetched credits config` 和 HTTP 402 `usage balance exhausted`；Cursor 看 `GetCurrentPeriodUsage` 的 Auto（自家模型）与高级模型；Grok Bot 周额度走 `GetSandUsageStatus`，页面上和 Codex/Grok/Cursor 并列。忽略 `resource_exhausted`（那是容量）。平时只探当前号。ChatGPT 用尽号不按显示的重置时间死等（窗口可能提前恢复），大约每 30 分钟点探一次；Grok/Cursor 仍等到显示的重置时间再查。不把空闲号当心跳扫。HTTP 间隔按消耗速度估「还能用多久」。探测失败退避 2 小时。
- Cursor 桌面和 cursor-agent 共用同一套 token：`switch cursor` 会把选中账号同时写进 IDE 和 CLI。
- 官方客户端登录第二个号时，`qswitchd` 会自己收进仓库，不必再跑 `qswitch capture`。
- ChatGPT 闲置号会保活。加号请用 `qswitch login codex`：优先拉起本机官方 `codex login --device-auth`（隔离 `CODEX_HOME`，强制 file 存储，不改当前 `auth.json` / 不 logout）。流量、轮询间隔和换票格式与官方 CLI 相同。在 ChatGPT.app 里切换账号等于官方 logout，会作废上一份 refresh。
- Grok 加号同样走 Device Code：`qswitch login grok` 优先拉起本机官方 `grok login --device-auth`（隔离 `GROK_HOME`，不改当前 `auth.json` / 不 logout）。页面加号走同一套 `auth.x.ai` device code。不要在当前 grok 里换号或 `grok logout`。
- 同一登录下的多个 workspace 共用当前 live 的 access token 去查用量；不同邮箱的 refresh_token 按官方 OAuth 刷新写回仓库。换号时若仍是同一 Gmail，会把 live token 合并进目标 workspace。
- Grok 闲置号同样保活：access 大约 6 小时过期，官方 CLI 会在到期前用 `refresh_token` 向 `auth.x.ai` 换票并轮换 refresh。仓库里的闲置号按同一条 OIDC 刷新写回；当前 live 会话不抢 refresh（避免和 CLI 双花）。Cursor 桌面/CLI 存的是约 60 天的 session JWT，access 与 refresh 是同一张票，没有可安全调用的续期接口，过期或登出作废后只能重新登录。

## 新增订阅

- **ZCode**：读取桌面客户端 `~/.zcode/v2/credentials.json` 中当前 BigModel / Z.ai 个人 Coding Plan，支持官方加密格式。使用官方 `/api/monitor/usage/quota/limit`，显示套餐、5 小时/周模型额度及月度 MCP 额度。MCP 用尽不误判模型额度耗尽；切换套餐后删除旧窗口。当前仅监控订阅，账号在 ZCode 中切换；不修改独立的 ZCode CLI 配置。
- **Kimi Code**：读取 `~/.kimi-code/credentials/kimi-code.json`（支持 `KIMI_CODE_HOME`），用 `/coding/v1/me` 确认账号身份，优先显示接口返回的手机号，缺省时显示昵称或账号 ID；用 `/coding/v1/usages` 展示 5 小时、7 天、月度总额度及编程分项。编程分项不作为独立耗尽门限。切号只原子替换凭据文件，保留 config/hooks；有 Kimi 会话运行时阻断切换。闲置账号按官方 OAuth 协议续期写入加密仓库，当前登录由官方客户端续期。身份无法确认时不收录、不抢用 refresh token。
- **Devin CLI**：按官方 `billingStrategy` 识别订阅额度，Max 仅显示周额度；总体重置时间跟随实际限制窗口。CLI 与 Devin.app 使用独立凭据，CLI 切号不关闭或改写 Devin.app。

首次收录 Kimi 需要先在官方客户端登录；未安装或未登录时保持空状态。

## 数据目录（敏感信息不进 git）

默认全部落在用户主目录下的标准位置，可用环境变量覆盖：

| 用途 | 默认路径 | 覆盖 |
|---|---|---|
| 配置、状态库、加密 vault、锁 | `~/.qswitch/` | `QSWITCH_DIR` |
| 加密账号快照 | `~/.qswitch/vault/<tool>/<id>.enc` | 随 `QSWITCH_DIR` |
| 包装密钥 | macOS Keychain `qswitch.vault` / `wrap-key-v1` | — |
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
qswitch list
qswitch status
qswitch probe [--tool codex|grok|cursor|devin|zcode|kimi]
qswitch switch codex <id-or-email>
qswitch switch cursor <id-or-email>                # 会关掉并重启 Cursor.app
qswitch doctor
qswitch serve [--addr 127.0.0.1:7432]
```

本机页面默认 `http://127.0.0.1:7432`（只绑 loopback）。`qswitchd` 起来后也能开；这时再跑 `qswitch serve` 会打印现成地址然后退出，不会跟 daemon 抢端口。用来看余量、收录当前登录、ChatGPT / Grok Device Code 加号、删除仓库里的号。页面上不会出现 token。

`GET /api/health` 返回服务存活状态和构建版本；配额与账号状态以 `/api/overview` 为准。网页写入要求同源请求，命令行无 Origin 的本地请求仍可使用。

Cursor 切号会同时写入 IDE 和 CLI 的登录字段。

```
cp ~/.qswitch/com.qswitch.plist ~/Library/LaunchAgents/com.qswitch.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.qswitch.plist
```

launchd 样例见 `contrib/launchd/`（占位符，须用 `qswitch init` 生成带本机路径的 plist）。默认 `KeepAlive=false`，先确认 Keychain 无弹窗。
