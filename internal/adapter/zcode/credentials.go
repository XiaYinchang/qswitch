package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

const credentialService = "ZCode Credential Encryption"

var (
	ErrCredentialVersion     = errors.New("zcode: 尚不支持此版本的桌面凭据加密格式")
	ErrCredentialSource      = errors.New("zcode: 桌面凭据与当前密钥的加密来源不一致，请使用桌面原有的系统密钥或自定义密钥")
	ErrCredentialKeyMismatch = errors.New("zcode: 当前加密密钥与桌面凭据不匹配")
	ErrOSCredential          = errors.New("zcode: 无法读取 ZCode 系统加密密钥，可能需要授权或解锁系统凭据库")
	ErrOSCredentialPending   = errors.New("zcode: 钥匙串读取未完成，可能需要授权或解锁")
	ErrOSCredentialFormat    = errors.New("zcode: ZCode 系统加密密钥格式无效")
	ErrOSCredentialAmbiguous = errors.New("zcode: 找到多个匹配的 ZCode 系统加密密钥，无法确认应使用哪一个")
)

// credentialReader is deliberately short lived. It never initializes or
// migrates the desktop store, and never retains a failed key lookup globally.
type credentialReader struct {
	home           string
	store          map[string]string
	keyringGet     func(service, account string) (string, error)
	selected       bool
	source, secret string
	selectionErr   error
	keyLoaded      bool
	key            []byte
	keyErr         error
}

func readManagedSecret(path string, private bool) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("zcode: 加密密钥文件必须是可读取的普通文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("zcode: 加密密钥文件必须是可读取的普通文件")
	}
	if private && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("zcode: credential-secret 文件不允许其他用户或用户组读取")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) < 32 || len(raw) > 4096 || !utf8.Valid(raw) {
		return "", errors.New("zcode: 加密密钥文件必须包含 32 至 4096 字节的 UTF-8 内容")
	}
	return string(raw), nil // Whitespace is part of the official secret.
}

func (r *credentialReader) selectSource() error {
	if r.selected {
		return r.selectionErr
	}
	r.selected = true
	r.source, r.secret, r.selectionErr = r.resolveSource()
	return r.selectionErr
}

func (r *credentialReader) resolveSource() (string, string, error) {
	secret, file := os.Getenv("ZCODE_CREDENTIAL_SECRET"), os.Getenv("ZCODE_CREDENTIAL_SECRET_FILE")
	if secret != "" && file != "" {
		return "", "", errors.New("zcode: ZCODE_CREDENTIAL_SECRET 与 ZCODE_CREDENTIAL_SECRET_FILE 不能同时设置")
	}
	if secret != "" {
		return "env", secret, nil
	}
	if file != "" {
		secret, err := readManagedSecret(file, false)
		if err != nil {
			return "", "", errors.New("zcode: 无法读取有效的 ZCODE_CREDENTIAL_SECRET_FILE 加密密钥文件")
		}
		return "env", secret, nil
	}
	dir := filepath.Dir(credentialsPath(r.home))
	marker, err := os.ReadFile(filepath.Join(dir, "credential-source"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("zcode: 无法读取桌面凭据的 credential-source 加密来源标记")
	}
	pinned := err == nil
	if pinned && string(marker) != "os\n" {
		return "", "", errors.New("zcode: 桌面凭据的 credential-source 加密来源标记无效")
	}
	hasEnv := false
	for _, value := range r.store {
		pinned = pinned || strings.HasPrefix(value, "enc:v2:os:")
		hasEnv = hasEnv || strings.HasPrefix(value, "enc:v2:env:")
	}
	if pinned {
		return "os", "", nil
	}
	secret, err = readManagedSecret(filepath.Join(dir, "credential-secret"), true)
	if err == nil {
		return "env", secret, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("zcode: 无法读取有效且权限私有的 credential-secret 加密密钥文件")
	}
	if hasEnv {
		return "", "", errors.New("zcode: 桌面凭据使用自定义加密密钥，但当前未找到对应的密钥配置")
	}
	return "os", "", nil
}

func credentialAccount(home string) (string, error) {
	path, err := filepath.Abs(credentialsPath(home))
	if err != nil {
		return "", ErrOSCredential
	}
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:]), nil
}

func (r *credentialReader) encryptionKey() ([]byte, error) {
	if r.keyLoaded {
		return r.key, r.keyErr
	}
	r.keyLoaded = true
	r.key, r.keyErr = r.loadKey()
	return r.key, r.keyErr
}

func (r *credentialReader) loadKey() ([]byte, error) {
	if err := r.selectSource(); err != nil {
		return nil, err
	}
	if r.source == "env" {
		key := sha256.Sum256([]byte(r.secret))
		return key[:], nil
	}
	account, err := credentialAccount(r.home)
	if err != nil {
		return nil, err
	}
	get := r.keyringGet
	if get == nil {
		get = readOSCredential
	}
	value, err := get(credentialService, account)
	if errors.Is(err, ErrOSCredentialPending) {
		return nil, ErrOSCredentialPending
	}
	if errors.Is(err, ErrOSCredentialFormat) {
		return nil, ErrOSCredentialFormat
	}
	if errors.Is(err, ErrOSCredentialAmbiguous) {
		return nil, ErrOSCredentialAmbiguous
	}
	if err != nil {
		return nil, ErrOSCredential
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(value) != 43 || len(key) != 32 || base64.RawURLEncoding.EncodeToString(key) != value {
		return nil, ErrOSCredentialFormat
	}
	return key, nil
}

func parseCiphertext(value string) (nonce, sealed []byte, err error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return nil, nil, ErrCredentialFormat
	}
	decoded := make([][]byte, 3)
	for i, part := range parts {
		decoded[i], err = base64.RawURLEncoding.Strict().DecodeString(part)
		if err != nil || base64.RawURLEncoding.EncodeToString(decoded[i]) != part {
			return nil, nil, ErrCredentialFormat
		}
	}
	if len(decoded[0]) != 12 || len(decoded[1]) != 16 {
		return nil, nil, ErrCredentialFormat
	}
	return decoded[0], append(decoded[2], decoded[1]...), nil
}

func openCredential(key, nonce, sealed []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", ErrCredentialSecret
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", ErrCredentialSecret
	}
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil || !utf8.Valid(plain) {
		return "", ErrCredentialSecret
	}
	return string(plain), nil
}

func fallbackCredentialSecret(home string) (string, error) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "", ErrCredentialSecret
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return "zcode-credential-fallback:" + platform + ":" + home + ":" + u.Username, nil
}

func credentialSecret(home string) (string, error) {
	if secret := os.Getenv("ZCODE_CREDENTIAL_SECRET"); secret != "" {
		return secret, nil
	}
	return fallbackCredentialSecret(home)
}

func (r *credentialReader) decrypt(value string) (string, error) {
	if !strings.HasPrefix(value, "enc:") {
		return value, nil
	}
	if strings.HasPrefix(value, "enc:v1:") {
		nonce, sealed, err := parseCiphertext(strings.TrimPrefix(value, "enc:v1:"))
		if err != nil {
			return "", err
		}
		if err := r.selectSource(); err != nil {
			return "", err
		}
		seen := map[string]bool{}
		for _, secret := range []string{r.secret, strings.TrimSpace(r.secret)} {
			if secret == "" || seen[secret] {
				continue
			}
			seen[secret] = true
			key := sha256.Sum256([]byte(secret))
			if plain, err := openCredential(key[:], nonce, sealed); err == nil {
				return plain, nil
			}
		}
		// A configured v1 secret must still work in service environments where
		// no OS username is available. Only look it up if fallback is needed.
		fallback, err := fallbackCredentialSecret(r.home)
		if err != nil {
			return "", err
		}
		key := sha256.Sum256([]byte(fallback))
		return openCredential(key[:], nonce, sealed)
	}
	if !strings.HasPrefix(value, "enc:v2:") {
		return "", ErrCredentialVersion
	}
	parts := strings.SplitN(value, ":", 5)
	if len(parts) != 5 || (parts[2] != "os" && parts[2] != "env") || len(parts[3]) != 24 {
		return "", ErrCredentialFormat
	}
	fingerprint, err := hex.DecodeString(parts[3])
	if err != nil || len(fingerprint) != 12 || parts[3] != strings.ToLower(parts[3]) {
		return "", ErrCredentialFormat
	}
	nonce, sealed, err := parseCiphertext(parts[4])
	if err != nil {
		return "", err
	}
	if err := r.selectSource(); err != nil {
		return "", err
	}
	if parts[2] != r.source {
		return "", ErrCredentialSource
	}
	key, err := r.encryptionKey()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(key)
	if parts[3] != hex.EncodeToString(sum[:])[:24] {
		return "", ErrCredentialKeyMismatch
	}
	return openCredential(key, nonce, sealed)
}

func decryptCredential(home, value string) (string, error) {
	r := credentialReader{home: home, store: map[string]string{"value": value}}
	return r.decrypt(value)
}
