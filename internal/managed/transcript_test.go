package managed

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSavedSessionSearchesEveryProject(t *testing.T) {
	const session = "0b7c3f9e-5d1a-4c2b-9e8f-1a2b3c4d5e6f"
	home := t.TempDir()
	t.Setenv("HOME", home)
	if savedSession(session) {
		t.Fatal("saved without a projects directory")
	}
	projects := filepath.Join(home, ".claude", "projects")
	for _, project := range []string{"-a", "-b"} {
		if err := os.MkdirAll(filepath.Join(projects, project), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Stray files such as .DS_Store are not projects.
	if err := os.WriteFile(filepath.Join(projects, ".DS_Store"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if savedSession(session) {
		t.Fatal("saved without a transcript")
	}
	transcript := filepath.Join(projects, "-b", session+".jsonl")
	if err := os.WriteFile(transcript, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if !savedSession(session) {
		t.Fatal("transcript in another project not found")
	}
	if err := os.Remove(transcript); err != nil {
		t.Fatal(err)
	}
	// Doubt keeps --resume.
	for _, id := range []string{"", "session-one", "../" + session, session[:8] + "x" + session[9:], session + "0"} {
		if !savedSession(id) {
			t.Errorf("%q counted as unsaved", id)
		}
	}
	if err := os.Chmod(projects, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(projects, 0700) })
	if os.Geteuid() != 0 && !savedSession(session) {
		t.Fatal("unreadable projects directory counted as unsaved")
	}
}
