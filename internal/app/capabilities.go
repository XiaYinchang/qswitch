package app

import (
	"fmt"
	"os"
	"runtime"

	"qswitch/internal/adapter"
)

type Capability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Command   string `json:"command,omitempty"`
}

func (a *App) Capabilities(tool adapter.Tool) map[string]Capability {
	out := capabilities(tool, runtime.GOOS, a.CheckScope(tool))
	if tool == adapter.Cursor && a.CheckScope(tool) == nil {
		if a.cursorDesktopSync() {
			out["desktop_sync"] = Capability{Available: true}
		} else {
			out["desktop_sync"] = Capability{Reason: "此平台仅管理 Cursor CLI，不读取或写入桌面账号"}
		}
	}
	if tool == adapter.Claude && runtime.GOOS == "darwin" && a.CheckScope(tool) == nil {
		supported := false
		if ad := a.Adapters[tool]; ad != nil {
			for _, path := range ad.LivePaths(a.UserHome) {
				if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
					supported = true
					break
				}
			}
		}
		if !supported {
			for _, action := range []string{"capture", "switch", "auto_switch", "run", "live_refresh", "continuation"} {
				out[action] = Capability{Reason: "此实例未发现受支持的 Claude 文件凭据；macOS Keychain 模式暂不支持，可在 Linux 实例管理"}
			}
		}
	}
	return out
}

func capabilities(tool adapter.Tool, goos string, scopeErr error) map[string]Capability {
	out := map[string]Capability{}
	for _, action := range []string{"capture", "probe", "switch", "auto_switch", "login", "run"} {
		out[action] = Capability{Available: true}
	}
	out["run"] = Capability{Reason: "请使用该工具的官方 CLI"}
	out["login"] = Capability{Reason: "先在当前主机的官方客户端登录，再收录已验证的账号", Command: "qswitch capture --tool " + string(tool)}
	switch tool {
	case adapter.Codex, adapter.Grok:
		out["login"] = Capability{Available: true, Command: "qswitch login " + string(tool)}
	case adapter.ZCode:
		for _, action := range []string{"switch", "auto_switch"} {
			out[action] = Capability{Reason: "ZCode 仅支持订阅监控，不支持切换账号"}
		}
	case adapter.Antigravity:
		out["run"] = Capability{Available: true, Command: "qswitch run antigravity"}
		out["login"] = Capability{Reason: "在当前主机完成 Antigravity 原生登录，qswitch 会自动收录已验证的身份", Command: "qswitch run antigravity"}
	case adapter.Claude:
		out["login"] = Capability{Available: goos == "linux", Reason: "Claude 隔离登录需要在 Linux 终端完成", Command: "qswitch login claude"}
		if goos == "linux" {
			out["login"] = Capability{Available: true, Command: "qswitch login claude"}
		}
		out["run"] = Capability{Available: goos != "windows", Command: "qswitch run claude"}
		if goos == "windows" {
			for _, action := range []string{"run", "switch", "auto_switch"} {
				out[action] = Capability{Reason: "Windows 暂不支持 Claude 受管会话与账号切换"}
			}
		}
	}

	out["desktop_sync"] = Capability{Reason: "此工具只管理 CLI 凭据，不同步独立桌面账号"}
	out["live_refresh"] = Capability{Available: tool != adapter.Codex && tool != adapter.ZCode && tool != adapter.Cursor && tool != adapter.Devin, Reason: "当前账号由原生客户端续期"}
	out["parked_refresh"] = Capability{Available: tool == adapter.Codex || tool == adapter.Grok || tool == adapter.Kimi || tool == adapter.Claude || tool == adapter.Antigravity, Reason: "此工具没有独立的闲置账号 OAuth 续期流程"}
	if out["parked_refresh"].Available {
		out["parked_refresh"] = Capability{Available: true}
	}
	out["continuation"] = Capability{Reason: "普通会话不会自动重启或发送续接消息"}
	if tool == adapter.Antigravity {
		out["desktop_sync"] = Capability{Available: true}
	}
	if tool == adapter.Cursor && goos == "darwin" {
		out["desktop_sync"] = Capability{Available: true}
	}
	if tool == adapter.Claude && goos != "windows" {
		out["continuation"] = Capability{Available: true}
	}
	if goos == "windows" && (tool == adapter.Grok || tool == adapter.Kimi) {
		out["live_refresh"] = Capability{Reason: "Windows 尚未验证官方凭据锁，当前账号由原生客户端续期"}
	}
	if goos == "windows" && tool == adapter.Kimi {
		out["parked_refresh"] = Capability{Reason: "Windows 的持久续期恢复尚未验证，请在原生客户端续期"}
	}
	if out["live_refresh"].Available {
		out["live_refresh"] = Capability{Available: true}
	}
	if scopeErr != nil {
		for action, cap := range out {
			cap.Available = false
			cap.Reason = scopeErr.Error()
			out[action] = cap
		}
	}
	return out
}

func (a *App) CheckAction(tool adapter.Tool, action string) error {
	if err := a.CheckScope(tool); err != nil {
		return err
	}
	cap, ok := a.Capabilities(tool)[action]
	if !ok || !cap.Available {
		return fmt.Errorf("%s 操作不可用：%s", tool, cap.Reason)
	}
	return nil
}

// Cursor's adapter owns the supported storage layout. Tests can select its
// platform explicitly; callers must not infer desktop support from CLI support.
func (a *App) cursorDesktopSync() bool {
	if capable, ok := a.Adapters[adapter.Cursor].(interface{ SupportsDesktopSync() bool }); ok {
		return capable.SupportsDesktopSync()
	}
	return runtime.GOOS == "darwin"
}
