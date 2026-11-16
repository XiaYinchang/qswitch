package codex

import (
	"qswitch/internal/adapter"
	"testing"
	"time"
)

func TestAPIKeyModeCannotBorrowOAuthIdentity(t *testing.T) {
	if _, _, err := identityFromRaw([]byte(`{"auth_mode":"api_key","tokens":{"account_id":"cached-oauth","access_token":"cached"}}`)); err == nil {
		t.Fatal("mixed API-key mode entered subscription rotation")
	}
}

func TestNoActivityEvidenceCannotAuthorizeCLIExit(t *testing.T) {
	ad := Adapter{List: func() ([]adapter.Proc, error) { return []adapter.Proc{{PID: 42, Command: `"C:\Tools\codex.exe"`}}, nil }}
	if ad.Idle(t.TempDir(), time.Second) {
		t.Fatal("missing session directory was treated as idle")
	}
}
