// Package zcode captures the desktop's personal Coding Plan for subscription
// monitoring. Desktop account restoration and the independent CLI are excluded.
package zcode

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qswitch/internal/adapter"
)

var (
	ErrReadOnly         = errors.New("zcode: desktop subscription monitoring only; account switching and CLI termination are unsupported")
	ErrCredentialSecret = errors.New("zcode: 无法解密桌面凭据，请确认使用与 ZCode 桌面相同的加密密钥")
	ErrCredentialFormat = errors.New("zcode: 桌面凭据的加密格式无效")
)

const payloadKind = "zcode.desktop-subscription.v1"

type Adapter struct {
	// A getter is scoped to this adapter; each capture reads the key at most
	// once and retries on the next capture after keyring access recovers.
	keyringGet func(service, account string) (string, error)
}

type Creds struct {
	Provider           string              `json:"provider"`
	APIKey             string              `json:"api_key,omitempty"`
	OAuthAccessToken   string              `json:"oauth_access_token,omitempty"`
	ProjectKeyLocation *ProjectKeyLocation `json:"project_key_location,omitempty"`
}

// ProjectKeyLocation identifies the current account's existing personal key;
// APIKeyID must never be sent as quota authorization.
type ProjectKeyLocation struct {
	OrganizationID string `json:"organizationId"`
	ProjectID      string `json:"projectId"`
	APIKeyID       string `json:"apiKeyId"`
}

func (p ProjectKeyLocation) valid() bool {
	return strings.TrimSpace(p.OrganizationID) != "" && strings.TrimSpace(p.ProjectID) != "" && strings.TrimSpace(p.APIKeyID) != ""
}

type payload struct {
	Kind     string           `json:"kind"`
	Identity adapter.Identity `json:"identity"`
	Creds    Creds            `json:"quota_credentials"`
}

func (Adapter) Name() adapter.Tool { return adapter.ZCode }

func credentialsPath(home string) string {
	return filepath.Join(home, ".zcode", "v2", "credentials.json")
}

func (Adapter) LivePaths(home string) []string { return []string{credentialsPath(home)} }

func (Adapter) Fingerprint(home string) ([32]byte, error) {
	raw, err := os.ReadFile(credentialsPath(home))
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

func (a Adapter) Capture(home string) ([]adapter.Blob, []adapter.Warning, error) {
	raw, err := os.ReadFile(credentialsPath(home))
	if err != nil {
		return nil, nil, err
	}
	var store map[string]string
	if json.Unmarshal(raw, &store) != nil || store == nil {
		return nil, nil, errors.New("zcode: invalid desktop credential store")
	}
	reader := credentialReader{home: home, rawStore: string(raw), keyringGet: a.keyringGet}
	provider, err := reader.decrypt(store["oauth:active_provider"])
	if err != nil {
		return nil, nil, err
	}
	if provider != "bigmodel" && provider != "zai" {
		return nil, nil, errors.New("zcode: no supported desktop account is active")
	}
	profileRaw, err := reader.decrypt(store["oauth:"+provider+":user_info"])
	if err != nil {
		return nil, nil, err
	}
	var profile struct {
		ID          string `json:"id"`
		UserID      string `json:"user_id"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Name        string `json:"name"`
		Email       string `json:"email"`
		RawProfile  struct {
			Email string `json:"email"`
		} `json:"rawProfile"`
	}
	if json.Unmarshal([]byte(profileRaw), &profile) != nil {
		return nil, nil, errors.New("zcode: invalid desktop account profile")
	}
	if provider == "zai" && profile.ID == "" {
		profile.ID = profile.UserID
	}
	if strings.TrimSpace(profile.ID) == "" || profile.ID == "unknown" {
		return nil, nil, errors.New("zcode: desktop account profile has no stable ID")
	}
	creds, err := captureCreds(&reader, store, provider, profile.ID)
	if err != nil {
		return nil, nil, err
	}
	id := adapter.Identity{Tool: adapter.ZCode, StableID: provider + ":" + profile.ID, Email: profile.Email, DisplayName: profile.DisplayName}
	if id.Email == "" {
		id.Email = profile.RawProfile.Email
	}
	if id.Email == "" && strings.Contains(profile.Username, "@") {
		id.Email = profile.Username
	}
	if id.DisplayName == "" {
		id.DisplayName = profile.Name
	}
	if id.DisplayName == "" {
		id.DisplayName = profile.Username
	}
	// Persist only the current account's quota authorization. OAuth captures
	// retain the login token and exact personal locator, never a refresh token,
	// another account's key, or the short-lived project token obtained later.
	data, err := json.Marshal(payload{Kind: payloadKind, Identity: id, Creds: creds})
	if err != nil {
		return nil, nil, err
	}
	return []adapter.Blob{{Tool: adapter.ZCode, Identity: id, CapturedAt: time.Now(), Payload: data}}, nil, nil
}

func planKey(provider, id string) string {
	return "account-provider:coding-plan:account:" + provider + "-individual-coding-plan:account:" + encodeURIComponent(id) + ":api-key"
}

func captureCreds(reader *credentialReader, store map[string]string, provider, id string) (Creds, error) {
	creds := Creds{Provider: provider}
	apiKey, err := reader.decrypt(store[planKey(provider, id)])
	if err != nil {
		return Creds{}, err
	}
	if creds.APIKey = strings.TrimSpace(apiKey); creds.APIKey != "" {
		return creds, nil
	}
	location, err := reader.decrypt(store[projectLocationKey(provider, id)])
	if err != nil {
		return Creds{}, err
	}
	if location == "" {
		return Creds{}, errors.New("zcode: 当前桌面账号尚未保存个人项目凭据，请先在 ZCode 桌面使用该账号完成初始化")
	}
	var locator ProjectKeyLocation
	if json.Unmarshal([]byte(location), &locator) != nil || !locator.valid() {
		return Creds{}, errors.New("zcode: 当前桌面账号的个人项目凭据位置无效")
	}
	token, err := reader.decrypt(store["oauth:"+provider+":access_token"])
	if err != nil {
		return Creds{}, err
	}
	if strings.TrimSpace(token) == "" {
		return Creds{}, errors.New("zcode: 当前桌面账号缺少 OAuth 登录凭据，请在 ZCode 桌面重新登录")
	}
	creds.OAuthAccessToken, creds.ProjectKeyLocation = token, &locator
	return creds, nil
}

func projectLocationKey(provider, id string) string {
	origin := "https://bigmodel.cn"
	if provider == "zai" {
		origin = "https://api.z.ai"
	}
	// Match encodeURIComponent(JSON.stringify(...)) exactly; QueryEscape uses
	// '+' for spaces and PathEscape leaves reserved characters unescaped.
	parts := []string{origin, provider, id}
	var tuple strings.Builder
	tuple.WriteByte('[')
	for i, part := range parts {
		if i > 0 {
			tuple.WriteByte(',')
		}
		tuple.WriteByte('"')
		for _, r := range part {
			switch r {
			case '"', '\\':
				tuple.WriteByte('\\')
				tuple.WriteRune(r)
			case '\b':
				tuple.WriteString(`\b`)
			case '\f':
				tuple.WriteString(`\f`)
			case '\n':
				tuple.WriteString(`\n`)
			case '\r':
				tuple.WriteString(`\r`)
			case '\t':
				tuple.WriteString(`\t`)
			default:
				if r < 0x20 {
					fmt.Fprintf(&tuple, `\u%04x`, r)
				} else {
					tuple.WriteRune(r)
				}
			}
		}
		tuple.WriteByte('"')
	}
	tuple.WriteString(",null,1,\"non-team\"]")
	return "project-key-location:v1:" + encodeURIComponent(tuple.String())
}

func encodeURIComponent(value string) string {
	var encoded strings.Builder
	const hex = "0123456789ABCDEF"
	for _, b := range []byte(value) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-_.!~*'()", rune(b)) {
			encoded.WriteByte(b)
		} else {
			encoded.WriteByte('%')
			encoded.WriteByte(hex[b>>4])
			encoded.WriteByte(hex[b&15])
		}
	}
	return encoded.String()
}

func readPayload(blob adapter.Blob) (payload, error) {
	var p payload
	if json.Unmarshal(blob.Payload, &p) != nil || p.Kind != payloadKind ||
		(p.Creds.Provider != "bigmodel" && p.Creds.Provider != "zai") ||
		!strings.HasPrefix(p.Identity.StableID, p.Creds.Provider+":") || strings.TrimSpace(strings.TrimPrefix(p.Identity.StableID, p.Creds.Provider+":")) == "" {
		return payload{}, errors.New("zcode: invalid subscription credential blob")
	}
	legacy := strings.TrimSpace(p.Creds.APIKey) != "" && p.Creds.OAuthAccessToken == "" && p.Creds.ProjectKeyLocation == nil
	oauth := p.Creds.APIKey == "" && strings.TrimSpace(p.Creds.OAuthAccessToken) != "" && p.Creds.ProjectKeyLocation != nil && p.Creds.ProjectKeyLocation.valid()
	if !legacy && !oauth {
		return payload{}, errors.New("zcode: invalid subscription credential blob")
	}
	p.Identity.Tool = adapter.ZCode
	return p, nil
}

func CredsFromBlob(blob adapter.Blob) (Creds, error) {
	p, err := readPayload(blob)
	return p.Creds, err
}

func (Adapter) IdentityOf(blob adapter.Blob) (adapter.Identity, error) {
	p, err := readPayload(blob)
	return p.Identity, err
}

func (Adapter) Restore(string, adapter.Blob, adapter.RestoreOpts) error { return ErrReadOnly }
func (Adapter) KillCLI(string) error                                    { return ErrReadOnly }
func (Adapter) Idle(string, time.Duration) bool                         { return false }
func (Adapter) ManualBlockers(string) (adapter.Holders, error) {
	return adapter.Holders{Why: []string{"zcode subscription monitoring only"}}, nil
}
func (a Adapter) AutoBlockers(home string) (adapter.Holders, error) {
	return a.ManualBlockers(home)
}
