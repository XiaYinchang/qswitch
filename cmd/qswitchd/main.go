package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"qswitch/internal/app"
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
	if err := a.RunDaemon(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
