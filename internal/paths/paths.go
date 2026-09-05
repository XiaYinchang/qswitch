package paths

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	RelDataDir  = ".qswitch"
	EnvDataDir  = "QSWITCH_DIR"
	LaunchLabel = "com.qswitch"
	LogFile     = "qswitch.log"
	ErrLogFile  = "qswitch.error.log"
	PlistName   = "com.qswitch.plist"
	DefaultBin  = "qswitchd"
)

func DataDir(home string) string {
	if d := strings.TrimSpace(os.Getenv(EnvDataDir)); d != "" {
		return d
	}
	return filepath.Join(home, RelDataDir)
}

func VaultDir(dataDir string) string { return filepath.Join(dataDir, "vault") }
func LocksDir(dataDir string) string { return filepath.Join(dataDir, "locks") }

func LogsDir(home string) string {
	return filepath.Join(home, "Library", "Logs")
}

func LogPath(home string) string    { return filepath.Join(LogsDir(home), LogFile) }
func ErrLogPath(home string) string { return filepath.Join(LogsDir(home), ErrLogFile) }

func DefaultBinDir(home string) string {
	return filepath.Join(home, ".local", "bin")
}

func LaunchAgentsDir(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents")
}

func LaunchAgentPath(home string) string {
	return filepath.Join(LaunchAgentsDir(home), PlistName)
}

func GeneratedPlistPath(dataDir string) string {
	return filepath.Join(dataDir, PlistName)
}
