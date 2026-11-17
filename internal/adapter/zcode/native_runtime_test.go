package zcode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func nativeFixtureFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func nativeRuntimeFixture(t *testing.T, prefix string) nativeRuntime {
	t.Helper()
	node := filepath.Join(prefix, "bin", "node")
	nativeFixtureFile(t, node, "synthetic executable; never run", 0o700)
	root := nodePackageRoot(node)
	nativeFixtureFile(t, filepath.Join(root, "package.json"), `{"name":"zcode-app-cli"}`, 0o600)
	module := filepath.Join(root, "node_modules", "@napi-rs", "keyring")
	nativeFixtureFile(t, filepath.Join(module, "package.json"), `{"name":"@napi-rs/keyring","main":"index.js"}`, 0o600)
	nativeFixtureFile(t, filepath.Join(module, "index.js"), "synthetic module; never run", 0o600)
	return nativeRuntime{node: node, module: module}
}

func TestNativeRuntimeDiscoveryUsesPairedNodeAndOneRead(t *testing.T) {
	for _, mode := range []string{"PATH", "shim", "explicit override", "minimal PATH mise", "minimal PATH nvm"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			prefix := filepath.Join(home, "runtime with spaces", "version-a")
			if mode == "minimal PATH mise" {
				prefix = filepath.Join(home, ".local", "share", "mise", "installs", "node", "version-a")
			}
			if mode == "minimal PATH nvm" {
				prefix = filepath.Join(home, ".nvm", "versions", "node", "version-a")
			}
			rt := nativeRuntimeFixture(t, prefix)
			opts := nativeRuntimeOptions{home: home, path: filepath.Dir(rt.node), env: []string{"PATH=fixture", "NODE_OPTIONS=must-not-run", "NODE_PATH=wrong-module", "NAPI_RS_NATIVE_LIBRARY_PATH=wrong-library", "DYLD_INSERT_LIBRARIES=wrong-library", "KEEP=ok"}}
			probeNode := rt.node
			switch mode {
			case "shim":
				probeNode = filepath.Join(home, "shims", "node")
				nativeFixtureFile(t, probeNode, "synthetic shim", 0o700)
				opts.path = filepath.Dir(probeNode)
			case "explicit override":
				opts.path = ""
				opts.node = "  " + rt.node + "  "
			case "minimal PATH mise", "minimal PATH nvm":
				opts.path = ""
			}
			calls, reads := 0, 0
			value := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			got, err := readNodeCredential(context.Background(), opts, "synthetic-service", "synthetic-account", func(_ context.Context, node string, args, env []string) ([]byte, error) {
				calls++
				if !reflect.DeepEqual(env, []string{"PATH=fixture", "KEEP=ok"}) {
					t.Fatalf("injected Node environment survived: %v", env)
				}
				if reflect.DeepEqual(args, []string{"-p", "process.execPath"}) {
					if node != probeNode {
						t.Fatal("probed a different Node install")
					}
					return []byte(rt.node + "\n"), nil
				}
				reads++
				if node != rt.node || !reflect.DeepEqual(args, []string{"-e", nativeKeyReadScript, rt.module, "synthetic-service", "synthetic-account"}) {
					t.Fatal("read did not use the paired runtime and exact identity")
				}
				return []byte(value), nil
			})
			if err != nil || got != value || calls != 2 || reads != 1 {
				t.Fatalf("read result err=%v calls=%d reads=%d", err, calls, reads)
			}
		})
	}
}

func TestNativeRuntimeDiscoveryRejectsMissingAmbiguousAndWrongPackages(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous", "wrong CLI package", "wrong keyring package", "explicit missing Node", "failed shim"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			opts := nativeRuntimeOptions{home: home}
			want := ErrNativeRuntimeUnavailable
			if mode != "missing" {
				rt := nativeRuntimeFixture(t, filepath.Join(home, ".local", "share", "mise", "installs", "node", "version-a"))
				switch mode {
				case "ambiguous":
					nativeRuntimeFixture(t, filepath.Join(home, ".local", "share", "mise", "installs", "node", "version-b"))
					want = ErrNativeRuntimeAmbiguous
				case "wrong CLI package":
					nativeFixtureFile(t, filepath.Join(nodePackageRoot(rt.node), "package.json"), `{"name":"other-cli"}`, 0o600)
				case "wrong keyring package":
					nativeFixtureFile(t, filepath.Join(rt.module, "package.json"), `{"name":"other-keyring"}`, 0o600)
				case "explicit missing Node":
					opts.node = filepath.Join(home, "missing-node")
				case "failed shim":
					opts.path = filepath.Dir(rt.node)
				}
			}
			calls := 0
			_, err := readNodeCredential(context.Background(), opts, "synthetic-service", "synthetic-account", func(_ context.Context, _ string, args, _ []string) ([]byte, error) {
				calls++
				if mode != "failed shim" || !reflect.DeepEqual(args, []string{"-p", "process.execPath"}) {
					t.Fatal("discovery attempted an unexpected runtime or key read")
				}
				return nil, errors.New("sensitive-backend-output")
			})
			if !errors.Is(err, want) || strings.Contains(err.Error(), "sensitive-backend-output") || mode != "failed shim" && calls != 0 {
				t.Fatalf("discovery err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestNativeRuntimeFindsCLISymlinkWithoutExecutingCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS symlink layout")
	}
	home := t.TempDir()
	rt := nativeRuntimeFixture(t, filepath.Join(home, "npm-prefix"))
	node := filepath.Join(home, "separate-node", "bin", "node")
	nativeFixtureFile(t, node, "synthetic executable", 0o700)
	cli := filepath.Join(nodePackageRoot(rt.node), "bin", "zcode.js")
	nativeFixtureFile(t, cli, "throw new Error('CLI must not run')", 0o600)
	if err := os.Symlink(cli, filepath.Join(filepath.Dir(node), "zcode")); err != nil {
		t.Fatal(err)
	}
	got, err := discoverNativeRuntime(context.Background(), nativeRuntimeOptions{home: home, path: filepath.Dir(node)}, func(_ context.Context, exe string, args, _ []string) ([]byte, error) {
		if exe != node || !reflect.DeepEqual(args, []string{"-p", "process.execPath"}) {
			t.Fatal("executed CLI or shell wrapper during discovery")
		}
		return []byte(node + "\n"), nil
	})
	if err != nil || got.node != node || got.module != rt.module {
		t.Fatalf("symlink discovery = %+v, %v", got, err)
	}
}

func TestNativeRuntimeDeduplicatesMiseVersionAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS symlink layout")
	}
	home := t.TempDir()
	installs := filepath.Join(home, ".local", "share", "mise", "installs", "node")
	rt := nativeRuntimeFixture(t, filepath.Join(installs, "version-full"))
	for _, alias := range []string{"latest", "major", "major-minor"} {
		if err := os.Symlink("version-full", filepath.Join(installs, alias)); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	got, err := discoverNativeRuntime(context.Background(), nativeRuntimeOptions{home: home}, func(_ context.Context, node string, args, _ []string) ([]byte, error) {
		calls++
		if node != rt.node || !reflect.DeepEqual(args, []string{"-p", "process.execPath"}) {
			t.Fatal("version aliases selected a different runtime")
		}
		return []byte(rt.node + "\n"), nil
	})
	if err != nil || calls != 1 || got.node != rt.node || got.module != rt.module {
		t.Fatalf("same-install aliases were ambiguous: runtime=%+v calls=%d err=%v", got, calls, err)
	}
}

type fixtureNodeExit int

func (e fixtureNodeExit) Error() string { return "backend message must not escape" }
func (e fixtureNodeExit) ExitCode() int { return int(e) }

func TestNativeKeyReadFailureNeverTriesAnotherExecutable(t *testing.T) {
	for _, mode := range []string{"denied", "missing binding", "invalid key", "timeout", "bad stdout"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			rt := nativeRuntimeFixture(t, filepath.Join(home, "chosen"))
			// Another complete install must never be tried after a read fails.
			nativeRuntimeFixture(t, filepath.Join(home, ".local", "share", "mise", "installs", "node", "other"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			want := ErrOSCredential
			_, err := readNodeCredential(ctx, nativeRuntimeOptions{home: home, path: filepath.Dir(rt.node)}, "synthetic-service", "synthetic-account", func(_ context.Context, exe string, args, _ []string) ([]byte, error) {
				if exe != rt.node {
					t.Fatal("retried using another executable")
				}
				if len(args) == 2 && args[0] == "-p" {
					return []byte(rt.node + "\n"), nil
				}
				reads++
				switch mode {
				case "missing binding":
					want = ErrNativeRuntimeUnavailable
					return nil, fixtureNodeExit(12)
				case "invalid key":
					want = ErrOSCredentialFormat
					return nil, fixtureNodeExit(14)
				case "timeout":
					want = ErrOSCredentialPending
					cancel()
					return nil, context.Canceled
				case "bad stdout":
					want = ErrOSCredentialFormat
					return []byte("sensitive-backend-output"), nil
				default:
					return nil, fixtureNodeExit(13)
				}
			})
			if !errors.Is(err, want) || reads != 1 || strings.Contains(err.Error(), "backend") {
				t.Fatalf("read err=%v count=%d", err, reads)
			}
		})
	}
}

func TestNativeKeyReadScriptWithSyntheticModule(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	module := t.TempDir()
	value := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	encoded, _ := json.Marshal(value)
	nativeFixtureFile(t, filepath.Join(module, "index.js"), `let reads=0; module.exports.Entry=class {
constructor(service,account) { if(service!=="synthetic-service" || account!=="synthetic-account") throw Error(); }
getPassword() { if(++reads!==1) throw Error(); return `+string(encoded)+`; }
setPassword() { throw Error("must not write"); }
deleteCredential() { throw Error("must not delete"); }
};`, 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := runNativeNode(ctx, node, []string{"-e", nativeKeyReadScript, module, "synthetic-service", "synthetic-account"}, nativeNodeEnvironment(os.Environ()))
	if err != nil || string(raw) != value {
		t.Fatalf("synthetic native read failed: %v", err)
	}
}

func TestNativeRuntimeErrorsSurviveCredentialReaderWithoutBackendDetails(t *testing.T) {
	clearManagedSecret(t)
	for _, want := range []error{ErrNativeRuntimeUnavailable, ErrNativeRuntimeAmbiguous} {
		r := credentialReader{home: t.TempDir(), rawStore: "enc:v2:os:", keyringGet: func(string, string) (string, error) { return "", want }}
		if _, err := r.loadKey(); !errors.Is(err, want) {
			t.Fatalf("runtime diagnosis was lost: %v", err)
		}
	}
}

func TestNativeHelperOutputLimitAppliesToPipeCopy(t *testing.T) {
	var output nativeOutput
	if _, err := io.Copy(&output, strings.NewReader(strings.Repeat("x", 4097))); err == nil || output.buffer.Len() > 4096 {
		t.Fatal("helper output bypassed its size limit")
	}
}
