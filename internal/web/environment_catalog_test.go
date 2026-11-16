package web

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qswitch/internal/app"
)

const remoteEnvironmentID = "11111111111111111111111111111111"

func TestEnvironmentCatalogTransportBoundaries(t *testing.T) {
	for _, mode := range []string{"local", "local forged portal", "untrusted forwarded headers", "ordinary proxy", "portal", "nested prefix", "empty directory"} {
		t.Run(mode, func(t *testing.T) {
			a := setupApp(t)
			a.Cfg.Web.Token = "synthetic-catalog-token"
			items := []app.Environment{{ID: a.Instance().ID, Name: "Gateway"}, {ID: remoteEnvironmentID, Name: "Remote"}}
			if mode != "empty directory" {
				if err := a.SetEnvironments(items); err != nil {
					t.Fatal(err)
				}
			}
			r := local("GET", "/api/environments")
			prefix := ""
			if mode != "local" && mode != "local forged portal" {
				r = proxied("GET", "/api/environments")
				prefix = "/qswitch"
			}
			if mode != "local" && mode != "ordinary proxy" {
				r.Header.Set("X-Qswitch-Portal", "1")
			}
			if mode == "untrusted forwarded headers" {
				r.RemoteAddr = "198.51.100.4:1234"
				prefix = ""
			}
			if mode == "nested prefix" {
				prefix = "/deck/qswitch"
				r.Header.Set("X-Forwarded-Prefix", prefix)
			}
			r.Header.Set("Authorization", "Bearer synthetic-catalog-token")
			w := do(New(a, "127.0.0.1:7432").Handler(), r)
			if w.Code != http.StatusOK {
				t.Fatalf("catalog status %d: %s", w.Code, w.Body.String())
			}
			var got struct {
				Instance     app.InstanceInfo    `json:"instance"`
				Environments []map[string]string `json:"environments"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			portal := mode == "portal" || mode == "nested prefix"
			count := 1
			if portal {
				count = 2
			}
			if got.Instance.ID != a.Instance().ID || len(got.Environments) != count {
				t.Fatalf("wrong catalog scope: %+v", got)
			}
			for _, item := range got.Environments {
				base := prefix + "/api/"
				if portal {
					base = prefix + "/api/instances/" + item["id"] + "/"
				} else if item["id"] != a.Instance().ID {
					t.Fatal("single-host transport advertised a remote instance")
				}
				if len(item) != 3 || item["api_base"] != base || item["name"] == "" {
					t.Fatalf("catalog leaked URL/metadata or used wrong API prefix: %v", item)
				}
			}
		})
	}
}

func TestEnvironmentCatalogAuthenticationAndMisconfiguration(t *testing.T) {
	a := setupApp(t)
	a.Cfg.Web.Token = "synthetic-catalog-token"
	if err := a.SetEnvironments([]app.Environment{{ID: remoteEnvironmentID, Name: "Private remote"}}); err != nil {
		t.Fatal(err)
	}
	h := New(a, "127.0.0.1:7432").Handler()
	request := func(auth bool) *http.Request {
		r := proxied("GET", "/api/environments")
		r.Header.Set("X-Qswitch-Portal", "1")
		if auth {
			r.Header.Set("Authorization", "Bearer synthetic-catalog-token")
		}
		return r
	}
	if w := do(h, request(false)); w.Code != 401 || strings.Contains(w.Body.String(), remoteEnvironmentID) || strings.Contains(w.Body.String(), "Private remote") {
		t.Fatalf("unauthenticated catalog leaked metadata: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, request(true)); w.Code != 500 || !strings.Contains(w.Body.String(), "gateway instance") {
		t.Fatalf("catalog without gateway was accepted: %d %s", w.Code, w.Body.String())
	}
	if err := os.WriteFile(filepath.Join(a.DataDir, "environments.json"), []byte(`[{"id":"bad-id","name":"Invalid"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if w := do(h, request(true)); w.Code != 500 {
		t.Fatal("invalid route identity was accepted")
	}
	if w := do(h, local("GET", "/api/environments")); w.Code != 200 {
		t.Fatal("local catalog read a broken portal directory")
	}
	for _, path := range []string{"/api/instances/" + remoteEnvironmentID + "/overview", "/api/environments/unknown", "/unknown"} {
		if w := do(h, local("GET", path)); w.Code != 404 {
			t.Fatalf("backend unexpectedly opened routing path %s: %d", path, w.Code)
		}
	}
}

func TestLoginAndLogoutPreserveEnvironmentDeepLink(t *testing.T) {
	a := setupApp(t)
	if err := SetPassword(a.DataDir, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	s := New(a, "127.0.0.1:7432")
	h := s.Handler()
	query := url.Values{"expected_instance": {a.Instance().ID}, "env": {remoteEnvironmentID}}
	login := "/qswitch/login?" + query.Encode()
	root := "/qswitch/?" + query.Encode()
	w := do(h, proxied("GET", "/?"+query.Encode()+"&next=https://outside.example/"))
	if w.Code != 303 || w.Header().Get("Location") != login {
		t.Fatal("unauthorized navigation lost the environment or copied another redirect")
	}
	w = do(h, proxied("GET", "/login?"+query.Encode()))
	if w.Code != 200 || !strings.Contains(html.UnescapeString(w.Body.String()), `action="`+login+`"`) || strings.Contains(w.Body.String(), "登录到") {
		t.Fatal("login form lost the deep link or binds login to an environment")
	}
	w = do(h, withForm(proxied("POST", "/login?"+query.Encode()), testUser, "wrong-password"))
	if w.Code != 401 || !strings.Contains(html.UnescapeString(w.Body.String()), `action="`+login+`"`) {
		t.Fatal("login failure discarded the selected environment")
	}
	w = do(h, withForm(proxied("POST", "/login?"+query.Encode()), testUser, testPass))
	if w.Code != 303 || w.Header().Get("Location") != root {
		t.Fatal("remote environment incorrectly changed the gateway login identity")
	}
	cookie := sessionCookieOf(t, w)
	r := proxied("GET", "/login?"+query.Encode())
	r.AddCookie(cookie)
	if w := do(h, r); w.Code != 303 || w.Header().Get("Location") != root {
		t.Fatal("authenticated login redirect lost the selected environment")
	}
	r = proxied("POST", "/logout?"+query.Encode())
	r.AddCookie(cookie)
	if w := do(h, r); w.Code != 303 || w.Header().Get("Location") != login {
		t.Fatal("logout discarded the environment deep link")
	}
	s.Now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
	r = proxied("GET", "/?"+query.Encode())
	r.AddCookie(cookie)
	if w := do(h, r); w.Code != 303 || w.Header().Get("Location") != login {
		t.Fatal("expired session discarded the environment deep link")
	}
	query.Set("expected_instance", remoteEnvironmentID)
	w = do(h, withForm(proxied("POST", "/login?"+query.Encode()), testUser, testPass))
	if w.Code != 409 || len(w.Result().Cookies()) != 0 {
		t.Fatal("a remote environment was accepted as the gateway identity")
	}
}

func TestNavigationQueryValuesCannotChangeRedirectDestination(t *testing.T) {
	env := `remote&expected_instance=other/#next`
	got := preserveNavigation("/qswitch/login", url.Values{"env": {env}, "expected_instance": {"gateway"}, "next": {"https://outside.example/"}})
	u, err := url.Parse(got)
	if err != nil || u.Host != "" || u.Path != "/qswitch/login" || u.Fragment != "" || u.Query().Get("env") != env || u.Query().Get("expected_instance") != "gateway" || u.Query().Get("next") != "" {
		t.Fatalf("navigation query changed redirect identity or destination: %s", got)
	}
}

func TestEmptyEnvironmentLoginNeverFallsBackToGateway(t *testing.T) {
	a := setupApp(t)
	if err := SetPassword(a.DataDir, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	s := New(a, "127.0.0.1:7432")
	h := s.Handler()
	query := url.Values{"env": {""}, "expected_instance": {a.Instance().ID}}
	login := "/qswitch/login?" + query.Encode()
	root := "/qswitch/?" + query.Encode()
	w := do(h, proxied("GET", "/?"+query.Encode()))
	if w.Code != 303 || w.Header().Get("Location") != login {
		t.Errorf("unauthorized redirect discarded explicit empty env: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = do(h, proxied("GET", "/login?"+query.Encode()))
	if w.Code != 200 || !strings.Contains(html.UnescapeString(w.Body.String()), `action="`+login+`"`) {
		t.Error("login form discarded explicit empty env")
	}
	w = do(h, withForm(proxied("POST", "/login?"+query.Encode()), testUser, "wrong-password"))
	if w.Code != 401 || !strings.Contains(html.UnescapeString(w.Body.String()), `action="`+login+`"`) {
		t.Error("failed login form discarded explicit empty env")
	}
	w = do(h, withForm(proxied("POST", "/login?"+query.Encode()), testUser, testPass))
	if w.Code != 303 || w.Header().Get("Location") != root {
		t.Errorf("successful login fell back to gateway: %d %s", w.Code, w.Header().Get("Location"))
	}
	cookie := sessionCookieOf(t, w)
	r := proxied("GET", "/login?"+query.Encode())
	r.AddCookie(cookie)
	if w := do(h, r); w.Code != 303 || w.Header().Get("Location") != root {
		t.Error("authenticated login navigation discarded explicit empty env")
	}
	s.Now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
	r = proxied("GET", "/?"+query.Encode())
	r.AddCookie(cookie)
	if w := do(h, r); w.Code != 303 || w.Header().Get("Location") != login {
		t.Error("expired session discarded explicit empty env")
	}
	w = do(h, proxied("GET", "/login?"+query.Encode()))
	if w.Code != 200 || !strings.Contains(html.UnescapeString(w.Body.String()), `action="`+login+`"`) {
		t.Error("expired session login form discarded explicit empty env")
	}
	w = do(h, withForm(proxied("POST", "/login?"+query.Encode()), testUser, testPass))
	if w.Code != 303 || w.Header().Get("Location") != root {
		t.Error("renewed login fell back to gateway")
	}
}

func TestNavigationDistinguishesAbsentAndEmptyEnvironment(t *testing.T) {
	for _, present := range []bool{false, true} {
		query := url.Values{"expected_instance": {""}}
		want := "/qswitch/login"
		if present {
			query.Set("env", "")
			want += "?env="
		}
		if got := preserveNavigation("/qswitch/login", query); got != want {
			t.Errorf("env present=%v: got %s, want %s", present, got, want)
		}
	}
}
