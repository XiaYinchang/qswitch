package cursoradp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"qswitch/internal/adapter"
)

func TestDesktopSupportDefaultsToHost(t *testing.T) {
	if got := (Adapter{}).SupportsDesktopSync(); got != (runtime.GOOS == "darwin") {
		t.Fatalf("desktop support = %v on %s", got, runtime.GOOS)
	}
}

func TestNonMacCaptureAndRestoreOnlyTouchCLI(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		for _, oldDesktop := range []bool{false, true} {
			t.Run(platform+map[bool]string{false: "-no-desktop", true: "-foreign-desktop"}[oldDesktop], func(t *testing.T) {
				home := t.TempDir()
				ad := Adapter{Platform: platform, List: func() ([]adapter.Proc, error) {
					return []adapter.Proc{{PID: 42, Command: `"C:\Cursor\Cursor.exe"`}}, nil
				}}
				oldAuth := []byte(`{"accessToken":"synthetic-old-token"}`)
				oldCfg := []byte(`{"theme":"preserve","authInfo":{"authId":"old-cli","email":"old@example.test"}}`)
				writeFixture := func(path string, raw []byte) {
					t.Helper()
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				writeFixture(cliAuth(home), oldAuth)
				writeFixture(cliCfg(home), oldCfg)
				foreignDesktop := []byte("not a database: must never be read or replaced")
				if oldDesktop {
					writeFixture(vscdb(home), foreignDesktop)
				}
				if paths := ad.LivePaths(home); !reflect.DeepEqual(paths, []string{cliAuth(home), cliCfg(home)}) {
					t.Fatalf("CLI paths = %v", paths)
				}
				blobs, warnings, err := ad.Capture(home)
				if err != nil || len(blobs) != 1 {
					t.Fatalf("capture: blobs=%d err=%v", len(blobs), err)
				}
				if blobs[0].Identity.StableID != "old-cli" || blobs[0].Incomplete || len(warnings) != 0 {
					t.Fatalf("CLI account incorrectly depends on desktop: id=%s incomplete=%v warnings=%v", blobs[0].Identity.StableID, blobs[0].Incomplete, warnings)
				}
				if err := ValidateCLI(blobs[0]); err != nil {
					t.Fatalf("captured CLI rejected by shared validator: %v", err)
				}
				var captured payload
				if err := json.Unmarshal(blobs[0].Payload, &captured); err != nil || len(captured.Desktop) != 0 {
					t.Fatalf("capture included desktop: %v", err)
				}
				fp, err := ad.Fingerprint(home)
				wantFP, wantErr := HashCLI(home)
				if err != nil || wantErr != nil || fp != wantFP {
					t.Fatalf("fingerprint depends on unsupported desktop: %v / %v", err, wantErr)
				}
				holders, err := ad.AutoBlockers(home)
				if err != nil || holders.ManualBusy() || holders.CursorApp || !ad.Idle(home, time.Second) {
					t.Fatalf("independent desktop blocked CLI switch: %+v / %v", holders, err)
				}
				// The caller need not know this host requires CLI-only restoration.
				undo, err := ad.RestoreWithRollback(home, transactionBlob(t), adapter.RestoreOpts{})
				if err != nil {
					t.Fatal(err)
				}
				current, _, err := ad.Capture(home)
				if err != nil || len(current) != 1 || current[0].Identity.StableID != "target" || current[0].Incomplete {
					t.Fatalf("target CLI identity not restored: %v", err)
				}
				checkDesktop := func() {
					t.Helper()
					if oldDesktop {
						raw, err := os.ReadFile(vscdb(home))
						if err != nil || !bytes.Equal(raw, foreignDesktop) {
							t.Fatal("foreign desktop changed")
						}
					} else if _, err := os.Stat(filepath.Join(home, "Library")); !os.IsNotExist(err) {
						t.Fatalf("created unsupported macOS data directory: %v", err)
					}
				}
				checkDesktop()
				if err := undo(); err != nil {
					t.Fatal(err)
				}
				for path, want := range map[string][]byte{cliAuth(home): oldAuth, cliCfg(home): oldCfg} {
					raw, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(raw, want) {
						t.Fatalf("CLI rollback lost exact original: %v", err)
					}
				}
				checkDesktop()
			})
		}
	}
}

func TestNonMacRestoreRequiresMatchingCLIIdentityAndToken(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		for _, mutation := range []string{"desktop-only", "wrong-cli-id", "missing-cli-id", "wrong-payload-id", "blank-cli-token", "missing-blob-id"} {
			t.Run(platform+"-"+mutation, func(t *testing.T) {
				home := t.TempDir()
				ad := Adapter{Platform: platform, List: func() ([]adapter.Proc, error) { return nil, nil }}
				blob := transactionBlob(t)
				var p payload
				if err := json.Unmarshal(blob.Payload, &p); err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "desktop-only":
					p.CLIAuth = nil
				case "wrong-cli-id":
					p.CLIInfo = map[string]any{"authId": "other-cli"}
				case "missing-cli-id":
					p.CLIInfo = nil
				case "wrong-payload-id":
					p.Identity.StableID = "other-account"
				case "blank-cli-token":
					p.CLIAuth["accessToken"] = " "
				case "missing-blob-id":
					blob.Identity.StableID = ""
				}
				var err error
				blob.Payload, err = json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateCLI(blob); err == nil {
					t.Fatal("shared validator accepted unverified CLI snapshot")
				}
				if token, err := CLIAccessToken(blob); err == nil || token != "" {
					t.Fatal("CLI quota credential selection bypassed validation")
				}
				if _, err := ad.RestoreWithRollback(home, blob, adapter.RestoreOpts{}); err == nil {
					t.Fatal("unverified native CLI snapshot accepted")
				}
				if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
					t.Fatalf("rejected restore wrote native data: %v", err)
				}
			})
		}
	}
}

func TestCLIAccessTokenNeverUsesDesktopToken(t *testing.T) {
	blob := transactionBlob(t)
	var p payload
	if err := json.Unmarshal(blob.Payload, &p); err != nil {
		t.Fatal(err)
	}
	p.Desktop["cursorAuth/accessToken"] = "unrelated-desktop-token"
	var err error
	blob.Payload, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	token, err := CLIAccessToken(blob)
	if err != nil || token != "target-token" {
		t.Fatalf("quota selected a credential outside the CLI plane: %v", err)
	}
}

func TestNonMacCLIStillBlocksSwitch(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			ad := Adapter{Platform: platform, List: func() ([]adapter.Proc, error) {
				return []adapter.Proc{{PID: 43, Command: `"C:\Agent\cursor-agent.exe"`}}, nil
			}}
			home := t.TempDir()
			holders, err := ad.ManualBlockers(home)
			if err != nil || !holders.ManualBusy() || ad.Idle(home, time.Second) {
				t.Fatalf("CLI without activity evidence was not blocked: %+v / %v", holders, err)
			}
			if _, err := ad.RestoreWithRollback(home, transactionBlob(t), adapter.RestoreOpts{}); err == nil {
				t.Fatal("running CLI accepted a credential write")
			}
		})
	}
}
