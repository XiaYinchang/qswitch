package quota

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

const CodexOriginator = "codex_cli_rs"

var codexCLIVersionOnce = sync.OnceValue(func() string {
	if os.Getenv("QSWITCH_IN_TEST") == "1" {
		return "0.151.0"
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		return "0.151.0"
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "0.151.0"
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "0.151.0"
	}
	v := fields[len(fields)-1]
	if v == "" || strings.ContainsAny(v, "\r\n") {
		return "0.151.0"
	}
	return v
})

var grokCLIVersionOnce = sync.OnceValue(func() string {
	if os.Getenv("QSWITCH_IN_TEST") == "1" {
		return "1.0.25"
	}
	bin, err := exec.LookPath("grok")
	if err != nil {
		return "1.0.25"
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "1.0.25"
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "1.0.25"
	}
	v := fields[0]
	if strings.EqualFold(v, "grok") && len(fields) > 1 {
		v = fields[1]
	}
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, "\r\n") {
		return "1.0.25"
	}
	return v
})

func GrokCLIVersion() string { return grokCLIVersionOnce() }

func CodexAuthUserAgent() string {
	if v := strings.TrimSpace(os.Getenv("QSWITCH_CODEX_UA_VERSION")); v != "" {
		return fmt.Sprintf("%s/%s (%s; %s)", CodexOriginator, v, runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("%s/%s (%s; %s)", CodexOriginator, codexCLIVersionOnce(), runtime.GOOS, runtime.GOARCH)
}

func setCodexAuthHeaders(req *http.Request, withOriginator bool) {
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", CodexAuthUserAgent())
	if withOriginator {
		req.Header.Set("originator", CodexOriginator)
	} else {
		req.Header.Del("originator")
	}
}
