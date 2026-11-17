// Package zcode captures the desktop's personal Coding Plan for subscription
// monitoring. Desktop account restoration and the independent CLI are excluded.
package zcode

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	Provider string `json:"provider"`
	APIKey   string `json:"api_key"`
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
	reader := credentialReader{home: home, store: store, keyringGet: a.keyringGet}
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
	apiKey, err := reader.decrypt(store[planKey(provider, profile.ID)])
	if err != nil {
		return nil, nil, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, nil, errors.New("zcode: current desktop account has no saved personal Coding Plan key")
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
	// The qswitch encrypted blob needs only this account's quota key. Do not
	// copy OAuth tokens, other accounts, provider settings, or CLI credentials.
	data, err := json.Marshal(payload{Kind: payloadKind, Identity: id, Creds: Creds{Provider: provider, APIKey: apiKey}})
	if err != nil {
		return nil, nil, err
	}
	return []adapter.Blob{{Tool: adapter.ZCode, Identity: id, CapturedAt: time.Now(), Payload: data}}, nil, nil
}

func planKey(provider, id string) string {
	return "account-provider:coding-plan:account:" + provider + "-individual-coding-plan:account:" + id + ":api-key"
}

func readPayload(blob adapter.Blob) (payload, error) {
	var p payload
	if json.Unmarshal(blob.Payload, &p) != nil || p.Kind != payloadKind || p.Identity.StableID == "" ||
		(p.Creds.Provider != "bigmodel" && p.Creds.Provider != "zai") || strings.TrimSpace(p.Creds.APIKey) == "" {
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
