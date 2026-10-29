package managed

import (
	"reflect"
	"strings"
	"testing"
)

func TestResumePreservesPermissionsWithoutReplayingPrompt(t *testing.T) {
	original := []string{"--permission-mode", "plan", "--model=sonnet", "--resume", "old", "--", "original work request"}
	for _, needsContinue := range []bool{false, true} {
		got, err := parseArgs(original, "exact-session", needsContinue)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"--permission-mode", "plan", "--model=sonnet", "--resume", "exact-session"}
		if needsContinue {
			want = append(want, "--", continuation)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("resume arguments: %q, want %q", got, want)
		}
	}
	initial, err := parseArgs(original, "", false)
	if err != nil || !reflect.DeepEqual(initial, original) {
		t.Fatalf("initial arguments changed: %q %v", initial, err)
	}
}

func TestUnsafeModesCannotDisableManagedProtocol(t *testing.T) {
	for _, flag := range []string{"--print", "--bare", "--safe-mode", "--settings", "--plugin-dir", "--no-session-persistence", "--disable-slash-commands", "--fork-session", "--output-format", "--remote", "--unknown"} {
		if _, err := parseArgs([]string{flag}, "", false); err == nil {
			t.Errorf("accepted %s", flag)
		}
	}
	if _, err := parseArgs([]string{"auth"}, "", false); err == nil {
		t.Fatal("accepted auth subcommand")
	}
}

func TestVersionRequiresCompleteMinimumRelease(t *testing.T) {
	for _, value := range []string{"2.1.312 (Claude Code)", "2.1.313", "2.2.0", "3.0.0"} {
		if !supportedVersion(value) {
			t.Errorf("rejected supported %q", value)
		}
	}
	for _, value := range []string{"", "2.1.311", "2.0.999", "1.99.999", "3.invalid.0", "3.0", "2.1.312-beta"} {
		if supportedVersion(value) {
			t.Errorf("accepted unsupported %q", value)
		}
	}
}

func TestRecoveryCommandQuotesSessionAndDirectory(t *testing.T) {
	command := recoveryCommand("/some path/claude", "/a'b", "id'; echo bad")
	if !strings.Contains(command, `'"'"'`) || !strings.Contains(command, " --resume '") {
		t.Fatalf("unsafe recovery command %q", command)
	}
}
