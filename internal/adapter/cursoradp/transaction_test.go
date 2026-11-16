package cursoradp

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"qswitch/internal/adapter"
	"testing"
	"time"
)

func TestDesktopTurnCannotBeInferredFromAbsentCLI(t *testing.T) {
	ad := Adapter{Platform: "darwin", List: func() ([]adapter.Proc, error) {
		return []adapter.Proc{{PID: 42, Command: `"C:\Cursor\Cursor.exe"`}}, nil
	}}
	if ad.Idle(t.TempDir(), time.Second) {
		t.Fatal("desktop was assumed idle because no CLI existed")
	}
}

func TestUndoReportsFailedCredentialRollback(t *testing.T) {
	t.Setenv("QSWITCH_IN_TEST", "1")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(cliAuth(home)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliAuth(home), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ad := Adapter{Platform: "darwin", List: func() ([]adapter.Proc, error) { return nil, nil }}
	undo, err := ad.RestoreWithRollback(home, transactionBlob(t), adapter.RestoreOpts{CLIOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cliAuth(home)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cliAuth(home), 0700); err != nil {
		t.Fatal(err)
	}
	if err := undo(); err == nil {
		t.Fatal("failed rollback was silently discarded")
	}
}

func transactionBlob(t *testing.T) adapter.Blob {
	t.Helper()
	id := adapter.Identity{Tool: adapter.Cursor, StableID: "target"}
	p := payload{Identity: id, Desktop: map[string]string{"cursorAuth/accessToken": "target-token", "cursorAuth/stripeMembershipAuthId": "target"}, CLIAuth: map[string]string{"accessToken": "target-token"}, CLIInfo: map[string]any{"authId": "target"}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return adapter.Blob{Tool: adapter.Cursor, Identity: id, Payload: raw}
}

func TestRestoreUndoPreservesAbsentFilesAndUnrelatedDB(t *testing.T) {
	for _, cliOnly := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			t.Run(map[bool]string{false: "full", true: "cli"}[cliOnly]+map[bool]string{false: "-absent", true: "-existing"}[exists], func(t *testing.T) {
				t.Setenv("QSWITCH_IN_TEST", "1")
				home := t.TempDir()
				ad := Adapter{Platform: "darwin", List: func() ([]adapter.Proc, error) { return nil, nil }}
				if exists {
					if err := writeDesktop(home, map[string]string{"cursorAuth/accessToken": "old-token", "cursorAuth/cachedEmail": "old@example.test"}); err != nil {
						t.Fatal(err)
					}
					db, err := sql.Open("sqlite", vscdb(home))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec(`INSERT INTO ItemTable(key,value) VALUES('unrelated','preserve')`); err != nil {
						t.Fatal(err)
					}
					db.Close()
					if err := os.MkdirAll(filepath.Dir(cliAuth(home)), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(cliAuth(home), []byte("old exact credentials"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(cliCfg(home), []byte(`{"theme":"preserve"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				undo, err := ad.RestoreWithRollback(home, transactionBlob(t), adapter.RestoreOpts{CLIOnly: cliOnly})
				if err != nil {
					t.Fatal(err)
				}
				if err := undo(); err != nil {
					t.Fatal(err)
				}
				if !exists {
					for _, path := range []string{cliAuth(home), cliCfg(home), vscdb(home)} {
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatal("undo did not restore absence")
						}
					}
					return
				}
				if raw, _ := os.ReadFile(cliAuth(home)); string(raw) != "old exact credentials" {
					t.Fatal("lost old CLI bytes")
				}
				if raw, _ := os.ReadFile(cliCfg(home)); string(raw) != `{"theme":"preserve"}` {
					t.Fatal("lost old config bytes")
				}
				desk, err := readDesktop(home)
				if err != nil || desk["cursorAuth/accessToken"] != "old-token" || desk["cursorAuth/stripeMembershipAuthId"] != "" {
					t.Fatal("DB auth fields were not restored exactly")
				}
				db, err := sql.Open("sqlite", vscdb(home))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var value string
				if err := db.QueryRow(`SELECT value FROM ItemTable WHERE key='unrelated'`).Scan(&value); err != nil || value != "preserve" {
					t.Fatal("unrelated database data changed")
				}
			})
		}
	}
}

func TestUnreadableOrMalformedConfigPreventsPartialSwitch(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "malformed", true: "nonregular"}[directory], func(t *testing.T) {
			t.Setenv("QSWITCH_IN_TEST", "1")
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(cliAuth(home)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cliAuth(home), []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			if directory {
				if err := os.Mkdir(cliCfg(home), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(cliCfg(home), []byte("invalid JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ad := Adapter{Platform: "darwin", List: func() ([]adapter.Proc, error) { return nil, nil }}
			if _, err := ad.RestoreWithRollback(home, transactionBlob(t), adapter.RestoreOpts{CLIOnly: true}); err == nil {
				t.Fatal("unsafe config accepted")
			}
			if raw, _ := os.ReadFile(cliAuth(home)); string(raw) != "old" {
				t.Fatal("CLI credential partially switched")
			}
		})
	}
}
