package web

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"qswitch/internal/app"
)

// Only this exact POST endpoint has separate machine authentication. Other
// methods and neighboring paths continue through normal web admission.
func reportRequestID(r *http.Request) (string, bool) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/api/reports/") {
		return "", false
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/reports/")
	decoded, err := hex.DecodeString(id)
	return id, err == nil && len(decoded) == 16 && id == strings.ToLower(id)
}

func (s *Server) postReport(w http.ResponseWriter, r *http.Request, id string) {
	x := externalOf(r)
	if CheckLoopback(s.Addr) == nil && !x.proxied && !loopbackHost(r.Host) {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !validOrigin(r, origin) {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	token, err := s.App.ReportingToken(id)
	if err != nil {
		if errors.Is(err, app.ErrReportInstance) {
			writeErr(w, http.StatusUnauthorized, "report authentication failed")
		} else {
			writeErr(w, http.StatusServiceUnavailable, "reporting is unavailable")
		}
		return
	}
	if len(r.Header.Values("Authorization")) != 1 || !bearerOK(r, token) {
		writeErr(w, http.StatusUnauthorized, "report authentication failed")
		return
	}
	if len(r.Header.Values(instanceHeader)) != 1 || r.Header.Get(instanceHeader) != id ||
		(r.URL.Query().Has("expected_instance") && r.URL.Query().Get("expected_instance") != id) {
		writeErr(w, http.StatusConflict, "report instance mismatch")
		return
	}
	var report app.Report
	if !readJSON(w, r, &report) {
		return
	}
	if err := s.App.AcceptReport(id, report); err != nil {
		switch {
		case errors.Is(err, app.ErrReportInstance):
			writeErr(w, http.StatusForbidden, "report instance is not configured")
		case errors.Is(err, app.ErrReportRejected):
			writeErr(w, http.StatusConflict, "report identity, timestamp or ordering is invalid")
		default:
			writeErr(w, http.StatusServiceUnavailable, "report could not be stored")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance_id": id, "observed_at": report.ObservedAt})
}

// This operator command uses the standard management authentication and target
// instance guard, unlike the receiving endpoint above.
func (s *Server) postReportNow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	observedAt, err := s.App.ReportNow(ctx)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "node status collection or reporting failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": s.App.Instance(), "observed_at": observedAt, "reported": true})
}
