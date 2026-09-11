package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/adapter/codex"
	"qswitch/internal/adapter/cursoradp"
	"qswitch/internal/adapter/grok"
	"qswitch/internal/busy"
	"qswitch/internal/clock"
	"qswitch/internal/config"
	"qswitch/internal/livefile"
	"qswitch/internal/notify"
	"qswitch/internal/paths"
	"qswitch/internal/quota"
	"qswitch/internal/secutil"
	"qswitch/internal/state"
	"qswitch/internal/vault"
)

type App struct {
	UserHome  string
	DataDir   string
	Cfg       config.Config
	Vault     *vault.Store
	State     *state.DB
	Adapters  map[adapter.Tool]adapter.Adapter
	HTTP      quota.HTTP
	Notify    notify.Notifier
	Clock     clock.Clock
	Bins      []string
	ListProcs func() ([]adapter.Proc, error)

	lastFP     map[adapter.Tool][32]byte
	deskFP     [32]byte
	deskFPOk   bool
	lastDeskAt time.Time
}

func Open(userHome, dataDir string, keys secutil.KeyProvider, bins []string, n notify.Notifier, list func() ([]adapter.Proc, error)) (*App, error) {
	if userHome == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		userHome = h
	}
	if dataDir == "" {
		dataDir = paths.DataDir(userHome)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(dataDir, "config.toml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := cfg.Save(cfgPath); err != nil {
			return nil, err
		}
	}
	st, err := state.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		return nil, err
	}
	if keys == nil {
		if secutil.InTest() {
			keys = &secutil.Memory{}
		} else {
			keys = secutil.Darwin{}
		}
	}
	if n == nil {
		n = notify.OS{Enabled: cfg.Notify.Enabled}
	}
	if list == nil {
		list = func() ([]adapter.Proc, error) { return (busy.PS{}).List() }
	}
	a := &App{
		UserHome:  userHome,
		DataDir:   dataDir,
		Cfg:       cfg,
		Vault:     &vault.Store{Dir: dataDir, Keys: keys, Bins: bins},
		State:     st,
		HTTP:      quota.HTTP{},
		Notify:    n,
		Clock:     clock.Real{},
		Bins:      bins,
		ListProcs: list,
		lastFP:    map[adapter.Tool][32]byte{},
	}
	a.Adapters = map[adapter.Tool]adapter.Adapter{
		adapter.Codex:  codex.Adapter{List: list},
		adapter.Grok:   grok.Adapter{List: list},
		adapter.Cursor: cursoradp.Adapter{List: list},
	}
	return a, nil
}

func (a *App) Close() error {
	if a.State != nil {
		return a.State.Close()
	}
	return nil
}

func (a *App) now() time.Time {
	if a.Clock == nil {
		return time.Now()
	}
	return a.Clock.Now()
}

func (a *App) Init() error {
	for _, dir := range []string{paths.VaultDir(a.DataDir), paths.LocksDir(a.DataDir), paths.LogsDir(a.UserHome)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if _, err := a.Vault.Keys.GetOrCreate(a.Bins); err != nil {
		return err
	}
	bin := filepath.Join(paths.DefaultBinDir(a.UserHome), paths.DefaultBin)
	for _, b := range a.Bins {
		if filepath.Base(b) == paths.DefaultBin && b != "" {
			bin = b
			break
		}
	}
	return paths.WriteLaunchdPlist(
		paths.GeneratedPlistPath(a.DataDir),
		bin,
		a.DataDir,
		paths.LogPath(a.UserHome),
		paths.ErrLogPath(a.UserHome),
	)
}

func (a *App) Capture(tool adapter.Tool) ([]adapter.Blob, []adapter.Warning, error) {
	ad := a.Adapters[tool]
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", string(tool)+".lock"))
	if err != nil {
		return nil, nil, err
	}
	defer lock.Close()
	blobs, warn, err := ad.Capture(a.UserHome)
	if err != nil {
		return nil, warn, err
	}
	desync := len(blobs) > 1
	for _, b := range blobs {
		if err := a.saveBlob(b); err != nil {
			return blobs, warn, err
		}
	}
	p, _ := a.State.GetPointer(string(tool))
	p.Tool = string(tool)
	if tool == adapter.Cursor {
		p.Desync = desync
	}
	if id := liveCLIIdentity(blobs); id != "" && p.StableID != id {
		p.StableID = id
		p.Gen++
	}
	if err := a.State.SetPointer(p); err != nil {
		return blobs, warn, err
	}
	return blobs, warn, nil
}

func liveCLIIdentity(blobs []adapter.Blob) string {
	for _, b := range blobs {
		if b.Incomplete || b.Identity.StableID == "" || strings.HasPrefix(b.Identity.StableID, "desktop:") {
			continue
		}
		return b.Identity.StableID
	}
	return ""
}

func (a *App) saveBlob(b adapter.Blob) error {
	id, err := a.Adapters[b.Tool].IdentityOf(b)
	if err != nil {
		return err
	}
	if err := a.Vault.Put(string(b.Tool), id.StableID, b.Payload); err != nil {
		return err
	}
	return a.State.UpsertAccount(state.Account{
		Tool:           string(b.Tool),
		StableID:       id.StableID,
		Email:          id.Email,
		PlanHint:       id.PlanHint,
		Incomplete:     b.Incomplete,
		StaleCLI:       b.StaleCLI,
		LastCapturedAt: a.now().Unix(),
		VaultGen:       1,
	})
}

func (a *App) loadBlob(tool adapter.Tool, ref string) (adapter.Blob, error) {
	id, err := a.resolveAccount(string(tool), ref)
	if err != nil {
		return adapter.Blob{}, err
	}
	raw, err := a.Vault.Get(string(tool), id)
	if err != nil {
		return adapter.Blob{}, fmt.Errorf("account %s/%s: %w", tool, ref, err)
	}
	ac, _ := a.State.GetAccount(string(tool), id)
	ad := a.Adapters[tool]
	b := adapter.Blob{Tool: tool, Payload: raw, Incomplete: ac.Incomplete, StaleCLI: ac.StaleCLI}
	ident, err := ad.IdentityOf(b)
	if err != nil {
		return b, err
	}
	b.Identity = ident
	return b, nil
}

func (a *App) resolveAccount(tool, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty account ref")
	}
	accs, err := a.State.ListAccounts(tool)
	if err != nil {
		return "", err
	}
	for _, ac := range accs {
		if ac.StableID == ref {
			return ac.StableID, nil
		}
	}
	if len(ref) >= 8 {
		var pref []state.Account
		for _, ac := range accs {
			if strings.HasPrefix(ac.StableID, ref) {
				pref = append(pref, ac)
			}
		}
		if len(pref) == 1 {
			return pref[0].StableID, nil
		}
		if len(pref) > 1 {
			return "", ambiguousRef(tool, ref, pref)
		}
	}
	var hits []state.Account
	for _, ac := range accs {
		if accountRefMatch(ac, ref) {
			hits = append(hits, ac)
		}
	}
	if len(hits) == 1 {
		return hits[0].StableID, nil
	}
	if len(hits) > 1 {
		return "", ambiguousRef(tool, ref, hits)
	}
	return ref, nil
}

func accountRefMatch(ac state.Account, ref string) bool {
	if ac.Email == ref || ac.PlanHint == ref {
		return true
	}
	label := ac.Email
	if ac.PlanHint != "" {
		label = ac.Email + "/" + ac.PlanHint
	}
	if label == ref {
		return true
	}
	if ac.Email != "" && ac.PlanHint != "" && len(ac.StableID) >= 8 && ref == ac.Email+"/"+ac.PlanHint+"/"+ac.StableID[:8] {
		return true
	}
	return false
}

func ambiguousRef(tool, ref string, hits []state.Account) error {
	var b strings.Builder
	fmt.Fprintf(&b, "ambiguous %s %q; pass chatgpt_account_id (same email can have personal + team workspaces):\n", tool, ref)
	for _, ac := range hits {
		fmt.Fprintf(&b, "  %s  plan=%s  %s\n", ac.Email, ac.PlanHint, ac.StableID)
	}
	return errors.New(b.String())
}

func (a *App) Switch(tool adapter.Tool, ref string, opts adapter.RestoreOpts, killCLI bool) error {
	ad := a.Adapters[tool]
	blob, err := a.loadBlob(tool, ref)
	if err != nil {
		return err
	}
	if tool == adapter.Cursor && blob.Incomplete && !opts.ForceAlign {
		return errors.New("cursor blob incomplete; quit Cursor.app then: qswitch switch cursor <desktop-id> --force-align")
	}
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", string(tool)+".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()

	h, err := ad.ManualBlockers(a.UserHome)
	if err != nil {
		return err
	}
	if opts.CLIOnly && tool == adapter.Cursor {
		var ag []adapter.Proc
		for _, p := range h.Manual {
			if !strings.Contains(p.Command, "Cursor.app/Contents/MacOS/Cursor") {
				ag = append(ag, p)
			}
		}
		h.Manual = ag
		h.CursorApp = false
	}
	if h.ManualBusy() {
		if killCLI {
			if !opts.CLIOnly && !ad.Idle(a.UserHome, a.Cfg.IdleGrace()) {
				return fmt.Errorf("not idle (idle_grace=%s); blocked_by_pid=%v", a.Cfg.IdleGrace(), h.ManualPIDs())
			}
			if err := ad.KillCLI(a.UserHome); err != nil {
				return err
			}
			time.Sleep(300 * time.Millisecond)
			h, _ = ad.ManualBlockers(a.UserHome)
			if opts.CLIOnly && tool == adapter.Cursor {
				var ag []adapter.Proc
				for _, p := range h.Manual {
					if !strings.Contains(p.Command, "Cursor.app/Contents/MacOS/Cursor") {
						ag = append(ag, p)
					}
				}
				h.Manual = ag
				h.CursorApp = false
			}
		}
		if h.ManualBusy() {
			return &adapter.BusyError{Holders: h}
		}
	}

	if _, cur, err := ad.Capture(a.UserHome); err == nil {
		_ = cur
		if blobs, _, err := ad.Capture(a.UserHome); err == nil {
			for _, b := range blobs {
				_ = a.saveBlob(b)
			}
		}
	}

	if tool == adapter.Codex {
		if live, err := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json")); err == nil {
			blob = codex.MergeLiveTokens(blob, live)
		}
	}
	if tool == adapter.Grok {
		if live, err := os.ReadFile(filepath.Join(a.UserHome, ".grok", "auth.json")); err == nil {
			blob = grok.MergeLiveTokens(blob, live)
		}
	}
	if err := ad.Restore(a.UserHome, blob, opts); err != nil {
		return err
	}
	desync := blob.Incomplete && !opts.CLIOnly
	if tool == adapter.Cursor {
		if blobs, _, err := ad.Capture(a.UserHome); err == nil {
			for _, b := range blobs {
				_ = a.saveBlob(b)
			}
			desync = len(blobs) > 1
		}
	}
	a.rememberFP(tool)
	p := state.Pointer{
		Tool:             string(tool),
		StableID:         blob.Identity.StableID,
		Gen:              1,
		LastApplyAt:      a.now().Unix(),
		LastApplyTo:      blob.Identity.StableID,
		WritebackRetries: 0,
		Desync:           desync,
	}
	if err := a.State.SetPointer(p); err != nil {
		return err
	}
	_ = a.State.ClearPending(string(tool))
	auto, _ := ad.AutoBlockers(a.UserHome)
	msg := fmt.Sprintf("switched %s -> %s (%s). 新进程才会用新号。", tool, blob.Identity.Email, blob.Identity.StableID)
	if auto.ChatGPTApp {
		msg += " WarnChatGPTAppRunning"
	}
	a.Notify.Send("qswitch", msg)
	if auto.ChatGPTApp {
		fmt.Fprintln(os.Stderr, "WarnChatGPTAppRunning: ChatGPT.app 可能写回旧 token")
	}
	return nil
}

func (a *App) Forget(tool adapter.Tool, ref string) error {
	b, err := a.loadBlob(tool, ref)
	if err != nil {
		return err
	}
	p, _ := a.State.GetPointer(string(tool))
	if p.StableID == b.Identity.StableID {
		return errors.New("refusing to forget the live account; switch away first or pass after --forget-live via forget after switch")
	}
	if err := a.Vault.Delete(string(tool), b.Identity.StableID); err != nil {
		return err
	}
	return a.State.DeleteAccount(string(tool), b.Identity.StableID)
}

func (a *App) Status() string {
	var b strings.Builder
	for _, t := range adapter.AllTools() {
		p, _ := a.State.GetPointer(string(t))
		acc, _ := a.State.GetAccount(string(t), p.StableID)
		ad := a.Adapters[t]
		man, _ := ad.ManualBlockers(a.UserHome)
		aut, _ := ad.AutoBlockers(a.UserHome)
		pend, _ := a.State.GetPending(string(t))
		email := acc.Email
		if email == "" {
			email = p.StableID
		}
		class := acc.LastQuotaClass
		if class == "" {
			class = "unknown"
		}
		plan := acc.PlanHint
		if plan == "" {
			plan = "-"
		}
		fmt.Fprintf(&b, "%s  active=%s plan=%s id=%s class=%s used=%.1f%% auto=%v desync=%v busy=%v blocked_by_pid=%v queued_since=%s chatgpt_app=%v\n",
			t, email, plan, p.StableID, class, acc.LastUsedPct, a.Cfg.General.AutoSwitch && a.Cfg.ToolEnabled(string(t)), p.Desync, man.ManualBusy(), man.ManualPIDs(), queued(pend), aut.ChatGPTApp)
	}
	return b.String()
}

func queued(p state.Pending) string {
	if p.QueuedAt == 0 {
		return "-"
	}
	return time.Unix(p.QueuedAt, 0).UTC().Format(time.RFC3339)
}

func (a *App) List(tool string) string {
	var b strings.Builder
	accs, _ := a.State.ListAccounts(tool)
	for _, ac := range accs {
		p, _ := a.State.GetPointer(ac.Tool)
		mark := " "
		if p.StableID == ac.StableID {
			mark = "*"
		}
		cool := "-"
		if until := visibleCooling(ac, a.now().Unix()); until > 0 {
			cool = time.Unix(until, 0).UTC().Format(time.RFC3339)
		}
		reset := "-"
		if ac.LastResetsAt > 0 {
			reset = time.Unix(ac.LastResetsAt, 0).UTC().Format(time.RFC3339)
		}
		plan := ac.PlanHint
		if plan == "" {
			plan = "-"
		}
		fmt.Fprintf(&b, "%s %s  %s  plan=%s  %s  class=%s used=%.1f%% reset=%s cool=%s incomplete=%v stale_cli=%v\n", mark, ac.Tool, ac.Email, plan, ac.StableID, ac.LastQuotaClass, ac.LastUsedPct, reset, cool, ac.Incomplete, ac.StaleCLI)
	}
	return b.String()
}

func (a *App) SaveConfig() error {
	return a.Cfg.Save(filepath.Join(a.DataDir, "config.toml"))
}

func (a *App) liveFingerprint(tool adapter.Tool) ([32]byte, error) {
	if tool != adapter.Cursor {
		return a.Adapters[tool].Fingerprint(a.UserHome)
	}
	cli, cliErr := cursoradp.HashCLI(a.UserHome)
	if a.lastDeskAt.IsZero() || a.now().Sub(a.lastDeskAt) >= a.Cfg.CursorPoll() {
		if d, err := cursoradp.HashDesktop(a.UserHome); err == nil {
			a.deskFP = d
			a.deskFPOk = true
			a.lastDeskAt = a.now()
		} else if !a.deskFPOk {
			return cursoradp.CombineFP(cli, cliErr, [32]byte{}, err)
		}
	}
	if a.deskFPOk {
		return cursoradp.CombineFP(cli, cliErr, a.deskFP, nil)
	}
	return cli, cliErr
}

func (a *App) rememberFP(tool adapter.Tool) {
	fp, err := a.liveFingerprint(tool)
	if err != nil {
		return
	}
	if a.lastFP == nil {
		a.lastFP = map[adapter.Tool][32]byte{}
	}
	a.lastFP[tool] = fp
	_ = a.State.RememberSelfWrite("fingerprint:"+string(tool), fp, a.now().Add(2*time.Second))
}

func (a *App) Ingest(tool adapter.Tool) error {
	ad := a.Adapters[tool]
	fp, err := a.liveFingerprint(tool)
	if err != nil {
		return nil
	}
	if a.lastFP == nil {
		a.lastFP = map[adapter.Tool][32]byte{}
	}
	if prev, ok := a.lastFP[tool]; ok && prev == fp {
		return nil
	}
	self, err := a.State.IsSelfWrite("fingerprint:"+string(tool), fp, a.now())
	if err != nil {
		return err
	}
	if self {
		a.lastFP[tool] = fp
		return nil
	}
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", string(tool)+".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	blobs, _, err := ad.Capture(a.UserHome)
	if err != nil {
		return err
	}
	if len(blobs) == 0 {
		return nil
	}
	var added int
	for _, b := range blobs {
		ident, ierr := ad.IdentityOf(b)
		isNew := false
		if ierr == nil && ident.StableID != "" && !strings.HasPrefix(ident.StableID, "desktop:") {
			if _, gerr := a.State.GetAccount(string(tool), ident.StableID); gerr != nil {
				isNew = true
			}
		}
		if err := a.saveBlob(b); err != nil {
			return err
		}
		if isNew {
			added++
			label := ident.DisplayName
			if label == "" {
				label = ident.Email
			}
			a.Notify.Send("qswitch", fmt.Sprintf("收录新账号 %s %s", tool, label))
		}
	}
	id := liveCLIIdentity(blobs)
	if id == "" {
		a.lastFP[tool] = fp
		return nil
	}
	p, _ := a.State.GetPointer(string(tool))
	prevID := p.StableID
	now := a.now().Unix()
	switch {
	case p.StableID == "" || p.StableID == id:
		p.Tool = string(tool)
		if p.StableID == "" {
			p.StableID = id
		}
		if tool == adapter.Cursor {
			p.Desync = len(blobs) > 1
		}
		_ = a.State.SetPointer(p)
	case p.LastApplyAt > 0 && now-p.LastApplyAt < int64(a.Cfg.Writeback().Seconds()) && p.WritebackRetries == 0:
		target, err := a.loadBlob(tool, p.LastApplyTo)
		if err != nil {
			a.Cfg.SetToolEnabled(string(tool), false)
			_ = a.SaveConfig()
			a.Notify.Send("qswitch", "writeback restore failed; auto disabled")
			return err
		}
		if err := ad.Restore(a.UserHome, target, adapter.RestoreOpts{}); err != nil {
			a.Cfg.SetToolEnabled(string(tool), false)
			_ = a.SaveConfig()
			a.Notify.Send("qswitch", "writeback restore failed; auto disabled")
			return err
		}
		p.WritebackRetries = 1
		_ = a.State.SetPointer(p)
		a.rememberFP(tool)
		a.Notify.Send("qswitch", "reverted writeback once")
		return nil
	case p.LastApplyAt > 0 && now-p.LastApplyAt < int64(a.Cfg.Writeback().Seconds()) && p.WritebackRetries >= 1:
		a.Cfg.SetToolEnabled(string(tool), false)
		_ = a.SaveConfig()
		a.Notify.Send("qswitch", "旧进程仍在回写; auto disabled")
	default:
		p.StableID = id
		p.WritebackRetries = 0
		p.Desync = len(blobs) > 1
		_ = a.State.SetPointer(p)
		_ = a.State.ClearPending(string(tool))
		if added == 0 {
			email := id
			if ac, err := a.State.GetAccount(string(tool), id); err == nil && ac.Email != "" {
				email = ac.Email
			}
			a.Notify.Send("qswitch", fmt.Sprintf("当前 %s 换成已收录账号 %s", tool, email))
		}
	}
	if tool == adapter.Codex && prevID != "" && prevID != id {
		a.refreshParkedCodex(prevID)
	}
	a.lastFP[tool] = fp
	return nil
}

func (a *App) refreshParkedCodex(id string) {
	blob, err := a.loadBlob(adapter.Codex, id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, tok, _, kerr := a.keepAliveCodex(ctx, blob, true)
	if kerr != nil && tok == "" {
		acc, _ := a.State.GetAccount(string(adapter.Codex), id)
		_ = a.State.UpdateQuotaSnapshot(string(adapter.Codex), id, string(quota.Expired), acc.LastUsedPct, acc.LastResetsAt, a.now().Unix())
	}
}

func (a *App) quotaInterval(tool, id string) time.Duration {
	pts, err := a.State.RecentQuota(tool, id, 16)
	var samples []quota.Sample
	if err == nil {
		for _, p := range pts {
			samples = append(samples, quota.Sample{
				T:       time.Unix(p.Ts, 0),
				UsedPct: p.UsedPct,
				Class:   quota.Class(p.Class),
			})
		}
	}
	return quota.Interval(samples, a.Cfg.IntervalMin(), a.Cfg.IntervalMax(), a.Cfg.ETAChecks())
}

func (a *App) ProbeLocal(tool adapter.Tool) {
	p, _ := a.State.GetPointer(string(tool))
	if p.StableID == "" {
		return
	}
	r, ok := a.tailLocal(tool)
	if !ok {
		return
	}
	if tool == adapter.Grok && r.Class == quota.Exhausted && r.Source == "jsonl_402" {
		acc, err := a.State.GetAccount(string(tool), p.StableID)
		if err == nil {
			switch quota.Class(acc.LastQuotaClass) {
			case quota.OK, quota.Soft:
				if acc.LastUsedPct < 90 && acc.LastHTTPAt > 0 {
					return
				}
			}
		}
	}
	_ = a.State.UpdateQuotaSnapshot(string(tool), p.StableID, string(r.Class), r.UsedPct, r.ResetsAt, a.now().Unix())
	_ = a.State.LogQuota(string(tool), p.StableID, string(r.Class), r.Source, r.UsedPct, a.now())
	if r.Class == quota.Exhausted {
		a.setCooling(string(tool), p.StableID, r.ResetsAt)
		a.considerSwitch(tool)
	}
}

func (a *App) tailLocal(tool adapter.Tool) (quota.Result, bool) {
	kind := quota.Kind(tool)
	var best quota.Result
	hit := false
	for _, path := range a.localQuotaFiles(tool) {
		r, ok := quota.Tail(kind, path)
		if !ok {
			continue
		}
		if !hit {
			best, hit = r, true
			continue
		}
		best = quota.Prefer(best, r)
	}
	return best, hit
}

func (a *App) localQuotaFiles(tool adapter.Tool) []string {
	home := a.UserHome
	switch tool {
	case adapter.Codex:
		p, _, err := quota.NewestJSONL(filepath.Join(home, ".codex", "sessions"))
		if err != nil || p == "" {
			return nil
		}
		return []string{p}
	case adapter.Grok:
		u := filepath.Join(home, ".grok", "logs", "unified.jsonl")
		if st, err := os.Stat(u); err == nil && !st.IsDir() && st.Size() > 0 {
			return []string{u}
		}
		return nil
	case adapter.Cursor:
		p, _, err := quota.NewestJSONLMatch(filepath.Join(home, ".cursor", "projects"), func(path string) bool {
			return strings.Contains(path, "agent-transcripts")
		})
		if err != nil || p == "" {
			return nil
		}
		return []string{p}
	default:
		return nil
	}
}

func (a *App) ProbeHTTP(ctx context.Context, tool adapter.Tool, force bool) {
	if !force && !a.Cfg.ToolEnabled(string(tool)) {
		return
	}
	p, _ := a.State.GetPointer(string(tool))
	if p.StableID == "" || strings.HasPrefix(p.StableID, "desktop:") {
		return
	}
	res := a.probeAccount(ctx, tool, p.StableID, !force)
	if res.Class == quota.Exhausted {
		a.considerSwitch(tool)
	}
}

func (a *App) ProbeRecovered(ctx context.Context, tool adapter.Tool, force bool) {
	if !force && !a.Cfg.ToolEnabled(string(tool)) {
		return
	}
	p, _ := a.State.GetPointer(string(tool))
	accs, err := a.State.ListAccounts(string(tool))
	if err != nil {
		return
	}
	now := a.now().Unix()
	limit := 1
	if force {
		limit = 8
	}
	n := 0
	for _, ac := range accs {
		if n >= limit {
			return
		}
		if ac.StableID == p.StableID || ac.Incomplete || ac.StaleCLI || strings.HasPrefix(ac.StableID, "desktop:") {
			continue
		}
		if quota.Class(ac.LastQuotaClass) != quota.Exhausted {
			continue
		}
		if !force && !a.recoverDue(tool, ac) {
			continue
		}
		res := a.probeAccount(ctx, tool, ac.StableID, false)
		n++
		if res.Class == quota.Unknown {
			_ = a.State.UpdateQuotaSnapshot(string(tool), ac.StableID, string(quota.Exhausted), ac.LastUsedPct, ac.LastResetsAt, now)
			a.setCooling(string(tool), ac.StableID, a.now().Add(a.Cfg.Backoff()).Unix())
			continue
		}
		if res.Class == quota.OK || res.Class == quota.Soft {
			_ = a.State.SetCooling(string(tool), ac.StableID, 0)
			label := ac.Email
			if ac.PlanHint != "" {
				label += "/" + ac.PlanHint
			}
			a.Notify.Send("qswitch", fmt.Sprintf("%s %s quota recovered (%.0f%% used)", tool, label, res.UsedPct))
			cur, _ := a.State.GetAccount(string(tool), p.StableID)
			if quota.Class(cur.LastQuotaClass) == quota.Exhausted {
				a.considerSwitch(tool)
			}
		}
	}
}

func (a *App) recoverDue(tool adapter.Tool, ac state.Account) bool {
	now := a.now().Unix()
	if ac.HTTPBackoffUntil > now {
		return false
	}
	if tool == adapter.Codex {
		wait := a.codexRecoverInterval(ac)
		if ac.LastHTTPAt > 0 && now-ac.LastHTTPAt < int64(wait.Seconds()) {
			return false
		}
		return true
	}
	if ac.CoolingUntil > now || ac.LastResetsAt > now {
		return false
	}
	if ac.LastHTTPAt > 0 && now-ac.LastHTTPAt < int64(a.Cfg.IntervalMin().Seconds()) {
		return false
	}
	return true
}

func (a *App) codexRecoverInterval(ac state.Account) time.Duration {
	now := a.now().Unix()
	advertised := ac.LastResetsAt
	if ac.CoolingUntil > advertised {
		advertised = ac.CoolingUntil
	}
	if advertised > 0 && advertised <= now {
		return a.Cfg.IntervalMin()
	}
	return config.DefaultCodexRecover
}

func (a *App) KeepAlive(ctx context.Context, tool adapter.Tool) {
	switch tool {
	case adapter.Codex:
		a.keepAliveCodexAccounts(ctx)
	case adapter.Grok:
		a.keepAliveGrokAccounts(ctx)
	}
}

func (a *App) keepAliveCodexAccounts(ctx context.Context) {
	p, _ := a.State.GetPointer(string(adapter.Codex))
	accs, err := a.State.ListAccounts(string(adapter.Codex))
	if err != nil {
		return
	}
	n := 0
	for _, ac := range accs {
		if ac.StableID == p.StableID || strings.HasPrefix(ac.StableID, "desktop:") {
			continue
		}
		blob, err := a.loadBlob(adapter.Codex, ac.StableID)
		if err != nil {
			continue
		}
		stored, err := codex.ReadAuth(blob)
		if err != nil || stored.Refresh == "" {
			continue
		}
		liveRaw, _ := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json"))
		if live, err := codex.ReadAuthBytes(liveRaw); err == nil && codex.SameOAuthUser(stored, live) {
			continue
		}
		force := quota.Class(ac.LastQuotaClass) == quota.Expired
		if !force && !stored.NeedsRefresh(a.now()) {
			continue
		}
		_, _, _, _ = a.keepAliveCodex(ctx, blob, force)
		n++
		if n >= 1 {
			return
		}
	}
}

func (a *App) keepAliveGrokAccounts(ctx context.Context) {
	p, _ := a.State.GetPointer(string(adapter.Grok))
	accs, err := a.State.ListAccounts(string(adapter.Grok))
	if err != nil {
		return
	}
	n := 0
	for _, ac := range accs {
		if ac.StableID == p.StableID || strings.HasPrefix(ac.StableID, "desktop:") {
			continue
		}
		blob, err := a.loadBlob(adapter.Grok, ac.StableID)
		if err != nil {
			continue
		}
		stored, err := grok.ReadAuth(blob)
		if err != nil || !stored.Refreshable() {
			continue
		}
		liveRaw, _ := os.ReadFile(filepath.Join(a.UserHome, ".grok", "auth.json"))
		if live, err := grok.ReadAuthBytes(liveRaw); err == nil && grok.SameOIDCUser(stored, live) {
			continue
		}
		force := quota.Class(ac.LastQuotaClass) == quota.Expired
		if !force && !stored.NeedsRefresh(a.now()) {
			continue
		}
		_, _, _ = a.keepAliveGrok(ctx, blob, force)
		n++
		if n >= 1 {
			return
		}
	}
}

func (a *App) keepAliveCodex(ctx context.Context, blob adapter.Blob, force bool) (adapter.Blob, string, string, error) {
	stored, err := codex.ReadAuth(blob)
	if err != nil {
		return blob, "", "", err
	}
	liveRaw, _ := os.ReadFile(filepath.Join(a.UserHome, ".codex", "auth.json"))
	if live, err := codex.ReadAuthBytes(liveRaw); err == nil && live.Access != "" && codex.SameOAuthUser(stored, live) {
		acct := stored.AccountID
		if acct == "" {
			acct = live.AccountID
		}
		return blob, live.Access, acct, nil
	}
	if !force && stored.Access != "" && !stored.NeedsRefresh(a.now()) {
		return blob, stored.Access, stored.AccountID, nil
	}
	if stored.Refresh == "" {
		if stored.Access == "" {
			return blob, "", stored.AccountID, errors.New("codex: no refresh_token")
		}
		return blob, stored.Access, stored.AccountID, nil
	}
	tok, err := a.HTTP.CodexRefresh(ctx, stored.Refresh)
	if err != nil {
		if tok.Permanent {
			return blob, "", stored.AccountID, err
		}
		return blob, stored.Access, stored.AccountID, err
	}
	nb, aerr := codex.ApplyRefresh(blob, tok, a.now())
	if aerr != nil {
		return blob, tok.AccessToken, stored.AccountID, aerr
	}
	if err := a.saveBlob(nb); err != nil {
		return nb, tok.AccessToken, stored.AccountID, err
	}
	access := tok.AccessToken
	acct := stored.AccountID
	if na, err := codex.ReadAuth(nb); err == nil {
		if na.Access != "" {
			access = na.Access
		}
		if na.AccountID != "" {
			acct = na.AccountID
		}
	}
	return nb, access, acct, nil
}

func (a *App) keepAliveGrok(ctx context.Context, blob adapter.Blob, force bool) (adapter.Blob, string, error) {
	stored, err := grok.ReadAuth(blob)
	if err != nil {
		return blob, "", err
	}
	liveRaw, _ := os.ReadFile(filepath.Join(a.UserHome, ".grok", "auth.json"))
	if live, err := grok.ReadAuthBytes(liveRaw); err == nil && live.Key != "" && grok.SameOIDCUser(stored, live) {
		return blob, live.Key, nil
	}
	if !force && stored.Key != "" && !stored.NeedsRefresh(a.now()) {
		return blob, stored.Key, nil
	}
	if !stored.Refreshable() {
		if stored.Key == "" {
			return blob, "", errors.New("grok: no refresh_token")
		}
		return blob, stored.Key, nil
	}
	tok, err := a.HTTP.GrokRefresh(ctx, stored.Refresh, stored.ClientID)
	if err != nil {
		if tok.Permanent {
			return blob, "", err
		}
		return blob, stored.Key, err
	}
	nb, aerr := grok.ApplyRefresh(blob, tok, a.now())
	if aerr != nil {
		return blob, tok.AccessToken, aerr
	}
	if err := a.saveBlob(nb); err != nil {
		return nb, tok.AccessToken, err
	}
	access := tok.AccessToken
	if na, err := grok.ReadAuth(nb); err == nil && na.Key != "" {
		access = na.Key
	}
	return nb, access, nil
}

func (a *App) probeAccount(ctx context.Context, tool adapter.Tool, id string, gateInterval bool) quota.Result {
	if id == "" || strings.HasPrefix(id, "desktop:") {
		return quota.Result{Class: quota.Unknown}
	}
	if a.HTTP.Client == nil && secutil.InTest() {
		acc, _ := a.State.GetAccount(string(tool), id)
		return quota.Result{Class: quota.Class(acc.LastQuotaClass), UsedPct: acc.LastUsedPct, ResetsAt: acc.LastResetsAt}
	}
	acc, err := a.State.GetAccount(string(tool), id)
	if err != nil {
		return quota.Result{Class: quota.Unknown}
	}
	now := a.now()
	if acc.HTTPBackoffUntil > now.Unix() {
		return quota.Result{Class: quota.Class(acc.LastQuotaClass), UsedPct: acc.LastUsedPct, ResetsAt: acc.LastResetsAt, Source: "backoff"}
	}
	if gateInterval {
		interval := a.quotaInterval(string(tool), id)
		if acc.LastHTTPAt > 0 && now.Unix()-acc.LastHTTPAt < int64(interval.Seconds()) {
			return quota.Result{Class: quota.Class(acc.LastQuotaClass), UsedPct: acc.LastUsedPct, ResetsAt: acc.LastResetsAt, Source: "cached"}
		}
		if acc.LastQuotaClass == string(quota.Exhausted) && acc.LastResetsAt > now.Unix() {
			return quota.Result{Class: quota.Exhausted, UsedPct: acc.LastUsedPct, ResetsAt: acc.LastResetsAt, Source: "cached"}
		}
	}
	blob, err := a.loadBlob(tool, id)
	if err != nil {
		return quota.Result{Class: quota.Unknown}
	}
	var res quota.Result
	switch tool {
	case adapter.Codex:
		forced := acc.LastQuotaClass == string(quota.Expired)
		nb, tok, acct, kerr := a.keepAliveCodex(ctx, blob, forced)
		if kerr != nil && tok == "" {
			_ = a.State.UpdateQuotaSnapshot(string(tool), id, string(quota.Expired), acc.LastUsedPct, acc.LastResetsAt, now.Unix())
			return quota.Result{Class: quota.Expired, Source: "refresh"}
		}
		res, _ = a.HTTP.Codex(ctx, tok, acct)
		if res.Class == quota.Expired && !forced {
			_, tok2, acct2, rerr := a.keepAliveCodex(ctx, nb, true)
			if rerr == nil && tok2 != "" && tok2 != tok {
				res, _ = a.HTTP.Codex(ctx, tok2, acct2)
			}
		}
	case adapter.Grok:
		forced := acc.LastQuotaClass == string(quota.Expired)
		nb, tok, kerr := a.keepAliveGrok(ctx, blob, forced)
		if kerr != nil && tok == "" {
			_ = a.State.UpdateQuotaSnapshot(string(tool), id, string(quota.Expired), acc.LastUsedPct, acc.LastResetsAt, now.Unix())
			return quota.Result{Class: quota.Expired, Source: "refresh"}
		}
		res, _ = a.HTTP.Grok(ctx, tok)
		if res.Class == quota.Expired && !forced {
			_, tok2, rerr := a.keepAliveGrok(ctx, nb, true)
			if rerr == nil && tok2 != "" && tok2 != tok {
				res, _ = a.HTTP.Grok(ctx, tok2)
			}
		}
	case adapter.Cursor:
		tok, err := cursoradp.AccessToken(blob)
		if err != nil {
			return quota.Result{Class: quota.Unknown}
		}
		res, _ = a.HTTP.Cursor(ctx, tok)
	default:
		return quota.Result{Class: quota.Unknown}
	}
	backoff := acc.HTTPBackoffUntil
	if res.Class == quota.Unknown {
		backoff = now.Add(a.Cfg.Backoff()).Unix()
	}
	_ = a.State.UpdateQuota(string(tool), id, string(res.Class), res.UsedPct, res.ResetsAt, now.Unix(), now.Unix(), backoff)
	if len(res.Buckets) > 0 {
		_ = a.State.SetQuotaDetail(string(tool), id, quota.EncodeBuckets(res.Buckets))
	}
	_ = a.State.LogQuota(string(tool), id, string(res.Class), res.Source, res.UsedPct, now)
	switch res.Class {
	case quota.Exhausted:
		a.setCooling(string(tool), id, res.ResetsAt)
	case quota.OK, quota.Soft:
		_ = a.State.SetCooling(string(tool), id, 0)
	}
	return res
}

func (a *App) setCooling(tool, id string, resetsAt int64) {
	until := resetsAt
	if until <= a.now().Unix() {
		until = a.now().Add(5 * time.Hour).Unix()
	}
	_ = a.State.SetCooling(tool, id, until)
}

func (a *App) considerSwitch(tool adapter.Tool) {
	if !a.Cfg.General.AutoSwitch || !a.Cfg.ToolEnabled(string(tool)) {
		return
	}
	p, _ := a.State.GetPointer(string(tool))
	if p.Desync && tool == adapter.Cursor {
		a.Notify.Send("qswitch", "cursor DESYNC; auto switch skipped")
		return
	}
	next := a.pickNext(tool, p.StableID)
	if next == "" {
		a.Notify.Send("qswitch", fmt.Sprintf("%s all accounts exhausted", tool))
		return
	}
	ad := a.Adapters[tool]
	auto, _ := ad.AutoBlockers(a.UserHome)
	pend := state.Pending{Tool: string(tool), FromID: p.StableID, ToID: next, QueuedAt: a.now().Unix(), Reason: "exhausted", BlockedByPID: joinPIDs(auto.AllPIDs())}
	_ = a.State.SetPending(pend)
	a.Notify.Send("qswitch", fmt.Sprintf("%s quota exhausted; will switch to %s", tool, next))
	a.TryApply(tool, false)
}

func (a *App) pickNext(tool adapter.Tool, current string) string {
	accs, _ := a.State.ListAccounts(string(tool))
	var best string
	bestPct := 101.0
	bestClass := 2
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	now := a.now().Unix()
	for _, ac := range accs {
		if ac.StableID == current || ac.Incomplete || ac.StaleCLI || strings.HasPrefix(ac.StableID, "desktop:") {
			continue
		}
		if ac.CoolingUntil > now {
			continue
		}
		r, pct := a.candidateRank(ctx, tool, ac)
		if r >= 2 {
			continue
		}
		if r < bestClass || (r == bestClass && pct < bestPct) {
			bestClass = r
			bestPct = pct
			best = ac.StableID
		}
	}
	return best
}

func (a *App) candidateRank(ctx context.Context, tool adapter.Tool, ac state.Account) (int, float64) {
	rank := func(c quota.Class, pct float64) (int, float64) {
		switch c {
		case quota.OK:
			return 0, pct
		case quota.Soft:
			return 1, pct
		default:
			return 9, pct
		}
	}
	now := a.now().Unix()
	switch quota.Class(ac.LastQuotaClass) {
	case quota.OK, quota.Soft:
		return rank(quota.Class(ac.LastQuotaClass), ac.LastUsedPct)
	case quota.Expired:
		return 9, 0
	case quota.Exhausted:
		if ac.LastResetsAt > now || ac.CoolingUntil > now {
			return 9, 0
		}
	}
	res := a.probeAccount(ctx, tool, ac.StableID, false)
	return rank(res.Class, res.UsedPct)
}

func (a *App) TryApply(tool adapter.Tool, killCLI bool) error {
	pend, err := a.State.GetPending(string(tool))
	if err != nil || pend.ToID == "" {
		return err
	}
	ad := a.Adapters[tool]
	auto, _ := ad.AutoBlockers(a.UserHome)
	if auto.AutoBusy() && !killCLI {
		if auto.ManualBusy() && ad.Idle(a.UserHome, a.Cfg.IdleGrace()) {
			return a.Switch(tool, pend.ToID, adapter.RestoreOpts{}, true)
		}
		return nil
	}
	if auto.ChatGPTApp && !auto.ManualBusy() {
		// auto-blocker only: do not write
		return nil
	}
	return a.Switch(tool, pend.ToID, adapter.RestoreOpts{}, killCLI)
}

func (a *App) Apply(tool adapter.Tool, killCLI bool) error {
	if tool == "" {
		var first error
		for _, t := range adapter.AllTools() {
			if err := a.TryApply(t, killCLI); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
	return a.TryApply(tool, killCLI)
}

func (a *App) Doctor() string {
	var b strings.Builder
	fmt.Fprintf(&b, "data=%s home=%s test=%v daemon=%v\n", a.DataDir, a.UserHome, secutil.InTest(), a.daemonRunning())
	for _, t := range adapter.AllTools() {
		ad := a.Adapters[t]
		h, _ := ad.ManualBlockers(a.UserHome)
		au, _ := ad.AutoBlockers(a.UserHome)
		p, _ := a.State.GetPointer(string(t))
		fmt.Fprintf(&b, "%s live=%s desync=%v manual_busy=%v auto_busy=%v chatgpt=%v cursor_app=%v pids=%v enabled=%v\n",
			t, p.StableID, p.Desync, h.ManualBusy(), au.AutoBusy(), au.ChatGPTApp, h.CursorApp, h.ManualPIDs(), a.Cfg.ToolEnabled(string(t)))
		for _, pth := range ad.LivePaths(a.UserHome) {
			st, err := os.Stat(pth)
			if err != nil {
				fmt.Fprintf(&b, "  missing %s\n", pth)
				continue
			}
			fmt.Fprintf(&b, "  %s size=%d mode=%s\n", pth, st.Size(), st.Mode())
		}
	}
	vsc := filepath.Join(a.UserHome, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	if st, err := os.Stat(vsc); err == nil {
		fmt.Fprintf(&b, "cursor vscdb size=%d (read-only doctor; tests must not open live db)\n", st.Size())
	}
	return b.String()
}

func (a *App) daemonRunning() bool {
	if a.ListProcs == nil {
		return false
	}
	list, err := a.ListProcs()
	if err != nil {
		return false
	}
	self := os.Getpid()
	for _, p := range list {
		base := p.Command
		if i := strings.IndexByte(base, ' '); i >= 0 {
			base = base[:i]
		}
		if filepath.Base(base) == "qswitchd" && p.PID != self {
			return true
		}
	}
	return false
}

func (a *App) RunDaemon(ctx context.Context) error {
	tIngest := time.NewTicker(2 * time.Second)
	tLocal := time.NewTicker(30 * time.Second)
	tHTTP := time.NewTicker(2 * time.Minute) // actual HTTP still gated by min_http_interval
	tApply := time.NewTicker(5 * time.Second)
	defer tIngest.Stop()
	defer tLocal.Stop()
	defer tHTTP.Stop()
	defer tApply.Stop()
	a.Notify.Send("qswitch", "daemon started")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tIngest.C:
			for _, t := range adapter.AllTools() {
				_ = a.Ingest(t)
			}
		case <-tLocal.C:
			for _, t := range adapter.AllTools() {
				a.ProbeLocal(t)
			}
		case <-tHTTP.C:
			if a.HTTP.Client == nil && secutil.InTest() {
				continue
			}
			for _, t := range adapter.AllTools() {
				a.KeepAlive(ctx, t)
				a.ProbeHTTP(ctx, t, false)
				a.ProbeRecovered(ctx, t, false)
			}
		case <-tApply.C:
			for _, t := range adapter.AllTools() {
				_ = a.TryApply(t, false)
			}
		}
	}
}

func joinPIDs(p []int) string {
	var s []string
	for _, n := range p {
		s = append(s, strconv.Itoa(n))
	}
	return strings.Join(s, ",")
}

func Fingerprint(b []byte) [32]byte { return sha256.Sum256(b) }
