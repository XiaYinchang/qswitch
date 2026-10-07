package app

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"qswitch/internal/quota"
)

// The Kimi desktop app keeps its web session in Chromium localStorage
// (a leveldb under the app's Application Support dir). The coding /usages
// endpoint no longer carries the monthly window, but the membership gateway
// the app's subscription page uses does. qswitch only ever reads the token:
// refreshing or rotating the desktop login would race the app itself, so an
// expired token simply means no monthly quota this round.
func (a *App) kimiWebSessionToken() (quota.KimiWebSession, string, bool) {
	dir := filepath.Join(a.UserHome, "Library", "Application Support", "kimi-desktop", "Local Storage", "leveldb")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return quota.KimiWebSession{}, "", false
	}
	var logs []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, filepath.Join(dir, e.Name()))
		}
	}
	sort.Slice(logs, func(i, j int) bool {
		fi, fj := logModTime(logs[i]), logModTime(logs[j])
		return fi.After(fj)
	})
	fresh := a.now().Add(30 * time.Second).Unix()
	var best quota.KimiWebSession
	var bestHost string
	for _, path := range logs {
		for _, cand := range kimiWebTokensFromLog(path) {
			s, err := quota.ParseKimiWebToken(cand.token)
			if err != nil || s.ExpiresAt <= fresh {
				continue
			}
			if bestHost == "" || s.ExpiresAt > best.ExpiresAt {
				best, bestHost = s, cand.host
			}
		}
	}
	if bestHost == "" {
		return quota.KimiWebSession{}, "", false
	}
	return best, bestHost, true
}

func logModTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

type kimiWebCandidate struct {
	host  string
	token string
}

// kimiWebTokensFromLog scans a leveldb journal file. Log records are never
// compressed and the desktop app appends every token refresh, so the newest
// .log alone stays current while the app runs.
func kimiWebTokensFromLog(path string) []kimiWebCandidate {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 16<<20 {
		return nil
	}
	var out []kimiWebCandidate
	pos := 0
	for pos+7 <= len(data) {
		length := int(binary.LittleEndian.Uint16(data[pos+4 : pos+6]))
		typ := data[pos+6]
		if pos+7+length > len(data) {
			break
		}
		rec := data[pos+7 : pos+7+length]
		pos += 7 + length
		// Chromium writes standalone batches with the first-fragment type.
		if typ > 1 {
			continue
		}
		out = append(out, kimiBatchTokens(rec)...)
	}
	return out
}

func kimiBatchTokens(rec []byte) []kimiWebCandidate {
	var out []kimiWebCandidate
	if len(rec) < 12 {
		return nil
	}
	i := 12
	for i < len(rec) {
		if rec[i] != 1 { // put
			break
		}
		i++
		klen, kend, ok := ldbVarint(rec, i)
		if !ok || uint64(len(rec)-kend) < klen {
			break
		}
		key := rec[kend : kend+int(klen)]
		i = kend + int(klen)
		vlen, vend, ok := ldbVarint(rec, i)
		if !ok || uint64(len(rec)-vend) < vlen {
			break
		}
		val := rec[vend : vend+int(vlen)]
		i = vend + int(vlen)
		host := kimiLSAccessTokenOrigin(key)
		if host == "" {
			continue
		}
		idx := bytes.Index(val, []byte("eyJhbGciOiJI"))
		if idx < 0 || len(val)-idx > 8192 {
			continue
		}
		out = append(out, kimiWebCandidate{host: host, token: string(val[idx:])})
	}
	return out
}

func kimiLSAccessTokenOrigin(key []byte) string {
	for _, host := range []string{"www.kimi.com", "www.kimi.ai"} {
		prefix := []byte("_https://" + host + "\x00\x01")
		if bytes.HasPrefix(key, prefix) && bytes.HasSuffix(key, []byte("access_token")) {
			return host
		}
	}
	return ""
}

func ldbVarint(b []byte, i int) (uint64, int, bool) {
	var out uint64
	var shift uint
	for {
		if i >= len(b) || shift > 63 {
			return 0, 0, false
		}
		c := b[i]
		i++
		out |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return out, i, true
		}
		shift += 7
	}
}
