package app

import (
	"fmt"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/cursoradp"
	"qswitch/internal/state"
)

// The caller holds Cursor's tool lock. Earlier versions called a usable CLI
// snapshot incomplete just because it had no macOS desktop token. Recheck only
// an account being operated on; never decrypt the whole vault at startup.
func (a *App) recheckCursorCLILocked(ac state.Account) (state.Account, error) {
	if ac.Tool != string(adapter.Cursor) || !ac.Incomplete || a.cursorDesktopSync() {
		return ac, nil
	}
	if err := a.CheckScope(adapter.Cursor); err != nil {
		return ac, err
	}
	b, err := a.loadBlob(adapter.Cursor, ac.StableID)
	if err != nil {
		return ac, err
	}
	if b.Identity.StableID != ac.StableID {
		return ac, fmt.Errorf("cursor 历史快照身份不一致，请在官方 CLI 登录后重新收录")
	}
	if err := cursoradp.ValidateCLI(b); err != nil {
		return ac, fmt.Errorf("cursor 历史快照没有可核实的 CLI 凭据，请在官方 CLI 登录后重新收录：%w", err)
	}
	ac.Incomplete = false
	// Upsert updates the credential metadata, not quota or failure backoff.
	// All other metadata is copied unchanged from the row read under this lock.
	if err := a.State.UpsertAccount(ac); err != nil {
		return ac, err
	}
	return ac, nil
}
