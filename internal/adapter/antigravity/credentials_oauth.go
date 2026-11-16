package antigravity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"qswitch/internal/busy"
)

// Client IDs and hashes identify the public installed-application OAuth
// configuration verified in Antigravity 2.32.1. The shared client secret is
// read from the installed native executable into memory, never shipped in
// qswitch's source, persisted, printed, or included in an error.
const (
	consumerClientID   = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	gcpClientID        = "884354919052-36trc1jjb3tguiac32ov6cod268c5blh.apps.googleusercontent.com"
	consumerSecretHash = "1d2f041093fd95aa8995a038c711d50a7960da09a505381c09a745d6ad0ecc60"
	gcpSecretHash      = "a2ddedc84850a9c48b7e01d61700848083bf0e10fb8d6c99c496803e8b48ced0"
)

type OAuthConfig struct{ ClientID, ClientSecret string }

var nativeOAuthCache struct {
	sync.Mutex
	configs map[string]OAuthConfig
}

func loadNativeOAuthConfig(method string) (OAuthConfig, error) {
	id, hash := consumerClientID, consumerSecretHash
	if method == "gcp" {
		id, hash = gcpClientID, gcpSecretHash
	}
	nativeOAuthCache.Lock()
	defer nativeOAuthCache.Unlock()
	if config, ok := nativeOAuthCache.configs[id]; ok {
		return config, nil
	}
	for _, path := range nativeOAuthExecutables() {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		st, statErr := file.Stat()
		if statErr != nil || !st.Mode().IsRegular() || st.Size() > 512<<20 {
			file.Close()
			continue
		}
		config, err := scanOAuthConfig(file, id, hash)
		file.Close()
		if err != nil {
			continue
		}
		if nativeOAuthCache.configs == nil {
			nativeOAuthCache.configs = make(map[string]OAuthConfig)
		}
		nativeOAuthCache.configs[id] = config
		return config, nil
	}
	return OAuthConfig{}, errors.New("supported OAuth configuration was not found in the installed Antigravity client; install or update the official agy CLI")
}

func nativeOAuthExecutables() []string {
	var paths []string
	if p, err := exec.LookPath("agy"); err == nil {
		paths = append(paths, p)
	}
	home, _ := os.UserHomeDir()
	// A portable/App-only install may not expose agy on PATH. Resolve the
	// actual native process image by PID, never log its command arguments.
	if processes, err := (busy.PS{}).List(); err == nil {
		for _, process := range processes {
			lower := strings.ToLower(process.Command)
			if !strings.Contains(lower, "language_server") && !strings.Contains(lower, "agy") {
				continue
			}
			path := credentialProcessExecutable(process.PID)
			if nativeOAuthExecutableName(path) {
				paths = append(paths, path)
			}
		}
	}
	paths = append(paths, filepath.Join(home, ".local", "bin", "agy"), filepath.Join(home, ".gemini", "antigravity", "bin", "language_server"))
	if runtime.GOOS == "darwin" {
		paths = append(paths, "/Applications/Antigravity.app/Contents/Resources/bin/language_server", filepath.Join(home, "Applications", "Antigravity.app", "Contents", "Resources", "bin", "language_server"))
	}
	if runtime.GOOS == "windows" {
		paths = append(paths, filepath.Join(home, ".local", "bin", "agy.exe"), filepath.Join(home, ".gemini", "antigravity", "bin", "language_server.exe"))
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs")} {
			if root != "" {
				paths = append(paths, filepath.Join(root, "Antigravity", "resources", "bin", "language_server.exe"))
			}
		}
	}
	if runtime.GOOS == "linux" {
		for _, root := range []string{"/opt/Antigravity", "/opt/antigravity", "/usr/share/antigravity", "/usr/lib/antigravity", filepath.Join(home, ".local", "share", "antigravity")} {
			paths = append(paths, filepath.Join(root, "resources", "bin", "language_server"))
		}
	}
	return paths
}

func nativeOAuthExecutableName(path string) bool {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(path, `\`, "/"))), ".exe")
	switch name {
	case "agy", "language_server", "language_server_macos", "language_server_macos_arm", "language_server_macos_arm64", "language_server_linux_x64", "language_server_linux_arm", "language_server_linux_arm64", "language_server_windows_x64", "language_server_windows_arm64":
		return true
	}
	return false
}

func scanOAuthConfig(reader io.Reader, clientID, expectedHash string) (OAuthConfig, error) {
	buf := make([]byte, 64<<10)
	var tail []byte
	foundID := false
	secret := ""
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			block := append(tail, buf[:n]...)
			if bytes.Contains(block, []byte(clientID)) {
				foundID = true
			}
			for offset := 0; offset < len(block); {
				i := bytes.Index(block[offset:], []byte("GOCSPX-"))
				if i < 0 {
					break
				}
				i += offset
				if i+35 <= len(block) {
					candidate := block[i : i+35]
					sum := sha256.Sum256(candidate)
					if hex.EncodeToString(sum[:]) == expectedHash {
						secret = string(candidate)
					}
				}
				offset = i + 7
			}
			if foundID && secret != "" {
				return OAuthConfig{clientID, secret}, nil
			}
			keep := len(clientID) + 35
			if keep > len(block) {
				keep = len(block)
			}
			tail = append([]byte(nil), block[len(block)-keep:]...)
		}
		if err != nil {
			break
		}
	}
	return OAuthConfig{}, errors.New("native OAuth configuration did not match the supported application")
}
