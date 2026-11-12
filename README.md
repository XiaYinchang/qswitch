# qswitch

本机多账号配额轮换器：把 Codex / Grok / Cursor / Devin CLI / Kimi Code 的官方登录收进加密仓库，当前号用尽后自动切到下一个有余量的号。 Devin 展示套餐及适用的日/周额度；ZCode 桌面 Coding Plan 支持独立的订阅监控。

硬约束：

- 正在生成不杀。切号时：空闲后结束对应 CLI；关掉并重启 ChatGPT.app / Cursor.app / Grok Bot.app；写入官方登录文件。Cursor 只改 `cursorAuth/*`，不碰聊天和 13GB 状态库。普通 CLI 不自动重启；通过 `qswitch run claude` 启动的受管会话保存工作目录和会话 ID，在受控切号后恢复原对话。
- 关闭桌面前先确认 CLI 可以切换，`--cli-only --kill-cli` 同样必须等待空闲。关闭桌面后若切换失败，会尝试重新打开原应用，并保留切换和重启错误。已排队的耗尽切换若遇到账号恢复、当前账号变化或自动切换关闭，会取消。
- 保活、探测、切号和删除按工具互斥；等待锁后重新读取凭据，避免重复使用轮换的 refresh token。daemon 每个工具独立调度，一个工具的慢请求不延误其他工具的收录、探测与待切换执行。保活临时失败保留已有配额并等待 2 分钟再试；永久失效标记需要重新登录，并使用较长的认证退避。保活与配额查询使用独立退避，查询接口故障不阻断安全续期。daemon 每 2 秒重载配置，关闭自动切换不必重启服务。
- 本地日志采用配额事件的时间，旧事件不会覆盖更新的探测结果或刚切换的账号。没有事件时间的旧格式日志只在尚未探测或切号时作为初始线索。
- 配额接口认证成功但无法按工具协议识别额度时，将历史登录失效改为用量未知；已有明确配额仍保留。网络失败保留已知状态并退避。
- 自动切号只使用最近一次有效的配额观测；历史“正常”不能覆盖后续查询失败。候选号的证据过期时先核验，待切任务执行前也会复查，失败退避期间不反复请求。当前号确认耗尽后，后续查询失败或被限流不推翻这一结论：在接口给出的重置时间前（ChatGPT 为下一次恢复检查前）照常排队并完成切号。页面用量条的颜色和百分比只表示已知用量；查询失败、历史记录及记录时间单独说明。当前账号超过查询上限、闲置账号超过 30 分钟，另留一个后台调度周期后，旧记录会隐藏“正常”等状态并标为历史数据；跨过额度重置时间也须重新确认。确认耗尽的记录在重置前一直有效，不标为历史数据，即使之后的查询失败。页面的“刷新”“刷新用量”和 `qswitch probe` 不查询确认耗尽、尚未重置的号，也不重复查询 1 分钟内查过的号，避免连续刷新把查询接口打到限流；单个账号的“查询”按钮照常查询。
- 多个候选号都有余量时，严格按最长额度窗口（如周额度）的重置时间先后选号，先用掉即将作废的余量，接近上限的号也不例外；重置时间已知的号排在未知的前面；重置时间相同或都未知时，先选“正常”再选“接近上限”，再选用量最低的号。
- Claude 受管会话的当前号还有额度时，如果另一个号的最长窗口在 12 小时内重置、还剩至少 10%、而且比当前号先重置，daemon 会主动切过去先用掉它：等所有受管会话都空闲（没有进行中的 turn、后台任务、定时任务或正在编辑的草稿）才开始，开始前会再查一次那个号；刚开始就有会话变忙，立刻放开它、稍后再试，不会扣住任何会话的输入。条件不再成立时取消，切号日志写明原因。刚切过号的 10 分钟内不做这种切换。
- Grok 通过官方 `GET /v1/billing?format=credits` 读取当前共享额度，并显示周/月周期和重置时间。与官方 `/usage` 一致，完整有效的统一计费周期省略百分比时按 0% 处理；空配置、无效周期或错误字段仍未知。只使用当前账期，不把历史用量或按量付费比例当作套餐比例；达到 100% 才判套餐耗尽，99.9% 不提前切号。有可用预付费余额或按量额度时仍保留可用状态。
- Codex/Grok 的 `--cli-only` 不恢复桌面快照。完整恢复先验证快照，桌面写入或最后的 CLI 写入失败时回滚受影响文件；回滚失败会明确报错。这不包含进程或主机突然中断后的自动恢复。
- 查配额按工具适配，不把裸 429 当作用尽。Codex 看会话 JSONL 的 `rate_limits` / `usage_limit_reached`；Grok 看本地 `billing: fetched credits config` 和 HTTP 402 `usage balance exhausted`；Cursor 看 `GetCurrentPeriodUsage` 的 Auto（自家模型）与高级模型；Grok Bot 周额度走 `GetSandUsageStatus`，页面上和 Codex/Grok/Cursor 并列。忽略 `resource_exhausted`（那是容量）。当前账号按消耗速度在默认 5–15 分钟内查询，独立于本地日志更新；闲置账号约每 30 分钟更新，每个工具每轮最多查一个，优先最久未查询的账号，`probe_idle_accounts=false` 可关闭。ChatGPT 用尽号不按显示的重置时间死等（窗口可能提前恢复），大约每 30 分钟点探一次；其他工具耗尽后仍等到显示的重置时间再查。普通查询连续失败按 2、4、8、15 分钟退避，成功后清零；429/503 的有效 `Retry-After` 会作为最短等待时间。`http_backoff` 保留为认证失效的较长退避。
- Cursor 桌面和 cursor-agent 共用同一套 token：`switch cursor` 会把选中账号同时写进 IDE 和 CLI。
- 官方客户端登录第二个号时，`qswitchd` 会自己收进仓库，不必再跑 `qswitch capture`。
- ChatGPT 闲置号会保活。加号请用 `qswitch login codex`：优先拉起本机官方 `codex login --device-auth`（隔离 `CODEX_HOME`，强制 file 存储，不改当前 `auth.json` / 不 logout）。流量、轮询间隔和换票格式与官方 CLI 相同。在 ChatGPT.app 里切换账号等于官方 logout，会作废上一份 refresh。
- Grok 加号同样走 Device Code：`qswitch login grok` 优先拉起本机官方 `grok login --device-auth`（隔离 `GROK_HOME`，不改当前 `auth.json` / 不 logout）。页面加号走同一套 `auth.x.ai` device code。不要在当前 grok 里换号或 `grok logout`。
- 同一登录下的多个 workspace 共用当前 live 的 access token 去查用量；不同邮箱的 refresh_token 按官方 OAuth 刷新写回仓库。换号时若仍是同一 Gmail，会把 live token 合并进目标 workspace。
- Grok 闲置号同样保活：access 大约 6 小时过期，官方 CLI 会在到期前用 `refresh_token` 向 `auth.x.ai` 换票并轮换 refresh。仓库里的闲置号按同一条 OIDC 刷新写回；当前 Grok CLI 运行时由官方客户端续期；CLI 已退出时，qswitch 持有官方 `auth.json.lock` 并重读凭据后续期，轮换结果同时写回登录文件和加密仓库。Grok Bot.app 运行不阻断这项 CLI 保活。Cursor 桌面/CLI 存的是约 60 天的 session JWT，access 与 refresh 是同一张票，没有可安全调用的续期接口，过期或登出作废后只能重新登录。

## Claude Code 多账号自动切换：使用方法

适用于 Linux 服务器（如 novix）上用 tmux 或 Rex 终端长期运行的 Claude Code 会话：当前号额度用完时，qswitch 自动切到有余量的号，并让被中断的会话接着干活。

### 一次性准备

1. 按下文“命令”一节的 systemd 步骤部署 `qswitch` 和 `qswitchd`，确认 `systemctl --user is-active qswitchd` 为 `active`。
2. 每个 Claude 账号收录一次（不影响当前登录）：

   ```sh
   qswitch login claude          # 打开授权链接，把 code 粘贴回当前终端
   qswitch list --tool claude    # 确认账号都已收录
   ```

3. 打开自动切换：`qswitch enable-auto --tool claude`。

### 新起一个任务

```sh
tmux new-session -s task1                          # 名字随意，每个任务一个
cd ~/workspace                                     # 进入项目目录
qswitch run claude --dangerously-skip-permissions  # 代替直接运行 claude
```

启动后照常在 Claude 里下达任务。离开按 `Ctrl+B D`，回来用 `tmux attach -t task1`；连着看也不影响自动切换。

用 Rex 时，在 Rex 里新开一个终端（App 里新建标签页，或运行 `rex new task1 -c ~/workspace`），进入项目目录后同样运行 `qswitch run claude --dangerously-skip-permissions`。关掉 App 或断开连接时终端仍留在 Rex 服务器上；App 正显示这个终端也不影响自动切换。

### 接着跑已有的会话

先在原会话里 `/exit`，退出时会显示 `claude --resume <会话ID>`。然后在 tmux 或 Rex 终端里按会话当时的状态启动：

```sh
# 已完成阶段任务、在等你输入
qswitch run claude --resume-continuation=idle --dangerously-skip-permissions --resume <会话ID>

# 因额度用完停下
qswitch run claude --resume-continuation=continue --dangerously-skip-permissions --resume <会话ID>
```

`idle` 会话不会收到续接指令。`continue` 会话在当前号有额度、又没有切号排队时，会在空闲且没有草稿的几秒内收到一次续接指令；当前号已耗尽时，由号恢复或下一次切号带它继续。

### 查看状态

| 想知道 | 命令 |
|---|---|
| 各会话是否就绪、被什么挡住 | `qswitch sessions`，或在会话里输入 `/qswitch-status` |
| 各账号用量与当前号 | `qswitch list --tool claude`、`qswitch status` |
| 网页总览 | 本机运行 `ssh -N -L 7432:127.0.0.1:7432 <服务器>`，打开 `http://127.0.0.1:7432` |

### 结束与升级

- 任务做完在会话里 `/exit`，正常退出不会被自动拉起。
- 升级 qswitch 只需按部署步骤替换程序（日常用 `scripts/deploy.sh`，见「命令」一节）；各会话在空闲时自动换成新版本并恢复原对话，不必手动重启。

### 注意事项

- **只有 `qswitch run claude` 在 tmux 或 Rex 终端里启动的会话参与受控切号。** 普通非受管 `claude` 不阻断自动或手动切号，qswitch 不等待、不退出、不杀、不恢复它；但共享的 `~/.claude/.credentials.json` 与 `~/.claude.json` 仍会更新，运行中的非受管会话内存账号不保证改变。在其他终端里启动的受管会话不能参与切号，排队的切号会一直等它退出。
- **可用的启动参数**：`--model`、`--fallback-model`、`--effort`、`--permission-mode`、`--dangerously-skip-permissions`、`--allowedTools`/`--disallowedTools`、`--tools`、`--agent`/`--agents`、`--system-prompt` 系列、`--max-budget-usd`、`--name`、`--verbose`、`--resume`/`--continue`。`--print`、`--settings`、`--plugin-dir` 等其他参数和子命令会被拒绝。
- **不能设置的环境变量**：`ANTHROPIC_API_KEY`、`ANTHROPIC_AUTH_TOKEN`、`ANTHROPIC_BASE_URL`、`CLAUDE_CONFIG_DIR`、`CLAUDE_CODE_OAUTH_TOKEN`、`CLAUDE_CODE_USE_BEDROCK`/`VERTEX`/`FOUNDRY` 等，会被拒绝启动。
- **切号只在受管会话空闲时进行**：进行中的 turn、活动前台工具、queued prompt 都会让切号等待。输入框里的草稿只在 30 秒内改过时让切号等待，之后照常切号，重启后草稿放回输入框；超过 8000 字的草稿会一直让切号等待。`rate_limit` 中断后的后台任务、agent、cron 不再阻断，但切号重启可能中断这些异步工作，恢复后应检查未完成的副作用工作。切号准备期间你发出的消息不会让切号等待：qswitch 先暂存，切号完成后作为重启会话的第一条消息自动发出，会话需要续接时接在续接指令之后；切号撤销时在原会话里原样发出。超过 6000 字的消息退回输入框，按草稿处理。
- **所有号都用完时**会等最早重置的那个号；重置后自动切过去并让中断的会话继续。
- **切号期间关掉某个会话**（自己 `/exit`、关终端、Claude 崩溃）不会卡住切号：runner 等 Claude 结束后退出本轮切号，其余会话照常切；这个会话不再被重启。
- **出现异常**时，`qswitch sessions` 的 `recovery=` 会给出原因和恢复命令；确认旧进程已退出后按提示手动恢复。这条记录只是提示，看过或恢复后用 `qswitch sessions --forget <runner>` 删掉（`runner` 取同一行的 `runner=`）。只有 runner 进程没来得及清理就没了（被 SIGKILL、OOM）时切号才会停下，日志里是 `runner <id> stopped during generation …; manual recovery required`：它的 Claude 可能还在跑，同样用 `--forget` 删记录，切号自动继续；记录里的 Claude 进程还在时它会拒绝，先结束那个进程。
- **切号过程**写在 qswitchd 的日志里，用 `journalctl --user -u qswitchd` 查看：每次切号何时排队、在等哪个会话及原因、何时切完或撤销及原因，每种状态只记一行；会话确认退出后超时、被 runner 结束时另记一行（`exit overdue, ending it`）。

## 新增订阅

- **ZCode**：读取桌面客户端 `~/.zcode/v2/credentials.json` 中当前 BigModel / Z.ai 个人 Coding Plan，支持官方加密格式。使用官方 `/api/monitor/usage/quota/limit`，显示套餐、5 小时/周模型额度及月度 MCP 额度。MCP 用尽不误判模型额度耗尽；切换套餐后删除旧窗口。当前仅监控订阅，账号在 ZCode 中切换；不修改独立的 ZCode CLI 配置。
- **Kimi Code**：读取 `~/.kimi-code/credentials/kimi-code.json`（支持 `KIMI_CODE_HOME`），用 `/coding/v1/me` 确认账号身份，优先显示接口返回的手机号，缺省时显示昵称或账号 ID；用 `/coding/v1/usages` 展示 5 小时、7 天、月度总额度及编程分项；`usages` 缺少某窗口时从响应的 `limits` 数组按窗口时长补齐（含月度窗口）。月度总额度另从桌面版 Kimi.app 订阅页同款的会员网关读取：只读复用桌面 App localStorage 里的网页会话（要求会话属于同一账号且未过期），不刷新、不轮换桌面登录，桌面 App 未运行或会话过期时自动跳过。编程分项不作为独立耗尽门限。切号只原子替换凭据文件，保留 config/hooks；有 Kimi 会话运行时阻断切换。闲置账号按官方 OAuth 协议续期写入加密仓库；当前 CLI 运行时由官方客户端续期，退出后由 qswitch 持有官方 OAuth 目录锁续期，并同时更新登录文件和加密仓库。续期门限跟随官方客户端，至少预留 5 分钟。身份无法确认时不收录、不抢用 refresh token。
- **Claude Code**：管理 `~/.claude/.credentials.json` 中的官方 OAuth 登录（Linux/macOS 文件布局，支持 `CLAUDE_CONFIG_DIR`；macOS Keychain 存储暂不支持）。收录前用官方 `/api/oauth/profile` 确认账号 uuid、邮箱和套餐（Pro/Max/Team），确认失败不收录。用量走官方状态栏同款 `GET /api/oauth/usage`：5 小时与 7 天窗口决定是否耗尽（达到 100% 才切号），Sonnet/Opus 专项周额度和按量付费余额只展示、不作为切换门限。切号原子替换凭据文件并同步 `~/.claude.json` 的 oauthAccount 显示信息；普通非受管 Claude 不阻断自动或手动切号，qswitch 只协调受管会话。refresh token 一次性轮换：闲置账号续期只写加密仓库；当前账号在 CLI 退出后由 qswitch 复核文件内容续期，同时写回登录文件和仓库。

  收录以官方 profile 返回的身份为准，同时修正快照中的账号显示信息；切号也会修正旧快照的显示信息，并在缺少 `.claude.json` 时创建该文件。凭据写入失败会回滚账号元数据，回滚失败单独报错。

  **续期恢复**：OAuth 续期与恢复写回仍会为了 refresh token 安全检查所有 Claude 进程。换票前在加密仓库记录待恢复事务，收到新凭据后先持久化，再验证身份和写回。profile 临时故障不会反复消费 refresh token；身份未确认的凭据隔离保存，不按旧元数据 UUID 收录。文件写回失败可在重启后恢复；若外部已登录另一个账号，不覆盖新登录。换票结果无法确定时停止复用旧 refresh token，明确要求重新登录。有 Claude 进程运行时由 Claude Code 自己续期当前账号；会话都空闲、token 过期后的定时查询会被拒并按认证失效长退避，之后一旦收录到 Claude Code 续期出的未过期 token，就撤销这次认证失效判定，恢复正常查询和撞限复查。

  **加号（Linux）**：在同一个终端运行一次 `qswitch login claude`。qswitch 在数据目录下创建私有的 `claude-enrollments/session-*` 临时目录，设置 `CLAUDE_CONFIG_DIR` 和 `CLAUDE_SECURESTORAGE_CONFIG_DIR`，运行官方 `claude auth login --claudeai`；打开授权链接并把完整 code 粘贴回当前终端、按回车即可。官方登录成功退出后，qswitch 校验账号身份、写入加密仓库并打印 `enrolled claude ...`，不需要另开终端或手动 `capture`。整个过程不清除、覆盖或登出当前登录，不改变当前账号指针，也不调用新账号的 refresh token。正常退出、Ctrl+C、终端挂断、SIGTERM 或超时会结束直接子进程并清理临时目录；SIGKILL 时由 Linux 结束直接子进程，残留目录在下次使用同一数据目录登录时回收。仅回收带有效标记、属主和权限正确且租约已释放的目录，不扫描旧 `/tmp` 目录，也不结束其他 Claude 会话。身份校验失败不报收录成功，需要重新执行登录。

  **终端输入**：Claude Code 2.1.311 的 `auth login` 在 TTY 输入下会关闭回显，但其按行读取器不显示输入字符；这与普通 `claude` 登录 TUI 不同。qswitch 保留终端正常回显，通过系统 `cat` 子进程把输入送至官方登录程序的 stdin 管道，官方 OAuth、浏览器回调与换票流程保持由 Claude Code 执行。登录结束或取消时一并结束输入转发进程，不在后台等待用户敲回车。

  **macOS 限制**：macOS 上凭据存系统 Keychain，qswitch 暂不支持直接管理。请在 Linux 服务器（如 novix）上运行 qswitch 管理 Claude 账号。

  **tmux / Rex 无人值守续接（Claude Code 2.1.312+）**：在 tmux 窗格或 Rex 终端的项目目录里通过 `qswitch run claude` 启动，启用 `qswitch enable-auto --tool claude`，并保持 `qswitchd` 运行。使用默认 OAuth 配置与同一个 qswitch 数据目录；不接受 API key、云厂商、独立凭据目录、无持久化或会绕过受管插件的启动选项。只有 `qswitch run claude` 受管会话参与切号协调；普通 `claude` 进程不阻断切换。准备切号、确认退出和重启前，runner 都向 tmux 或 Rex 服务器核实自己仍在启动时的窗格或终端里：该终端的进程是 runner 的上级进程，且 runner 读的就是这个终端。核实不了的会话不参与切号。在 Rex 终端里开的 tmux 按 tmux 窗格核实。

  ```sh
  tmux new-session -s work   # 或在 Rex 里新开一个终端
  qswitch run claude
  # 可以随时 attach 查看；离开时 Ctrl+B D 即可，tmux 仍保留会话。Rex 终端在 App 关掉后也继续运行
  qswitch sessions --json
  ```

  daemon 确认当前配额耗尽且候选号可用后，等待所有受管会话就绪，再经官方 `/exit` 退出。会话确认退出后 15 秒仍未结束（例如 Claude Code 把 `/exit` 压在输入队列里，或为插件看不到的后台工作弹出确认框），runner 先发 SIGTERM、10 秒后再发 SIGKILL 结束它，按已确认的退出处理。确认退出时还有后台任务、agent 或定时任务的会话，这个时限是 5 秒：Claude Code 会先弹出「Background work is running」确认框，没人回答，`/exit` 不会执行。确认时会话已空闲，结束进程不会丢掉退出本来会保留的内容。某个 runner 一时没有应答控制请求时，切号保留到下一轮重试，不取消，以免已经退出的会话被不带续接地重启。所有子进程退出确认后才写凭据，并在原目录用原会话 ID 恢复（还没发过消息的新会话没有记录可恢复，用同一 ID 重新开始）；仅因 `rate_limit` 中断的会话收到一次继续工作的指令，已完成或用户取消的会话保持等待输入。初次接管历史会话必须显式指定状态：`qswitch run claude --resume-continuation=idle --resume <session-id>`，或对额度中断会话使用 `--resume-continuation=continue`。`continue` 会话的续接时机见下一段。

  受管会话一旦因 `rate_limit` 中断，daemon 在约 5 秒内用官方接口复查当前号，不等下一次定时查询；确认耗尽才排队切号，查询退避仍然生效。已确认耗尽的号不再因后续会话中断重复查询，直接排队切号，避免同一账号短时间内请求过多被限流。耗尽号（当前号或闲置号）过了接口给出的重置时间，daemon 在 10 秒余量后复查一次，不受重置前那次查询的间隔限制，查询退避仍然生效。若当前号自己恢复、没有发生切号：会话一直开着时由 Claude Code 自带的自动续接继续；会话在等待期间重启过（切号取消、runner 升级或手动以 `continue` 接管都会让 Claude Code 取消自带续接）时，qswitch 补发一次续接指令。daemon 每 5 秒检查一次：只要当前号有额度、又没有切号排队，就让这类会话在空闲且无草稿时继续，不依赖恰好看到号恢复的那一刻，daemon 重启或会话晚于恢复才重启都不会漏掉。

  进行中的 turn、活动前台工具、queued prompt 以及 30 秒内改过的文本草稿都会阻断自动切号；tmux 有客户端连接、Rex App 正显示这个终端都不阻断，你看着时也可能看到会话自动退出并恢复。会话内 `/qswitch-status` 可查看具体阻断原因。受管会话因 `rate_limit` 中断后，后台任务、agent、cron 不再阻断；没撞限的会话挂着后台任务或 agent 时，切号最多等 3 分钟；挂着定时任务（cron）时不等，因为定时任务不会自己结束。runner 自升级仍会等定时任务结束。两种情况下切号重启都会停掉这些工作，重启后的续接指令列出被停掉的项目，提醒会话先检查它们留下的结果（包括在其他机器上启动的进程）再决定是否重跑。只保证已提交对话，恢复后应检查未完成的副作用工作；官方接口不能保证未提交附件和所有临时弹窗状态。草稿检查不是原子输入锁：在会话退出前一刻开始输入的内容可能丢失，使用本模式即接受这项自动重启边界。

  升级 qswitch 后不必手动重启受管会话。runner 发现自己的程序文件被替换（按部署方式改名覆盖），会等会话空闲且没有切号在进行时，经同一套确认流程 `/exit`，在原进程内换成新版本，并像切号时一样用原会话 ID 恢复。runner 的进程号、ID、控制端口和租约都不变，不发续接指令；会话在此之前被额度中断的状态原样保留。还在等续接的会话不升级：因 `rate_limit` 中断的会话马上要参与切号，由切号带着续接指令重启；切号后带着续接指令重启的会话，要等续接答完或被中止后才升级。否则升级会先把它重启一遍，丢掉续接、取消 Claude Code 自带的自动续接，还会停掉切号本来允许它保留到那时的后台任务。会话一忙起来就放弃本次升级，下次空闲再试；换版失败则留在旧版本继续运行。

  用户正常 `/exit` 不会被重新拉起；不能参与切号的会话退出后，排队的切号照常进行。切号排空期间会话自己结束（`/exit`、挂断、崩溃）或 runner 收到 SIGTERM/SIGHUP 时，runner 等 Claude 结束后退出本轮切号，切号照常进行，这个会话不会被重启；崩溃的会话，以及已为切号退出 Claude 却没等到重启的会话，在 `qswitch sessions` 里留有恢复命令。runner 没来得及清理就消失（SIGKILL、OOM）时，它的 Claude 可能还在运行，切号停下并在日志里报 `manual recovery required`；用 `qswitch sessions --forget <runner>` 删掉这条记录（`runner` 取 `qswitch sessions` 里的 `runner=`），切号继续；记录里的 Claude 进程还在时它会拒绝。崩溃等留下的恢复记录不挡切号，看过或恢复后也用它清掉。若凭据已写入但 daemon 未能确认提交，不自动发送续接指令。
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

配额查询记录保留 30 天，daemon 启动时和之后每天清理一次；每个账号始终保留最近 16 条和最近一次有效观测，长期闲置的账号不会丢失用量依据。

Keychain 读取失败或密钥格式异常时直接报错，不重新生成或覆盖包装密钥。首次创建若遇到另一进程同时初始化，会回读已有密钥。

macOS 上每个 qswitch 进程只在第一次用到包装密钥时读一次 Keychain，之后用内存里的副本，同一时刻只有一个 `security` 在跑。每次 `security` 最多等 2 分钟：登录钥匙串锁着时它会弹密码框等人输入，超时就结束这次读取并报错。读取失败后 10 分钟内直接返回这次的错误，不再询问 Keychain，钥匙串锁着时不会接连弹框。

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
qswitch run claude [--resume-continuation=idle|continue] [Claude arguments...]  # 受管会话，在 tmux 或 Rex 终端里运行
qswitch sessions [--json] [--forget <runner>]    # 会话状态与异常恢复提示；删掉已结束会话留下的记录
qswitch enable-auto [--tool ...]                  # 打开自动切换（disable-auto 关闭）
qswitch list
qswitch status
qswitch probe [--tool codex|grok|cursor|devin|zcode|kimi|claude]
qswitch switch codex <id-or-email>
qswitch switch cursor <id-or-email>                # 会关掉并重启 Cursor.app
qswitch doctor
qswitch serve [--addr 127.0.0.1:7432]
qswitch web set-password <用户名> [--stdin]     # 页面登录密码，见「页面登录」

scripts/deploy.sh [--ref <commit>] [novix|macmini ...]   # 新 clone 构建并部署到两台机器，见下文
```

本机页面默认 `http://127.0.0.1:7432`（只绑 loopback）。`qswitchd` 起来后也能开；这时再跑 `qswitch serve` 会打印现成地址然后退出，不会跟 daemon 抢端口。用来看余量、收录当前登录、ChatGPT / Grok Device Code 加号、删除仓库里的号。页面上不会出现 token。

`GET /api/health` 返回服务存活状态和构建版本；配额与账号状态以 `/api/overview` 为准。网页写入要求同源请求，命令行无 Origin 的本地请求仍可使用。

普通 Git 工作区构建自动从 Go 嵌入的 VCS 信息读取提交号，未提交改动带 `-dirty`；发布构建可通过 `-ldflags "-X qswitch/internal/buildinfo.Version=<release>"` 显式指定版本。无 VCS 信息且未指定版本时显示 `dev`。CLI 的 `version`、daemon 的 `--version`、健康接口与页面标题使用同一个版本标识。

页面显示阻塞切换的 PID、待切换目标账号及排队状态。Kimi 会话运行时必须先退出，仅等待 30 秒空闲不会解除阻塞；普通非受管 Claude 不阻断自动或手动切号。通过 `qswitch run claude` 启动的受管会话会在空闲时自动退出、切号并恢复。排队目标仍需在执行前通过最新用量和安全检查。

### 页面登录

`qswitch web set-password <用户名>` 设置页面密码（至少 10 个字符；交互输入不回显，`--stdin` 从标准输入读一行给脚本用）。密码以 PBKDF2-SHA256 存在 `~/.qswitch/web-auth.json`（0600），daemon 在一秒内自动读到，不用重启。设了密码以后，页面和 `/api/*` 都先要登录，只有 `GET /api/health` 不用；脚本仍可用 `Authorization: Bearer <[web] token>`，`?token=` 不再接受。登录态是 30 天的 HttpOnly Cookie，由随密码一起轮换的密钥签名：改密码后所有浏览器都要重新登录，daemon 重启不掉线，「退出登录」只清当前浏览器。同一来源连续输错 5 次暂停登录 15 分钟，登录成功、失败暂停都记一行日志。没设密码时行为和以前一样：loopback 监听不要登录（SSH 隧道已经认过人），公网监听要 token。页面上不会出现密码和 token。

### 经 Deck 访问（novix）

novix 的页面挂在 `https://deck.xiayinchang.top/qswitch/`，Deck 首页有入口卡片。链路是：novix 上的用户单元 `qswitch-tunnel.service`（`contrib/deck/qswitch-tunnel.service`）用专用密钥向 lisa-hk 建反向 SSH 隧道 `-R 127.0.0.1:7432:127.0.0.1:7432`，lisa-hk 上的 `qswitch-tunnel` 账号只允许这一个 remote forward（`contrib/deck/sshd-qswitch-tunnel.conf`），lisa-hk 的 nginx 把 `deck.xiayinchang.top/qswitch/` 代理到隧道并带上 `X-Forwarded-Host / -Proto / -Prefix / -For`（`contrib/deck/nginx-deck-qswitch.conf`）。qswitch 只信任来自 loopback 对端的这几个头，用它们判断同源、给 Cookie 加 `Secure` 和 `Path=/qswitch`、按真实来源计登录失败；页面里的请求都是相对路径，挂在任何前缀下都能用。经代理的请求在既没有密码也没有 token 时一律 403。本机仍可用 `ssh -N -L 7432:127.0.0.1:7432 novix` 打开 `http://127.0.0.1:7432`，同样要登录。

公网暴露：`[web] addr` 设为非 loopback 地址时必须先设密码或 `[web] token`，否则拒绝启动；这时所有请求都要登录或 Bearer，Origin 同源校验照常生效。明文 HTTP 下密码会经过链路，只应放在 TLS 反向代理之后。

Cursor 切号会同时写入 IDE 和 CLI 的登录字段。

```
cp ~/.qswitch/com.qswitch.plist ~/Library/LaunchAgents/com.qswitch.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.qswitch.plist
```

launchd 样例见 `contrib/launchd/`（占位符，须用 `qswitch init` 生成带本机路径的 plist）。默认 `KeepAlive=false`，先确认 Keychain 无弹窗。

日常升级两台机器用 `scripts/deploy.sh [--ref <commit>] [novix|macmini ...]`，在 Mac mini 上运行：从 origin 新 clone 指定提交（默认 `origin/main`）构建 linux/amd64 和 darwin/arm64，核对嵌入的提交号和校验和；目标机有排队的切号就停下；否则把两个程序放到 `~/.local/bin` 替换并重启 daemon，60 秒内健康接口没报出新版本就换回备份重启；之后等第一次额度探测再删备份，并报告 novix 上还没换到新程序的受管会话 runner（它们空闲时自己升级）。首次安装仍按下面的步骤手动做。

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
