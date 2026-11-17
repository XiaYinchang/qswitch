package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/app"
)

// ServingInstance identifies a local listener before a CLI recommends its URL.
// A different data directory must not silently route a user to that listener.
func ServingInstance(addr string) (app.InstanceInfo, error) {
	if err := CheckLoopback(addr); err != nil {
		return app.InstanceInfo{}, err
	}
	c := &http.Client{Timeout: 800 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected redirect") }}
	res, err := c.Get("http://" + addr + "/api/health")
	if err != nil {
		return app.InstanceInfo{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return app.InstanceInfo{}, fmt.Errorf("health status %d", res.StatusCode)
	}
	var body struct {
		Instance app.InstanceInfo `json:"instance"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body); err != nil {
		return app.InstanceInfo{}, err
	}
	if body.Instance.ID == "" {
		return app.InstanceInfo{}, errors.New("listener has no verified instance identity")
	}
	return body.Instance, nil
}

const instanceHeader = "X-Qswitch-Instance"

func (s *Server) checkInstance(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	want := r.Header.Get(instanceHeader)
	if want == "" && r.Method == http.MethodPost {
		writeErr(w, 400, "missing X-Qswitch-Instance")
		return false
	}
	if want != "" && (s.App.Instance().ID == "" || want != s.App.Instance().ID) {
		writeErr(w, 409, "instance mismatch; no operation was performed")
		return false
	}
	if expected := r.URL.Query().Get("expected_instance"); expected != "" && expected != s.App.Instance().ID {
		writeErr(w, 409, "instance mismatch; no operation was performed")
		return false
	}
	return true
}

func (s *Server) requireTool(w http.ResponseWriter, tool adapter.Tool, action string) bool {
	if err := s.App.CheckScope(tool); err != nil {
		writeErr(w, 403, err.Error())
		return false
	}
	if action != "" {
		if err := s.App.CheckAction(tool, action); err != nil {
			writeErr(w, 409, err.Error())
			return false
		}
	}
	return true
}

func (s *Server) postAuto(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool    string `json:"tool"`
		Enabled *bool  `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tool, err := adapter.ParseTool(req.Tool)
	if err != nil || req.Enabled == nil {
		writeErr(w, 400, "tool and enabled are required")
		return
	}
	if !s.requireTool(w, tool, "") {
		return
	}
	if err := s.App.SetAuto(tool, *req.Enabled); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, s.App.Overview())
}

func (s *Server) getDiagnostics(w http.ResponseWriter, r *http.Request) {
	tool, err := adapter.ParseTool(r.URL.Query().Get("tool"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.requireTool(w, tool, "") {
		return
	}
	writeJSON(w, 200, map[string]any{"instance": s.App.Instance(), "tool": tool, "capabilities": s.App.Capabilities(tool), "diagnostics": s.App.DoctorTool(tool)})
}

func (s *Server) getEnvironments(w http.ResponseWriter, r *http.Request) {
	x := externalOf(r)
	instance := s.App.Instance()
	items := []app.Environment{{ID: instance.ID, Name: instance.Name}}
	portal := false
	if x.proxied && r.Header.Get("X-Qswitch-Portal") == "1" {
		configured, err := s.App.Environments()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if len(configured) != 0 {
			found := false
			for _, item := range configured {
				found = found || item.ID == instance.ID
			}
			if !found {
				writeErr(w, 500, "environment directory does not contain the gateway instance")
				return
			}
			items, portal = configured, true
		}
	}
	type environmentRoute struct {
		app.Environment
		APIBase  string              `json:"api_base"`
		Snapshot *app.ReportSnapshot `json:"snapshot,omitempty"`
	}
	routes := make([]environmentRoute, 0, len(items))
	for _, item := range items {
		base := x.path("/api/")
		if portal {
			base = x.path("/api/instances/" + item.ID + "/")
		}
		route := environmentRoute{Environment: item, APIBase: base}
		if portal {
			// A broken or missing report only makes this instance unavailable.
			// Reading the catalog never asks a node to collect fresh state.
			snapshot, _ := s.App.ReportSnapshot(item.ID)
			route.Snapshot = &snapshot
		}
		routes = append(routes, route)
	}
	_, loginRequired := s.loginRequired(r)
	writeJSON(w, 200, map[string]any{
		"instance": instance, "environments": routes, "login_required": loginRequired, "control_plane": portal,
		"server_time": s.App.ReportServerTime(), "report_max_age_seconds": int64(app.ReportMaxAge / time.Second),
	})
}

func loginTool(r *http.Request) adapter.Tool {
	if strings.HasPrefix(r.URL.Path, "/api/login/grok") {
		return adapter.Grok
	}
	return adapter.Codex
}

func preserveNavigation(path string, query url.Values) string {
	values := url.Values{}
	if expected := query.Get("expected_instance"); expected != "" {
		values.Set("expected_instance", expected)
	}
	if query.Has("env") {
		values.Set("env", query.Get("env"))
	}
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}
