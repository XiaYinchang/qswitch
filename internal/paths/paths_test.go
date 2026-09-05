package paths

import (
	"path/filepath"
	"testing"
)

func TestDataDirDefaultAndEnv(t *testing.T) {
	t.Setenv(EnvDataDir, "")
	if got := DataDir("/home/me"); got != filepath.Join("/home/me", RelDataDir) {
		t.Fatalf("default %s", got)
	}
	t.Setenv(EnvDataDir, "/tmp/qs")
	if got := DataDir("/home/me"); got != "/tmp/qs" {
		t.Fatalf("env %s", got)
	}
}

func TestStandardLayout(t *testing.T) {
	home := "/Users/example"
	if LaunchLabel != "com.qswitch" {
		t.Fatal(LaunchLabel)
	}
	if LaunchAgentPath(home) != filepath.Join(home, "Library", "LaunchAgents", PlistName) {
		t.Fatal(LaunchAgentPath(home))
	}
	if LogPath(home) != filepath.Join(home, "Library", "Logs", LogFile) {
		t.Fatal(LogPath(home))
	}
}
