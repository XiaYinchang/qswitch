package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"qswitch/internal/config"
)

const ReportInterval = 15 * time.Second

type reportRequest struct {
	ctx    context.Context
	result chan reportResult
}
type reportResult struct {
	observed int64
	err      error
}

// ReportNow requests a new observation rather than replaying a previous body.
// It is a control command: the browser still reads the control-plane snapshot.
func (a *App) ReportNow(ctx context.Context) (int64, error) {
	if _, err := reportEndpoint(a.cfgSnapshot().Reporting, a.Instance().ID); err != nil {
		return 0, err
	}
	a.reportMu.Lock()
	requests := a.reportRequests
	done := a.reportDone
	a.reportMu.Unlock()
	if requests == nil {
		return 0, errors.New("节点上报尚未启动")
	}
	request := reportRequest{ctx: ctx, result: make(chan reportResult, 1)}
	select {
	case requests <- request:
	case <-done:
		return 0, errors.New("节点上报已停止")
	case <-ctx.Done():
		return 0, errors.New("等待节点采集超时")
	}
	select {
	case result := <-request.result:
		return result.observed, result.err
	case <-done:
		return 0, errors.New("节点上报已停止")
	case <-ctx.Done():
		return 0, errors.New("等待节点上报超时")
	}
}

// RunReporter isolates status collection from page requests. A slow or offline
// node cannot delay reading the other nodes' saved snapshots.
func (a *App) RunReporter(ctx context.Context) { a.runReporter(ctx, ReportInterval) }

func (a *App) runReporter(ctx context.Context, interval time.Duration) {
	requests := make(chan reportRequest, 1)
	done := make(chan struct{})
	a.reportMu.Lock()
	if a.reportRequests != nil {
		a.reportMu.Unlock()
		return
	}
	a.reportRequests = requests
	a.reportDone = done
	a.reportMu.Unlock()
	defer func() {
		a.reportMu.Lock()
		a.reportRequests = nil
		a.reportDone = nil
		close(done)
		a.reportMu.Unlock()
	}()
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastObserved int64
	var failed bool
	publish := func(ctx context.Context) reportResult {
		if ctx.Err() != nil {
			return reportResult{err: ctx.Err()}
		}
		cfg := a.cfgSnapshot().Reporting
		if cfg.URL == "" && cfg.Token == "" {
			return reportResult{err: errors.New("节点未配置控制面上报")}
		}
		endpoint, err := reportEndpoint(cfg, a.Instance().ID)
		if err != nil {
			return reportResult{err: err}
		}
		// Rejecting replayed timestamps at the receiver also requires successive
		// explicit refreshes within one second to produce distinct observations.
		if delay := time.Until(time.Unix(lastObserved+1, 0)); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return reportResult{err: ctx.Err()}
			case <-timer.C:
			}
		}
		observed := time.Now().Unix()
		lastObserved = observed
		report := Report{ObservedAt: observed, Overview: a.Overview()}
		if ctx.Err() != nil {
			return reportResult{err: ctx.Err()}
		}
		err = sendReport(ctx, client, endpoint, cfg.Token, report)
		if err != nil {
			if !failed && a.Log != nil {
				a.Log("control-plane status report failed")
			}
			failed = true
		} else if failed {
			if a.Log != nil {
				a.Log("control-plane status reporting recovered")
			}
			failed = false
		}
		return reportResult{observed: observed, err: err}
	}
	if ctx.Err() == nil {
		publish(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-requests:
			requestCtx, cancel := context.WithCancel(request.ctx)
			stop := context.AfterFunc(ctx, cancel)
			request.result <- publish(requestCtx)
			stop()
			cancel()
		case <-ticker.C:
			publish(ctx)
		}
	}
}

func reportEndpoint(cfg config.Reporting, id string) (string, error) {
	if cfg.URL == "" || len(strings.TrimSpace(cfg.Token)) < 32 {
		return "", errors.New("节点未配置有效的控制面上报地址和凭据")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("控制面上报地址无效")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", errors.New("控制面上报必须使用 HTTPS 或本机回环地址")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/reports/" + id
	u.RawPath = ""
	return u.String(), nil
}

func sendReport(ctx context.Context, client *http.Client, endpoint, token string, report Report) error {
	body, err := json.Marshal(report)
	if err != nil {
		return errors.New("无法编码节点状态")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("无法创建节点上报请求")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Qswitch-Instance", report.Overview.Instance.ID)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return errors.New("无法连接控制面，上报将在下个周期重试")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1024))
		return errors.New("控制面未接受节点状态")
	}
	var ack struct {
		InstanceID string `json:"instance_id"`
		ObservedAt int64  `json:"observed_at"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1024)).Decode(&ack) != nil || ack.InstanceID != report.Overview.Instance.ID || ack.ObservedAt != report.ObservedAt {
		return errors.New("控制面未确认该节点的本次上报")
	}
	return nil
}
