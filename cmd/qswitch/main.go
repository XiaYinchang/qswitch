package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
	"qswitch/internal/web"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	home, _ := os.UserHomeDir()
	bins := []string{lookSelf(), lookSibling("qswitchd")}
	a, err := app.Open(home, os.Getenv("QSWITCH_DIR"), nil, bins, nil, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer a.Close()

	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "init":
		return fail(a.Init())
	case "capture":
		tools := adapter.AllTools()
		if t, ok := flagTool(rest); ok {
			tools = []adapter.Tool{t}
		}
		for _, t := range tools {
			blobs, warn, err := a.Capture(t)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", t, err)
				continue
			}
			for _, b := range blobs {
				fmt.Printf("captured %s %s %s incomplete=%v stale_cli=%v\n", t, b.Identity.Email, b.Identity.StableID, b.Incomplete, b.StaleCLI)
			}
			for _, w := range warn {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w)
			}
		}
		return 0
	case "list":
		tool := ""
		if t, ok := flagTool(rest); ok {
			tool = string(t)
		}
		fmt.Print(a.List(tool))
		return 0
	case "status":
		fmt.Print(a.Status())
		return 0
	case "doctor":
		fmt.Print(a.Doctor())
		return 0
	case "serve":
		addr := a.Cfg.WebAddr()
		if v, ok := flagVal(rest, "--addr"); ok {
			a.Cfg.Web.Addr = v
			addr = v
		}
		if web.AlreadyServing(addr) {
			fmt.Printf("页面已经在跑：http://%s\n（一般是 qswitchd 占着这个端口，直接打开即可）\n", addr)
			return 0
		}
		fmt.Printf("qswitch web http://%s\n", addr)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := web.New(a, addr).Run(ctx)
		if err != nil && strings.Contains(err.Error(), "address already in use") {
			fmt.Printf("端口被占用。若 daemon 已启动，打开 http://%s 即可。\n", addr)
			return 1
		}
		return fail(err)
	case "probe":
		tools := adapter.AllTools()
		if t, ok := flagTool(rest); ok {
			tools = []adapter.Tool{t}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, t := range tools {
			a.KeepAlive(ctx, t)
			a.ProbeLocal(t)
			a.ProbeHTTP(ctx, t, true)
			a.ProbeRecovered(ctx, t, true)
		}
		fmt.Print(a.Status())
		return 0
	case "switch":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: qswitch switch <tool> <account-id|email> [--force-align] [--allow-stale-cli] [--kill-cli] [--cli-only]")
			fmt.Fprintln(os.Stderr, "  会关掉并重启 ChatGPT.app / Cursor.app / Grok Bot.app；空闲 CLI 一并结束")
			fmt.Fprintln(os.Stderr, "  --cli-only 只写 CLI 登录文件，不动桌面 App")
			fmt.Fprintln(os.Stderr, "  same email with personal+team workspaces: pass chatgpt_account_id")
			return 2
		}
		t, err := adapter.ParseTool(rest[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		opts := adapter.RestoreOpts{
			ForceAlign:    has(rest, "--force-align"),
			AllowStaleCLI: has(rest, "--allow-stale-cli"),
			CLIOnly:       has(rest, "--cli-only"),
		}
		err = a.Switch(t, rest[1], opts, has(rest, "--kill-cli"))
		return fail(err)
	case "apply":
		var t adapter.Tool
		if x, ok := flagTool(rest); ok {
			t = x
		}
		return fail(a.Apply(t, has(rest, "--kill-cli")))
	case "enable-auto":
		return setAuto(a, rest, true)
	case "disable-auto":
		return setAuto(a, rest, false)
	case "login":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: qswitch login codex|grok")
			fmt.Fprintln(os.Stderr, "  走本机官方 Device Code，不登出当前会话。")
			return 2
		}
		t, err := adapter.ParseTool(rest[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), 16*time.Minute)
		defer cancel()
		var (
			id   adapter.Identity
			lerr error
		)
		switch t {
		case adapter.Codex:
			fmt.Fprintln(os.Stderr, "加号使用官方 Codex Device Code。不要在 ChatGPT.app 里换号，不要跑 codex logout。")
			id, lerr = a.LoginCodex(ctx, func(code, url string) {
				fmt.Printf("\nFollow these steps to sign in with ChatGPT using device code authorization:\n\n1. Open this link in your browser and sign in to your account\n   %s\n\n2. Enter this one-time code (expires in 15 minutes)\n   %s\n\nContinue only if you started this login in qswitch. If a website or another person gave you this code, cancel.\n", url, code)
			})
		case adapter.Grok:
			fmt.Fprintln(os.Stderr, "加号使用官方 Grok Device Code。不要在当前 grok 里换号，不要跑 grok logout。")
			id, lerr = a.LoginGrok(ctx, func(code, url string) {
				fmt.Printf("\nTo sign in, open this URL in your browser:\n   %s\n\nThen enter this code:\n   %s\n\nContinue only if you started this login in qswitch. If a website or another person gave you this code, cancel.\n", url, code)
			})
		default:
			fmt.Fprintln(os.Stderr, "only `qswitch login codex` and `qswitch login grok` are implemented")
			return 2
		}
		if lerr != nil {
			return fail(lerr)
		}
		fmt.Printf("enrolled %s %s %s (live auth.json untouched)\n", t, id.Email, id.StableID)
		return 0
	case "forget":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: qswitch forget <tool> <id|email>")
			return 2
		}
		t, err := adapter.ParseTool(rest[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return fail(a.Forget(t, rest[1]))
	case "version", "--version":
		fmt.Println("qswitch 0.1.0")
		return 0
	default:
		usage()
		return 2
	}
}

func setAuto(a *app.App, rest []string, on bool) int {
	name := ""
	if t, ok := flagTool(rest); ok {
		name = string(t)
	}
	if on && name == "cursor" {
		p, _ := a.State.GetPointer("cursor")
		if p.Desync {
			fmt.Fprintln(os.Stderr, "cursor still DESYNC; quit Cursor.app then switch --force-align from desktop")
			return 1
		}
	}
	if name == "" {
		a.Cfg.General.AutoSwitch = on
	} else {
		if err := a.Cfg.SetToolEnabled(name, on); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if on {
			a.Cfg.General.AutoSwitch = true
		}
	}
	return fail(a.SaveConfig())
}

func flagVal(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"="), true
		}
	}
	return "", false
}

func flagTool(args []string) (adapter.Tool, bool) {
	for i, a := range args {
		if a == "--tool" && i+1 < len(args) {
			t, err := adapter.ParseTool(args[i+1])
			return t, err == nil
		}
		if strings.HasPrefix(a, "--tool=") {
			t, err := adapter.ParseTool(strings.TrimPrefix(a, "--tool="))
			return t, err == nil
		}
	}
	return "", false
}

func has(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func fail(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, err)
	return 1
}

func lookSelf() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	p, _ = filepath.EvalSymlinks(p)
	return p
}

func lookSibling(name string) string {
	dir := filepath.Dir(lookSelf())
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func usage() {
	fmt.Fprintf(os.Stderr, `qswitch — 多账号配额轮换器

  qswitch init
  qswitch capture [--tool codex|grok|cursor]
  qswitch login codex          # Device Code 加号，不登出当前 ChatGPT 会话
  qswitch login grok           # Device Code 加号，不登出当前 Grok 会话
  qswitch list [--tool ...]
  qswitch status
  qswitch probe [--tool codex|grok|cursor|devin]
  qswitch switch <tool> <id|email> [--force-align] [--allow-stale-cli] [--kill-cli]
  qswitch apply [--tool ...] [--kill-cli]
  qswitch enable-auto [--tool ...]
  qswitch disable-auto [--tool ...]
  qswitch forget <tool> <id|email>
  qswitch doctor
  qswitch serve [--addr 127.0.0.1:7432]
`)
}
