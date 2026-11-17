package app

import (
	"errors"
	"reflect"
	"testing"

	"qswitch/internal/adapter"
)

type overviewStatusAdapter struct {
	adapter.Adapter
	holders      adapter.Holders
	err          error
	manual, auto int
}

func (a *overviewStatusAdapter) ManualBlockers(string) (adapter.Holders, error) {
	a.manual++
	return a.holders, a.err
}

func (a *overviewStatusAdapter) AutoBlockers(string) (adapter.Holders, error) {
	a.auto++
	return a.holders, a.err
}

func TestOverviewObservesBlockersOnceAndPreservesDesktopIndicators(t *testing.T) {
	a, _ := setup(t)
	var tools []adapter.Tool
	for _, tool := range adapter.AllTools() {
		if tool != adapter.Claude {
			tools = append(tools, tool)
		}
	}
	if err := a.SetManagedTools(tools); err != nil {
		t.Fatal(err)
	}
	statuses := map[adapter.Tool]*overviewStatusAdapter{}
	for _, tool := range tools {
		status := &overviewStatusAdapter{
			Adapter: a.Adapters[tool],
			holders: adapter.Holders{
				Manual:     []adapter.Proc{{PID: 21}},
				AutoExtra:  []adapter.Proc{{PID: 22}},
				ChatGPTApp: true, CursorApp: true, GrokBotApp: true, DevinApp: true,
			},
		}
		statuses[tool], a.Adapters[tool] = status, status
	}
	for _, view := range a.Overview().Tools {
		status := statuses[adapter.Tool(view.Tool)]
		if status.manual != 0 || status.auto != 1 {
			t.Errorf("%s observed blockers manual=%d auto=%d; want one combined observation", view.Tool, status.manual, status.auto)
		}
		if !view.Busy || view.BlockersUnavailable || !reflect.DeepEqual(view.BlockedPIDs, []int{21}) {
			t.Errorf("%s lost manual blocker or confused desktop-only PID with a manual blocker: %+v", view.Tool, view)
		}
		if !view.ChatGPTApp || !view.CursorApp || !view.GrokBotApp || !view.DevinApp {
			t.Errorf("%s lost desktop indicators", view.Tool)
		}
	}
	// A later display read must get fresh state, not reuse a global cache.
	for _, status := range statuses {
		status.holders = adapter.Holders{AutoExtra: []adapter.Proc{{PID: 22}}, ChatGPTApp: true}
	}
	for _, view := range a.Overview().Tools {
		if statuses[adapter.Tool(view.Tool)].auto != 2 || view.Busy || len(view.BlockedPIDs) != 0 || !view.ChatGPTApp {
			t.Errorf("%s did not refresh manual status independently from desktop indicators: %+v", view.Tool, view)
		}
	}
}

func TestOverviewBlockerFailureNeverReportsIdle(t *testing.T) {
	a, _ := setup(t)
	if err := a.SetManagedTools([]adapter.Tool{adapter.Cursor}); err != nil {
		t.Fatal(err)
	}
	status := &overviewStatusAdapter{Adapter: a.Adapters[adapter.Cursor], err: errors.New("synthetic process listing unavailable")}
	a.Adapters[adapter.Cursor] = status
	view := a.Overview().Tools[0]
	if !view.BlockersUnavailable || !view.Busy || len(view.BlockedPIDs) != 0 || status.auto != 1 {
		t.Fatalf("unavailable status appeared idle or invented a PID: %+v", view)
	}
	_, bot := splitCursorBot(view)
	if !bot.BlockersUnavailable || !bot.Busy {
		t.Fatalf("Grok Bot lost Cursor's unavailable status: %+v", bot)
	}
	status.err = nil
	view = a.Overview().Tools[0]
	if view.BlockersUnavailable || view.Busy || status.auto != 2 {
		t.Fatalf("recovered observation did not clear unavailable status: %+v", view)
	}
}

func TestOverviewManagedClaudeUsesOneObservationForBlockersAndDetails(t *testing.T) {
	a, _ := setup(t)
	if err := a.SetManagedTools([]adapter.Tool{adapter.Claude}); err != nil {
		t.Fatal(err)
	}
	live := &fakeRunner{ready: true, needsContinue: true, reason: "synthetic continuation pending"}
	finished := &fakeRunner{exited: true}
	startFakeRunner(t, a, 1, live)
	startFakeRunner(t, a, 2, finished)
	// Calling either adapter blocker method would duplicate the runner read
	// or consult unrelated unmanaged Claude processes.
	status := &overviewStatusAdapter{Adapter: a.Adapters[adapter.Claude], holders: adapter.Holders{Manual: []adapter.Proc{{PID: 99}}}}
	a.Adapters[adapter.Claude] = status
	view := a.Overview().Tools[0]
	live.mu.Lock()
	liveCalls := live.statuses
	live.mu.Unlock()
	finished.mu.Lock()
	finishedCalls := finished.statuses
	finished.mu.Unlock()
	if status.manual != 0 || status.auto != 0 || liveCalls != 1 || finishedCalls != 1 {
		t.Fatalf("duplicate Claude observation: manual=%d auto=%d live=%d finished=%d", status.manual, status.auto, liveCalls, finishedCalls)
	}
	if !view.Busy || view.BlockersUnavailable || view.ManagedUnavailable || !reflect.DeepEqual(view.BlockedPIDs, []int{7001}) {
		t.Fatalf("managed Claude blocker state changed: %+v", view)
	}
	if len(view.Managed) != 1 || view.Managed[0].PID != 7001 || !view.Managed[0].Ready || !view.Managed[0].NeedsContinue || view.Managed[0].Reason != live.reason {
		t.Fatalf("runner details did not come from the same observation: %+v", view.Managed)
	}
	live.mu.Lock()
	live.statusError = true
	live.mu.Unlock()
	view = a.Overview().Tools[0]
	if !view.Busy || !view.BlockersUnavailable || !view.ManagedUnavailable || len(view.Managed) != 0 || len(view.BlockedPIDs) != 0 {
		t.Fatalf("failed runner observation appeared idle or reused stale details: %+v", view)
	}
}
