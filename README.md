# qswitch

本机多账号配额轮换器：把 Codex / Grok / Cursor 的官方登录收进加密仓库，当前号用尽后自动切到下一个有余量的号。

硬约束：

- 正在生成不杀；生成停住后只重启 CLI，本机会话可 resume。
- 查配额按工具适配，不把裸 429 当作用尽。Codex 看会话 JSONL 的 `rate_limits` / `usage_limit_reached`；Grok 看本地 `billing: fetched credits config` 和 HTTP 402 `usage balance exhausted`；Cursor 看 `GetCurrentPeriodUsage` 的 Auto（自家模型）与高级模型；Grok Bot 周额度走 `GetSandUsageStatus`，页面上和 Codex/Grok/Cursor 并列。忽略 `resource_exhausted`（那是容量）。平时只探当前号。ChatGPT 用尽号不按显示的重置时间死等（窗口可能提前恢复），大约每 30 分钟点探一次；Grok/Cursor 仍等到显示的重置时间再查。不把空闲号当心跳扫。HTTP 间隔按消耗速度估「还能用多久」。探测失败退避 2 小时。
- Cursor 桌面和 cursor-agent 共用同一套 token：`switch cursor` 会把选中账号同时写进 IDE 和 CLI。
- 官方客户端登录第二个号时，`qswitchd` 会自己收进仓库，不必再跑 `qswitch capture`。
- ChatGPT 闲置号会保活。加号请用 `qswitch login codex`：优先拉起本机官方 `codex login --device-auth`（隔离 `CODEX_HOME`，强制 file 存储，不改当前 `auth.json` / 不 logout）。流量、轮询间隔和换票格式与官方 CLI 相同。在 ChatGPT.app 里切换账号等于官方 logout，会作废上一份 refresh。
- Grok 加号同样走 Device Code：`qswitch login grok` 优先拉起本机官方 `grok login --device-auth`（隔离 `GROK_HOME`，不改当前 `auth.json` / 不 logout）。页面加号走同一套 `auth.x.ai` device code。不要在当前 grok 里换号或 `grok logout`。
- 同一登录下的多个 workspace 共用当前 live 的 access token 去查用量；不同邮箱的 refresh_token 按官方 OAuth 刷新写回仓库。换号时若仍是同一 Gmail，会把 live token 合并进目标 workspace。
- Grok 闲置号同样保活：access 大约 6 小时过期，官方 CLI 会在到期前用 `refresh_token` 向 `auth.x.ai` 换票并轮换 refresh。仓库里的闲置号按同一条 OIDC 刷新写回；当前 live 会话不抢 refresh（避免和 CLI 双花）。Cursor 桌面/CLI 存的是约 60 天的 session JWT，access 与 refresh 是同一张票，没有可安全调用的续期接口，过期或登出作废后只能重新登录。

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

## 命令

```
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -o bin/qswitch ./cmd/qswitch
CGO_ENABLED=0 go build -o bin/qswitchd ./cmd/qswitchd

qswitch init
qswitch capture
qswitch login codex
qswitch login grok
qswitch list
qswitch status
qswitch probe [--tool codex|grok|cursor]
qswitch switch codex <id-or-email>
qswitch switch cursor <desktop-id> --force-align   # 须先退出 Cursor.app
qswitch doctor
qswitch serve [--addr 127.0.0.1:7432]
```

本机页面默认 `http://127.0.0.1:7432`（只绑 loopback）。`qswitchd` 起来后也能开；这时再跑 `qswitch serve` 会打印现成地址然后退出，不会跟 daemon 抢端口。用来看余量、收录当前登录、ChatGPT / Grok Device Code 加号、删除仓库里的号。页面上不会出现 token。

Cursor 自动切换默认关，直到桌面与 CLI 对齐。

```
cp ~/.qswitch/com.qswitch.plist ~/Library/LaunchAgents/com.qswitch.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.qswitch.plist
```

launchd 样例见 `contrib/launchd/`（占位符，须用 `qswitch init` 生成带本机路径的 plist）。默认 `KeepAlive=false`，先确认 Keychain 无弹窗。
