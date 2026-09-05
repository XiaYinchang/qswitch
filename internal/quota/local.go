package quota

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func NewestJSONL(root string) (string, time.Time, error) {
	return NewestJSONLMatch(root, nil)
}

func NewestJSONLMatch(root string, match func(path string) bool) (string, time.Time, error) {
	var newest string
	var mt time.Time
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		if match != nil && !match(path) {
			return nil
		}
		if info.ModTime().After(mt) {
			mt = info.ModTime()
			newest = path
		}
		return nil
	})
	return newest, mt, err
}

func TailRateLimits(path string) (Result, bool) {
	return Tail(KindCodex, path)
}

func Tail(kind Kind, path string) (Result, bool) {
	n := 32 * 1024
	if kind == KindGrok {
		n = 256 * 1024
	}
	b, err := tailBytes(path, n)
	if err != nil || len(b) == 0 {
		return Result{Class: Unknown, Source: "jsonl"}, false
	}
	return classifyTail(kind, string(b))
}

func classifyTail(kind Kind, text string) (Result, bool) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		switch kind {
		case KindCodex:
			if r, ok := classifyCodexLine(line); ok {
				return r, true
			}
		case KindGrok:
			if r, ok := classifyGrokLine(line); ok {
				return r, true
			}
		case KindCursor:
			if r, ok := classifyCursorLine(line); ok {
				return r, true
			}
		default:
			if r, ok := classifyCodexLine(line); ok {
				return r, true
			}
		}
	}
	return Result{Class: Unknown, Source: "jsonl"}, false
}

func classifyCodexLine(line string) (Result, bool) {
	if strings.Contains(line, "rate_limit") {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil {
			rl := lookup(obj, "rate_limits")
			if rl == nil {
				rl = lookup(obj, "rate_limit")
			}
			if rl != nil {
				r := classifyValue(rl, "jsonl", KindCodex)
				if r.Class != Unknown {
					return r, true
				}
			}
		}
	}
	if codexPlanExhaustedText(line) {
		return Result{Class: Exhausted, UsedPct: 100, Source: "jsonl_usage_limit"}, true
	}
	return Result{}, false
}

func classifyGrokLine(line string) (Result, bool) {
	if grokQuotaExhausted(0, line) {
		return Result{Class: Exhausted, UsedPct: 100, Source: "jsonl_402"}, true
	}
	if !strings.Contains(line, "creditUsagePercent") && !strings.Contains(line, "fetched credits") && !strings.Contains(line, "billing") {
		return Result{}, false
	}
	var obj map[string]any
	if json.Unmarshal([]byte(line), &obj) != nil {
		return Result{}, false
	}
	cfg := lookup(obj, "config")
	if cfg == nil {
		cfg = obj
	}
	r := classifyValue(cfg, "jsonl_billing", KindGrok)
	if r.Class == Unknown {
		r = classifyValue(obj, "jsonl_billing", KindGrok)
	}
	if r.Class != Unknown {
		return r, true
	}
	return Result{}, false
}

func classifyCursorLine(line string) (Result, bool) {
	if cursorCapacityText(line) {
		return Result{}, false
	}
	if cursorAccountStoppedText(line) {
		return Result{Class: Exhausted, UsedPct: 100, Source: "jsonl_usage_limit"}, true
	}
	return Result{}, false
}

func lookup(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	for _, v := range m {
		if child, ok := v.(map[string]any); ok {
			if x, ok := child[key]; ok {
				return x
			}
		}
	}
	return nil
}

func tailBytes(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size == 0 {
		return nil, nil
	}
	start := size - int64(n)
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, int64(n)+1))
}

func DirIdle(root string, grace time.Duration, now time.Time) bool {
	newest := time.Time{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if newest.IsZero() {
		return true
	}
	return now.Sub(newest) >= grace
}

func Prefer(a, b Result) Result {
	rank := func(c Class) int {
		switch c {
		case Exhausted:
			return 4
		case Soft:
			return 3
		case OK:
			return 2
		case Expired:
			return 1
		default:
			return 0
		}
	}
	if rank(a.Class) > rank(b.Class) {
		return a
	}
	if rank(b.Class) > rank(a.Class) {
		return b
	}
	if a.UsedPct >= b.UsedPct {
		return a
	}
	return b
}
