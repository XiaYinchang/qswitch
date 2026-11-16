package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"qswitch/internal/adapter"
	"qswitch/internal/livefile"
)

const (
	nativeService      = "gemini"
	nativeAccount      = "antigravity"
	maxCredentialBytes = 64 << 10
)

var (
	ErrCredentialNotFound = errors.New("antigravity: credential does not exist")
	ErrKeyringUnavailable = errors.New("native credential backend is not installed")
	// ErrReauthenticationRequired identifies explicitly rejected authorization
	// or a missing refresh token. Transport and service failures stay retryable.
	ErrReauthenticationRequired = errors.New("antigravity: sign in again in the official client")
)

// OAuthToken is the native oauth2.Token JSON representation. Raw credentials
// are retained separately so newer native metadata is not discarded.
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	ExpiresIn    int64     `json:"expires_in,omitempty"`
}

type Credential struct {
	Token        *OAuthToken `json:"token"`
	AuthMethod   string      `json:"auth_method,omitempty"`
	IDTokenEmail string      `json:"id_token_email,omitempty"`
	WriterID     string      `json:"writer_id,omitempty"`
}

// Candidate must never be logged: Raw and Credential contain bearer secrets.
type Candidate struct {
	Source     string
	Raw        json.RawMessage
	Credential Credential
}

type credentialBackend interface {
	Read() ([]byte, error)
	Write([]byte) error
	Delete() error
}

// CredentialPaths lists only native locations, never backups or arbitrary profiles.
func CredentialPaths(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"),
		filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(home, ".gemini", "antigravity", "antigravity-oauth-token"),
	}
}

func ParseCredential(raw []byte) (Credential, error) {
	var c Credential
	if len(raw) == 0 || len(raw) > maxCredentialBytes || json.Unmarshal(raw, &c) != nil {
		return c, errors.New("invalid Antigravity credential JSON")
	}
	if c.Token == nil || (c.Token.AccessToken == "" && c.Token.RefreshToken == "") {
		return c, ErrCredentialNotFound
	}
	if c.AuthMethod != "" && c.AuthMethod != "consumer" && c.AuthMethod != "gcp" {
		return c, errors.New("unsupported Antigravity authentication method")
	}
	return c, nil
}

func ReadCredentials(home string) ([]Candidate, error) {
	return readCredentials(home, nativeCredentialBackend())
}

func readCredentials(home string, backend credentialBackend) ([]Candidate, error) {
	var result []Candidate
	seen := make(map[[32]byte]bool)
	add := func(source string, raw []byte) error {
		c, err := ParseCredential(raw)
		if errors.Is(err, ErrCredentialNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		key := sha256.Sum256(raw)
		if !seen[key] {
			result = append(result, Candidate{source, append([]byte(nil), raw...), c})
			seen[key] = true
		}
		return nil
	}
	raw, err := backend.Read()
	if err == nil {
		if err := add("keyring:"+nativeService+"/"+nativeAccount, raw); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, ErrCredentialNotFound) && !errors.Is(err, ErrKeyringUnavailable) {
		return nil, fmt.Errorf("read Antigravity native credentials: %w", err)
	}
	// Existing fallback files can hold a previously authorized account even
	// while the keyring is active. Read them as candidates, never as live proof.
	for _, path := range CredentialPaths(home) {
		raw, err := readCredentialFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := add(path, raw); err != nil {
			return nil, err
		}
	}
	if len(result) == 0 {
		return nil, ErrCredentialNotFound
	}
	return result, nil
}

func readCredentialFile(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("antigravity: credential file is not a regular file")
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("antigravity: credential file permissions must exclude other users")
	}
	if st.Size() > maxCredentialBytes {
		return nil, errors.New("antigravity: credential file is too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, errors.New("antigravity: credential file changed while opening it")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if len(raw) > maxCredentialBytes {
		return nil, errors.New("antigravity: credential file is too large")
	}
	return raw, err
}

// WriteCredential updates the native shared item and every existing native
// fallback. The caller must hold qswitch's Antigravity lock. Each file is
// atomically replaced; failures restore completed writes from memory.
func WriteCredential(home string, raw []byte) error {
	return writeCredential(home, raw, nativeCredentialBackend(), livefile.AtomicWrite)
}

// WriteCredentialWithRollback retains the exact previous value of every slot
// in memory until the caller finishes live-client synchronization. Call the
// returned function if that synchronization or identity readback fails.
func WriteCredentialWithRollback(home string, raw []byte) (func() error, error) {
	var undo func() error
	err := writeCredentialTransaction(home, raw, nativeCredentialBackend(), livefile.AtomicWrite, &undo)
	return undo, err
}

func writeCredential(home string, raw []byte, backend credentialBackend, writeFile func(string, []byte, os.FileMode) error) error {
	return writeCredentialTransaction(home, raw, backend, writeFile, nil)
}

func writeCredentialTransaction(home string, raw []byte, backend credentialBackend, writeFile func(string, []byte, os.FileMode) error, undo *func() error) error {
	if _, err := parseConsumerCredential(raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return errors.New("invalid Antigravity credential JSON")
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	fields["writer_id"], _ = json.Marshal("qswitch-" + hex.EncodeToString(id))
	next, err := json.Marshal(fields)
	if err != nil {
		return errors.New("encode Antigravity credential")
	}
	previous, keyErr := backend.Read()
	keyAvailable := !errors.Is(keyErr, ErrKeyringUnavailable)
	keyExisted := keyErr == nil
	if keyErr != nil && !errors.Is(keyErr, ErrCredentialNotFound) && !errors.Is(keyErr, ErrKeyringUnavailable) {
		return fmt.Errorf("read native credential before switch: %w", keyErr)
	}
	if keyExisted {
		if err := existingConsumerSlot(previous); err != nil {
			return err
		}
	}
	type snapshot struct {
		path string
		raw  []byte
	}
	var files []snapshot
	for _, path := range CredentialPaths(home) {
		old, err := readCredentialFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := existingConsumerSlot(old); err != nil {
			return err
		}
		files = append(files, snapshot{path, old})
	}
	if !keyAvailable && len(files) == 0 {
		return errors.New("no native keyring or existing Antigravity credential file is available")
	}
	// Detect changes between the snapshot and commit before changing any item.
	for _, f := range files {
		check, err := readCredentialFile(f.path)
		if err != nil || !bytes.Equal(check, f.raw) {
			return errors.New("antigravity: credentials changed during switch preparation")
		}
	}
	if keyAvailable {
		check, err := backend.Read()
		if (keyExisted && (err != nil || !bytes.Equal(check, previous))) || (!keyExisted && !errors.Is(err, ErrCredentialNotFound)) {
			return errors.New("native Antigravity credential changed during switch preparation")
		}
	}
	completed := 0
	keyWritten := false
	rollback := func(cause error) error {
		var rollbackErr error
		for i := completed - 1; i >= 0; i-- {
			f := files[i]
			check, err := readCredentialFile(f.path)
			if err != nil || !bytes.Equal(check, next) {
				rollbackErr = errors.Join(rollbackErr, errors.New("credential file changed externally; rollback refused to overwrite it"))
				continue
			}
			if err := writeFile(f.path, f.raw, 0o600); err != nil {
				rollbackErr = errors.Join(rollbackErr, errors.New("credential file rollback failed"))
			}
		}
		if keyWritten {
			check, err := backend.Read()
			if err != nil || !bytes.Equal(check, next) {
				rollbackErr = errors.Join(rollbackErr, errors.New("native credential changed externally; rollback refused to overwrite it"))
			} else if keyExisted {
				if err := backend.Write(previous); err != nil {
					rollbackErr = errors.Join(rollbackErr, errors.New("native credential rollback failed"))
				}
			} else if err := backend.Delete(); err != nil {
				rollbackErr = errors.Join(rollbackErr, errors.New("native credential rollback failed"))
			}
		}
		return errors.Join(cause, rollbackErr)
	}
	if keyAvailable {
		if err := backend.Write(next); err != nil {
			// A native command can complete its mutation but fail on readback.
			check, readErr := backend.Read()
			if readErr == nil && bytes.Equal(check, next) {
				keyWritten = true
				return rollback(fmt.Errorf("write native Antigravity credential: %w", err))
			}
			return fmt.Errorf("write native Antigravity credential: %w", err)
		}
		keyWritten = true
	}
	for _, f := range files {
		if err := writeFile(f.path, next, 0o600); err != nil {
			return rollback(errors.New("write Antigravity credential file failed"))
		}
		completed++
	}
	if keyWritten {
		check, err := backend.Read()
		if err != nil || !bytes.Equal(check, next) {
			return rollback(errors.New("native Antigravity credential readback failed"))
		}
	}
	for _, f := range files {
		check, err := readCredentialFile(f.path)
		if err != nil || !bytes.Equal(check, next) {
			return rollback(errors.New("antigravity: credential file readback failed"))
		}
	}
	if undo != nil {
		*undo = func() error { return rollback(nil) }
	}
	return nil
}

// Empty native slots are not accounts. Every occupied slot must belong to
// the supported consumer flow before a transaction can replace any of them.
func existingConsumerSlot(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	c, err := ParseCredential(raw)
	if errors.Is(err, ErrCredentialNotFound) {
		return nil
	}
	if err != nil {
		return errors.New("antigravity: existing native credentials are malformed; no credentials were changed")
	}
	if c.AuthMethod != "consumer" {
		return errors.New("antigravity: existing native credentials use unsupported authentication; no credentials were changed")
	}
	return nil
}

type CredentialsClient struct {
	Client *http.Client
	// OAuth is injectable for synthetic tests. Production reads the installed
	// official client and verifies its shared application configuration hash.
	OAuth *OAuthConfig
}

func (c CredentialsClient) client() *http.Client {
	client := &http.Client{Timeout: 20 * time.Second}
	if c.Client != nil {
		clone := *c.Client
		client = &clone
	}
	// Never forward refresh tokens to an HTTP redirect target.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func Refresh(ctx context.Context, raw []byte) ([]byte, error) {
	return (CredentialsClient{}).Refresh(ctx, raw)
}

func (c CredentialsClient) Refresh(ctx context.Context, raw []byte) ([]byte, error) {
	credential, err := ParseCredential(raw)
	if err != nil {
		return nil, err
	}
	if credential.Token.RefreshToken == "" {
		return nil, fmt.Errorf("%w: missing refresh token", ErrReauthenticationRequired)
	}
	config := c.OAuth
	if config == nil {
		native, err := loadNativeOAuthConfig(credential.AuthMethod)
		if err != nil {
			return nil, err
		}
		config = &native
	}
	if config.ClientID == "" || config.ClientSecret == "" {
		return nil, errors.New("antigravity: OAuth application configuration is incomplete")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {credential.Token.RefreshToken}, "client_id": {config.ClientID}, "client_secret": {config.ClientSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("create Antigravity token refresh request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, errors.New("antigravity: token refresh transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, credentialHTTPError("token refresh", resp, false)
	}
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxCredentialBytes)).Decode(&refreshed) != nil || refreshed.AccessToken == "" || refreshed.ExpiresIn <= 0 || refreshed.ExpiresIn > 366*24*60*60 {
		return nil, errors.New("invalid Antigravity token refresh response")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var token map[string]json.RawMessage
	_ = json.Unmarshal(fields["token"], &token)
	token["access_token"], _ = json.Marshal(refreshed.AccessToken)
	token["expires_in"], _ = json.Marshal(refreshed.ExpiresIn)
	token["expiry"], _ = json.Marshal(time.Now().UTC().Add(time.Duration(refreshed.ExpiresIn) * time.Second))
	if refreshed.TokenType != "" {
		token["token_type"], _ = json.Marshal(refreshed.TokenType)
	}
	if refreshed.RefreshToken != "" {
		token["refresh_token"], _ = json.Marshal(refreshed.RefreshToken)
	}
	fields["token"], _ = json.Marshal(token)
	return json.Marshal(fields)
}

func Verify(ctx context.Context, raw []byte) (adapter.Identity, error) {
	return (CredentialsClient{}).Verify(ctx, raw)
}

func (c CredentialsClient) Verify(ctx context.Context, raw []byte) (adapter.Identity, error) {
	credential, err := ParseCredential(raw)
	if err != nil {
		return adapter.Identity{}, err
	}
	if credential.Token.AccessToken == "" {
		return adapter.Identity{}, errors.New("antigravity: credential has no access token")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+credential.Token.AccessToken)
	resp, err := c.client().Do(req)
	if err != nil {
		return adapter.Identity{}, errors.New("antigravity: identity verification transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return adapter.Identity{}, credentialHTTPError("identity verification", resp, true)
	}
	var user struct {
		ID       string `json:"id"`
		Email    string `json:"email"`
		Verified bool   `json:"verified_email"`
		Name     string `json:"name"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxCredentialBytes)).Decode(&user) != nil || user.ID == "" || user.Email == "" || !user.Verified {
		return adapter.Identity{}, errors.New("antigravity: identity is missing or its email is unverified")
	}
	if credential.IDTokenEmail != "" && !strings.EqualFold(credential.IDTokenEmail, user.Email) {
		return adapter.Identity{}, errors.New("antigravity: credential identity does not match its stored email")
	}
	return adapter.Identity{Tool: adapter.Antigravity, StableID: user.ID, Email: user.Email, DisplayName: user.Name}, nil
}

// Only protocol error codes may change retry policy. Provider descriptions,
// response bodies and transport errors can contain credentials and are never
// included in the returned error. A throttled or failing server is not proof
// that the user's authorization has been revoked.
func credentialHTTPError(operation string, resp *http.Response, userInfo bool) error {
	reauthenticate := userInfo && resp.StatusCode == http.StatusUnauthorized
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		var failure struct {
			Code string `json:"error"`
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCredentialBytes+1))
		if err == nil && len(raw) <= maxCredentialBytes && json.Unmarshal(raw, &failure) == nil {
			switch failure.Code {
			case "invalid_grant", "invalid_token":
				reauthenticate = true
			}
		}
	}
	if reauthenticate {
		return fmt.Errorf("%w: %s HTTP %d", ErrReauthenticationRequired, operation, resp.StatusCode)
	}
	return fmt.Errorf("antigravity: %s HTTP %d", operation, resp.StatusCode)
}
