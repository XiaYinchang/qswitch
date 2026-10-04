package app

import (
	"strings"
	"testing"

	"qswitch/internal/state"
)

func TestKimiPhoneLabelsAndReferences(t *testing.T) {
	a, _ := setup(t)
	ac := state.Account{Tool: "kimi", StableID: "fixture-account-a", Phone: "+86 176****0000", DisplayName: "Fixture User", PlanHint: "Allegretto"}
	if err := a.State.UpsertAccount(ac); err != nil {
		t.Fatal(err)
	}
	if err := a.State.SetPointer(state.Pointer{Tool: "kimi", StableID: ac.StableID}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.List("kimi"), ac.Phone) || !strings.Contains(a.Status(), "active="+ac.Phone) {
		t.Fatal("CLI omitted the displayed phone label")
	}
	for _, ref := range []string{ac.Phone, ac.DisplayName, ac.Phone + "/" + ac.PlanHint, ac.Phone + "/" + ac.PlanHint + "/" + ac.StableID[:8]} {
		id, err := a.resolveAccount("kimi", ref)
		if err != nil || id != ac.StableID {
			t.Fatalf("displayed account reference was not resolved: %v", err)
		}
	}
	ac.StableID = "fixture-account-b"
	if err := a.State.UpsertAccount(ac); err != nil {
		t.Fatal(err)
	}
	_, err := a.resolveAccount("kimi", ac.Phone)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), ac.Phone) || strings.Contains(err.Error(), "chatgpt_account_id") {
		t.Fatal("duplicate masked phone must report labeled stable-ID choices")
	}
}
