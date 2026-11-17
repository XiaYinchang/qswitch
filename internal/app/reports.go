package app

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qswitch/internal/livefile"
)

const (
	ReportMaxAge    = 60 * time.Second
	ReportClockSkew = 5 * time.Second
	MaxReportBytes  = 1 << 20
)

var (
	ErrReportRejected = errors.New("report rejected")
	ErrReportInstance = errors.New("report instance is not configured")
	errReportCorrupt  = errors.New("stored report content is invalid")
)

// Report contains display data only. Credentials are never part of Overview.
type Report struct {
	ObservedAt int64    `json:"observed_at"`
	Overview   Overview `json:"overview"`
}

type ReportSnapshot struct {
	ObservedAt int64     `json:"observed_at"`
	ReceivedAt int64     `json:"received_at"`
	Stale      bool      `json:"stale"`
	Overview   *Overview `json:"overview"`
}

type storedReport struct {
	Report
	ReceivedAt int64 `json:"received_at"`
}

func validReportInstance(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}

func (a *App) reportInstance(id string) error {
	if !validReportInstance(id) {
		return ErrReportInstance
	}
	items, err := a.Environments()
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID == id {
			return nil
		}
	}
	return ErrReportInstance
}

func readPrivateReportFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("reporting file must be a regular file with mode 0600")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxReportBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxReportBytes {
		return nil, errors.New("reporting file exceeds size limit")
	}
	return raw, nil
}

// ReportingToken reads a private, per-instance machine credential. An admin
// token or a token shared by two instances cannot authorize reporting.
func (a *App) ReportingToken(id string) (string, error) {
	if err := a.reportInstance(id); err != nil {
		return "", err
	}
	raw, err := readPrivateReportFile(filepath.Join(a.DataDir, "reporting-tokens.json"))
	if err != nil {
		return "", err
	}
	var tokens map[string]string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return "", errors.New("invalid reporting credentials")
	}
	seen := map[string]bool{}
	adminToken := a.WebToken()
	for instance, token := range tokens {
		if !validReportInstance(instance) || len(token) < 32 || strings.ContainsAny(token, " \t\r\n") || token == adminToken || seen[token] {
			return "", errors.New("reporting credentials must be independent per-instance secrets")
		}
		seen[token] = true
	}
	if tokens[id] == "" {
		return "", ErrReportInstance
	}
	return tokens[id], nil
}

func (a *App) reportPath(id string) string {
	return filepath.Join(a.DataDir, "reports", id+".json")
}

func (a *App) readReport(id string) (storedReport, error) {
	var stored storedReport
	raw, err := readPrivateReportFile(a.reportPath(id))
	if err != nil {
		return stored, err
	}
	if json.Unmarshal(raw, &stored) != nil || stored.Overview.Instance.ID != id || stored.ObservedAt <= 0 || stored.ReceivedAt <= 0 {
		return storedReport{}, errReportCorrupt
	}
	return stored, nil
}

// AcceptReport atomically replaces only this instance's latest observation.
// The per-instance lock also prevents concurrent senders from moving it back.
func (a *App) AcceptReport(id string, report Report) error {
	if err := a.reportInstance(id); err != nil {
		return err
	}
	if report.Overview.Instance.ID != id {
		return fmt.Errorf("%w: instance mismatch", ErrReportRejected)
	}
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", "report-"+id+".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	now := a.now().Unix()
	if report.ObservedAt <= 0 || report.ObservedAt > now+int64(ReportClockSkew/time.Second) || now-report.ObservedAt >= int64(ReportMaxAge/time.Second) {
		return fmt.Errorf("%w: observation timestamp is outside the accepted window", ErrReportRejected)
	}
	previous, err := a.readReport(id)
	// A valid fresh report can repair corrupt content, but file type,
	// permissions, and I/O failures must not be silently overwritten.
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errReportCorrupt) {
		return err
	}
	if err == nil && report.ObservedAt <= previous.ObservedAt {
		return fmt.Errorf("%w: observation is not newer than the stored report", ErrReportRejected)
	}
	stored := storedReport{Report: report, ReceivedAt: now}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if len(raw)+1 > MaxReportBytes {
		return fmt.Errorf("%w: report exceeds size limit", ErrReportRejected)
	}
	return livefile.AtomicWrite(a.reportPath(id), append(raw, '\n'), 0o600)
}

// ReportServerTime lets transports use the same clock as report expiry.
func (a *App) ReportServerTime() int64 { return a.now().Unix() }

// ReportSnapshot never contacts a node or runs local account/session probes.
// Missing or invalid files return an unavailable snapshot; expired snapshots
// retain their display data and original timestamps across process restarts.
func (a *App) ReportSnapshot(id string) (ReportSnapshot, error) {
	missing := ReportSnapshot{Stale: true}
	if err := a.reportInstance(id); err != nil {
		return missing, err
	}
	stored, err := a.readReport(id)
	if errors.Is(err, os.ErrNotExist) {
		return missing, nil
	}
	if err != nil {
		return missing, err
	}
	now := a.now().Unix()
	stale := stored.ObservedAt > now+int64(ReportClockSkew/time.Second) || stored.ReceivedAt > now+int64(ReportClockSkew/time.Second) ||
		now-stored.ObservedAt >= int64(ReportMaxAge/time.Second) || now-stored.ReceivedAt >= int64(ReportMaxAge/time.Second)
	return ReportSnapshot{ObservedAt: stored.ObservedAt, ReceivedAt: stored.ReceivedAt, Stale: stale, Overview: &stored.Overview}, nil
}
