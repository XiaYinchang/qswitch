package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
)

func TestInstanceAndScopeGuardsBeforeMutation(t *testing.T) {
	a := setupApp(t)
	if err := a.SetManagedTools([]adapter.Tool{adapter.Antigravity}); err != nil {
		t.Fatal(err)
	}
	h := New(a, "127.0.0.1:7432").Handler()
	for _, tc := range []struct {
		name, header, origin, path, body string
		want                             int
	}{
		{"missing id", "", "", "/api/auto", `{"tool":"antigravity","enabled":false}`, 400},
		{"wrong id", "wrong", "", "/api/auto", `{"tool":"antigravity","enabled":false}`, 409},
		{"wrong origin", a.Instance().ID, "https://evil.example", "/api/auto", `{"tool":"antigravity","enabled":false}`, 403},
		{"capture scope", a.Instance().ID, "", "/api/capture", `{"tool":"codex"}`, 403},
		{"switch scope", a.Instance().ID, "", "/api/switch", `{"tool":"codex","id":"acc-a"}`, 403},
		{"probe scope", a.Instance().ID, "", "/api/probe", `{"tool":"codex","id":"acc-a"}`, 403},
		{"forget scope", a.Instance().ID, "", "/api/forget", `{"tool":"codex","id":"acc-a"}`, 403},
		{"login scope", a.Instance().ID, "", "/api/login/codex", `{}`, 403},
		{"auto scope", a.Instance().ID, "", "/api/auto", `{"tool":"codex","enabled":false}`, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			r.Host = "127.0.0.1:7432"
			r.Header.Set(instanceHeader, tc.header)
			r.Header.Set("Origin", tc.origin)
			w := do(h, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d %s", w.Code, w.Body.String())
			}
		})
	}
	if !a.Cfg.Codex.Enabled || !a.Cfg.Antigravity.Enabled {
		t.Fatal("rejected request mutated configuration")
	}
	w := do(h, local("GET", "/api/overview"))
	var ov app.Overview
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if len(ov.Tools) != 1 || ov.Tools[0].Tool != "antigravity" || ov.Instance.ID != a.Instance().ID {
		t.Fatal("overview advertised out-of-scope tools")
	}
}

func TestAutoAndEnvironmentWritesBindCurrentInstance(t *testing.T) {
	a := setupApp(t)
	h := New(a, "127.0.0.1:7432").Handler()
	if err := a.SetAuto("", false); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/auto", strings.NewReader(`{"tool":"codex","enabled":true}`))
	r.Host = "127.0.0.1:7432"
	r.Header.Set(instanceHeader, a.Instance().ID)
	w := do(h, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var ov app.Overview
	_ = json.Unmarshal(w.Body.Bytes(), &ov)
	for _, tool := range ov.Tools {
		if tool.Tool == "codex" && (tool.Auto || tool.AutoReason == "") {
			t.Fatal("hidden master re-enable")
		}
	}
	r = httptest.NewRequest("POST", "/api/environments", strings.NewReader(`{"environments":[{"id":"fixture","name":"Deck","url":"https://example.test/qswitch/"}]}`))
	r.Host = "127.0.0.1:7432"
	r.Header.Set(instanceHeader, a.Instance().ID)
	if w := do(h, r); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("retired environment writer remains available: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, local("GET", "/api/health?expected_instance=another")); w.Code != 409 {
		t.Fatal("expected identity not enforced")
	}
}

func TestLoginNavigationPreservesExpectedIdentity(t *testing.T) {
	a := setupApp(t)
	if err := SetPassword(a.DataDir, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	s := New(a, "127.0.0.1:7432")
	h := s.Handler()
	id := a.Instance().ID
	w := do(h, proxied("GET", "/?expected_instance="+id))
	want := "/qswitch/login?expected_instance=" + id
	if w.Code != 303 || w.Header().Get("Location") != want {
		t.Fatalf("lost expected identity: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = do(h, proxied("GET", "/login?expected_instance="+id))
	if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
		t.Fatal("login form lost expected identity")
	}
	r := withForm(proxied("POST", "/login?expected_instance="+id), testUser, testPass)
	w = do(h, r)
	if w.Code != 303 || w.Header().Get("Location") != "/qswitch/?expected_instance="+id {
		t.Fatal("login success lost expected identity")
	}
	r = withForm(proxied("POST", "/login?expected_instance=other"), testUser, testPass)
	w = do(h, r)
	if w.Code != http.StatusConflict || len(w.Result().Cookies()) != 0 {
		t.Fatal("logged in to wrong expected instance")
	}
}
