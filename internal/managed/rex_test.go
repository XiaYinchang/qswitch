package managed

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// useStdin points the runner's input at a character device, so a fake ps
// can name that device as the block's terminal.
func useStdin(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}

// fakeRex installs a rex CLI that answers process for one named block, and a
// ps that reports tty as the terminal of any process.
func fakeRex(t *testing.T, reply, tty string) rexTerminal {
	t.Helper()
	ps, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	scripts := map[string]string{
		"rex": "#!/bin/sh\n[ \"$*\" = '--autostart=false --timeout=2s block call --session=session:s1 --block=block:b1 process' ] || exit 2\nprintf '%s\\n' " + shellQuote(reply) + "\n",
		"ps":  "#!/bin/sh\nif [ \"$1 $2\" = '-o tty=' ]; then printf '%s\\n' " + shellQuote(tty) + "; exit 0; fi\nexec " + shellQuote(ps) + " \"$@\"\n",
	}
	for name, script := range scripts {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		// The first run of a new executable can stall while the system scans
		// it; keep that out of the 2 s budget of the check under test.
		_ = exec.Command(path, "-o", "tty=").Run()
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return rexTerminal{session: "session:s1", block: "block:b1", binDir: dir}
}

func TestRexBlockVerification(t *testing.T) {
	useStdin(t, os.DevNull)
	own := `{"last_exit":null,"child":{"pid":` + strconv.Itoa(os.Getpid()) + `}}`
	sibling := exec.Command("sleep", "30")
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sibling.Process.Kill(); _ = sibling.Wait() }()
	unrelated := `{"last_exit":null,"child":{"pid":` + strconv.Itoa(sibling.Process.Pid) + `}}`
	for _, c := range []struct{ name, reply, tty, want string }{
		{"own block", own, "null", ""},
		{"closed block", `{"last_exit":{"code":0},"child":null}`, "null", "no longer alive"},
		{"other terminal", own, "zero", "runner input is not the Rex block terminal"},
		{"process without terminal", own, "?", "cannot inspect Rex block terminal"},
		{"unrelated process", unrelated, "null", "not an ancestor"},
		{"unreadable reply", "not json", "null", "cannot verify Rex terminal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := verifiedRexBlock(fakeRex(t, c.reply, c.tty))
			if c.want == "" && err != nil {
				t.Fatalf("own block rejected: %v", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
		})
	}
	// Rex must be asked about the named block; it otherwise uses the focused one.
	rex := fakeRex(t, own, "null")
	rex.block = "block:b2"
	if err := verifiedRexBlock(rex); err == nil {
		t.Fatal("another block's answer accepted")
	}
	rex.block = "block:b1 --block=block:b2"
	if err := verifiedRexBlock(rex); err == nil || !strings.Contains(err.Error(), "invalid Rex terminal identity") {
		t.Fatalf("malformed block identity: %v", err)
	}
}

func TestTerminalAcceptsTmuxOrRex(t *testing.T) {
	useStdin(t, os.DevNull)
	own := `{"last_exit":null,"child":{"pid":` + strconv.Itoa(os.Getpid()) + `}}`
	if err := verifiedTerminal("", "", rexTerminal{}); err == nil || !strings.Contains(err.Error(), "tmux pane or Rex terminal") {
		t.Fatalf("plain terminal: %v", err)
	}
	t.Run("rex", func(t *testing.T) {
		if err := verifiedTerminal("", "", fakeRex(t, own, "null")); err != nil {
			t.Fatal(err)
		}
	})
	// A Rex server started from tmux passes its tmux variables to every terminal.
	t.Run("rex with inherited tmux variables", func(t *testing.T) {
		if err := verifiedTerminal("%1", "/nonexistent/tmux,1,0", fakeRex(t, own, "null")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("neither verified", func(t *testing.T) {
		closed := fakeRex(t, `{"last_exit":{"code":0},"child":null}`, "null")
		if err := verifiedTerminal("%1", "/nonexistent/tmux,1,0", closed); err == nil || !strings.Contains(err.Error(), "tmux pane") {
			t.Fatalf("tmux error not reported: %v", err)
		}
	})
}
