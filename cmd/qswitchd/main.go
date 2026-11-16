package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
	"qswitch/internal/buildinfo"
	"qswitch/internal/web"
)

func main() {
	version := flag.Bool("version", false, "print version")
	toolName := flag.String("tool", "", "manage only this tool (default: all configured tools)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: qswitchd [--tool <tool>] [--version]")
		os.Exit(2)
	}
	toolSpecified := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "tool" {
			toolSpecified = true
		}
	})
	if toolSpecified && *toolName == "" {
		fmt.Fprintln(os.Stderr, "--tool requires a value")
		os.Exit(2)
	}
	selected := adapter.AllTools()
	if *toolName != "" {
		tool, err := adapter.ParseTool(*toolName)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		selected = []adapter.Tool{tool}
	}
	if *version {
		fmt.Println(buildinfo.Version)
		return
	}
	home, _ := os.UserHomeDir()
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	dir := filepath.Dir(self)
	bins := []string{filepath.Join(dir, "qswitch"), self}
	a, err := app.Open(home, os.Getenv("QSWITCH_DIR"), nil, bins, nil, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer a.Close()
	if err := a.SetManagedTools(selected); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// journald stamps each line; one line per change in a switch's progress.
	a.Log = log.New(os.Stderr, "", 0).Printf
	if err := a.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if a.Cfg.Web.Enabled {
		addr := a.Cfg.WebAddr()
		go func() {
			fmt.Fprintln(os.Stderr, "web http://"+addr)
			if err := web.New(a, addr).Run(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "web:", err)
			}
		}()
	}
	if err := a.RunDaemonTools(ctx, selected); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
