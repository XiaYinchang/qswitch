package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"qswitch/internal/app"
	"qswitch/internal/web"
)

func main() {
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
	if err := a.RunDaemon(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
