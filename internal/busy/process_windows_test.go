package busy

import (
	"os"
	"testing"
)

func TestWindowsProcessSnapshot(t *testing.T) {
	raw := []byte(`[{"ProcessId":42,"CommandLine":"\"C:\\Apps\\Antigravity.exe\" --reuse-window","ExecutablePath":"C:\\Apps\\Antigravity.exe","Name":"Antigravity.exe"},{"ProcessId":43,"CommandLine":null,"ExecutablePath":"C:\\Apps\\idle.exe","Name":"idle.exe"}]`)
	rows, err := parseWindowsProcesses(raw)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].PID != 42 || rows[0].Command != `"C:\Apps\Antigravity.exe" --reuse-window` {
		t.Fatalf("command lost: %+v", rows[0])
	}
	if rows[1].Command != `C:\Apps\idle.exe` {
		t.Fatalf("fallback lost: %+v", rows[1])
	}
}

func TestWindowsProcessListIncludesCaller(t *testing.T) {
	processes, err := (PS{}).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		if process.PID == os.Getpid() {
			return
		}
	}
	t.Fatal("native process snapshot omitted caller")
}

func TestWindowsAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("caller was not alive")
	}
	if Alive(-1) {
		t.Fatal("invalid pid was alive")
	}
}
