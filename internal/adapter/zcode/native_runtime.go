package zcode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var (
	ErrNativeRuntimeUnavailable = errors.New("zcode: 未找到可用的 ZCode CLI 配套 Node 和原生密钥模块，请确认该 CLI 已安装完整")
	ErrNativeRuntimeAmbiguous   = errors.New("zcode: 找到多套 ZCode CLI 配套运行时，无法确认当前使用哪一套")
)

type nativeRuntimeOptions struct {
	home, path, node string
	env              []string
}
type nativeRuntime struct{ node, module string }
type nativeNodeRunner func(context.Context, string, []string, []string) ([]byte, error)

// Do not load the CLI entrypoint or desktop bundle: either can initialize or
// migrate credentials. The native binding reads exactly one existing item.
const nativeKeyReadScript = `let Entry;
try { Entry = require(process.argv[1]).Entry; if (typeof Entry !== "function") throw new Error(); }
catch { process.exit(12); }
try {
  const value = new Entry(process.argv[2], process.argv[3]).getPassword();
  if (typeof value !== "string") process.exitCode = 13;
  else if (!/^[A-Za-z0-9_-]{43}$/.test(value)) process.exitCode = 14;
  else process.stdout.write(value);
} catch { process.exitCode = 13; }`

func readNodeCredential(ctx context.Context, opts nativeRuntimeOptions, service, account string, run nativeNodeRunner) (string, error) {
	rt, err := discoverNativeRuntime(ctx, opts, run)
	if err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ErrNativeRuntimeUnavailable
	}
	raw, err := run(ctx, rt.node, []string{"-e", nativeKeyReadScript, rt.module, service, account}, nativeNodeEnvironment(opts.env))
	if ctx.Err() != nil {
		return "", ErrOSCredentialPending
	}
	if err != nil {
		var exited interface{ ExitCode() int }
		if errors.As(err, &exited) {
			switch exited.ExitCode() {
			case 12:
				return "", ErrNativeRuntimeUnavailable
			case 14:
				return "", ErrOSCredentialFormat
			}
		}
		return "", ErrOSCredential
	}
	value := string(raw)
	key, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(value) != 43 || len(key) != 32 || base64.RawURLEncoding.EncodeToString(key) != value {
		return "", ErrOSCredentialFormat
	}
	return value, nil
}

func discoverNativeRuntime(ctx context.Context, opts nativeRuntimeOptions, run nativeNodeRunner) (nativeRuntime, error) {
	// ZCODE_NODE is the existing CLI override, not a qswitch-specific setting.
	requested := strings.TrimSpace(opts.node)
	if requested == "" {
		requested = "node"
	}
	if node := findExecutable(requested, opts.path); node != "" {
		actual, err := resolveNodeExecutable(ctx, node, opts.env, run)
		if err != nil {
			return nativeRuntime{}, ErrNativeRuntimeUnavailable
		}
		roots := []string{nodePackageRoot(node), nodePackageRoot(actual)}
		// A zcode symlink can expose an npm prefix independent of Node's prefix.
		for _, dir := range filepath.SplitList(opts.path) {
			if dir == "" || !filepath.IsAbs(dir) {
				continue
			}
			if target, err := filepath.EvalSymlinks(filepath.Join(dir, "zcode")); err == nil {
				roots = append(roots, enclosingCLIPackage(target))
			}
		}
		modules := uniqueRuntimeModules(roots)
		if len(modules) == 1 {
			return nativeRuntime{node: actual, module: modules[0]}, nil
		}
		if len(modules) > 1 {
			return nativeRuntime{}, ErrNativeRuntimeAmbiguous
		}
		// An explicit CLI Node selection must not silently use another install.
		if strings.TrimSpace(opts.node) != "" {
			return nativeRuntime{}, ErrNativeRuntimeUnavailable
		}
	} else if strings.TrimSpace(opts.node) != "" {
		return nativeRuntime{}, ErrNativeRuntimeUnavailable
	}
	// launchd often has a minimal PATH. Inspect installed, paired npm layouts
	// without choosing a version or invoking any installer/version manager.
	var installed []nativeRuntime
	seen := map[string]bool{}
	for _, pattern := range []string{
		filepath.Join(opts.home, ".local", "share", "mise", "installs", "node", "*", "bin", "node"),
		filepath.Join(opts.home, ".nvm", "versions", "node", "*", "bin", "node"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, node := range matches {
			if !executableFile(node) {
				continue
			}
			module := nativeModuleAt(nodePackageRoot(node))
			if module == "" {
				continue
			}
			resolved, err := filepath.EvalSymlinks(node)
			if err != nil {
				continue
			}
			identity := resolved + "\x00" + module
			if seen[identity] {
				continue
			}
			seen[identity] = true
			installed = append(installed, nativeRuntime{node: resolved, module: module})
		}
	}
	if len(installed) > 1 {
		return nativeRuntime{}, ErrNativeRuntimeAmbiguous
	}
	if len(installed) == 0 {
		return nativeRuntime{}, ErrNativeRuntimeUnavailable
	}
	actual, err := resolveNodeExecutable(ctx, installed[0].node, opts.env, run)
	if err != nil {
		return nativeRuntime{}, ErrNativeRuntimeUnavailable
	}
	installed[0].node = actual
	return installed[0], nil
}

func resolveNodeExecutable(ctx context.Context, node string, env []string, run nativeNodeRunner) (string, error) {
	raw, err := run(ctx, node, []string{"-p", "process.execPath"}, nativeNodeEnvironment(env))
	if err != nil {
		return "", ErrNativeRuntimeUnavailable
	}
	actual := strings.TrimSuffix(string(raw), "\n")
	if !filepath.IsAbs(actual) || !executableFile(actual) {
		return "", ErrNativeRuntimeUnavailable
	}
	return filepath.Clean(actual), nil
}

func findExecutable(name, path string) string {
	if filepath.IsAbs(name) {
		if executableFile(name) {
			return filepath.Clean(name)
		}
		return ""
	}
	if strings.ContainsAny(name, `/\`) {
		return ""
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if executableFile(candidate) {
			return candidate
		}
	}
	return ""
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}

func nodePackageRoot(node string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(node)), "lib", "node_modules", "zcode-app-cli")
}

func enclosingCLIPackage(entry string) string {
	dir := filepath.Dir(entry)
	for i := 0; i < 8; i++ {
		if filepath.Base(dir) == "zcode-app-cli" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func uniqueRuntimeModules(roots []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, root := range roots {
		if module := nativeModuleAt(root); module != "" && !seen[module] {
			seen[module] = true
			result = append(result, module)
		}
	}
	return result
}

func nativeModuleAt(root string) string {
	if root == "" || !packageNamed(root, "zcode-app-cli") {
		return ""
	}
	module, err := filepath.EvalSymlinks(filepath.Join(root, "node_modules", "@napi-rs", "keyring"))
	if err != nil || !packageNamed(module, "@napi-rs/keyring") {
		return ""
	}
	info, err := os.Stat(filepath.Join(module, "index.js"))
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return module
}

func packageNamed(dir, name string) bool {
	f, err := os.Open(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return false
	}
	var manifest struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(raw, &manifest) == nil && manifest.Name == name
}

func nativeNodeEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if name == "NODE_OPTIONS" || name == "NODE_PATH" || strings.HasPrefix(name, "NAPI_RS_") || strings.HasPrefix(name, "DYLD_") || name == "LD_PRELOAD" || name == "LD_LIBRARY_PATH" {
			continue
		}
		result = append(result, entry)
	}
	return result
}

type nativeOutput struct{ buffer bytes.Buffer }

func (b *nativeOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 4096 {
		return 0, errors.New("native helper output exceeded limit")
	}
	return b.buffer.Write(p)
}

func runNativeNode(ctx context.Context, node string, args, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Env = env
	cmd.WaitDelay = time.Second
	var output nativeOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return output.buffer.Bytes(), nil
}
