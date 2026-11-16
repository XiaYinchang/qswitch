package antigravity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestNativeOAuthScanRequiresBothIDAndVerifiedSecretAcrossChunks(t *testing.T) {
	id := "synthetic.apps.example.invalid"
	secret := "GOCSPX-" + strings.Repeat("x", 28)
	hash := sha256.Sum256([]byte(secret))
	data := bytes.Repeat([]byte{0}, 3*64<<10)
	copy(data[(64<<10)-7:], id)
	copy(data[(2*64<<10)-4:], secret)
	config, err := scanOAuthConfig(bytes.NewReader(data), id, hex.EncodeToString(hash[:]))
	if err != nil || config.ClientID != id || config.ClientSecret != secret {
		t.Fatal("native config spanning chunks was not detected")
	}
	if _, err := scanOAuthConfig(bytes.NewReader(data), "missing-client", hex.EncodeToString(hash[:])); err == nil {
		t.Fatal("accepted a secret without its client identity")
	}
	if _, err := scanOAuthConfig(bytes.NewReader(data), id, strings.Repeat("0", 64)); err == nil {
		t.Fatal("accepted a secret with the wrong hash")
	}
	copy(data[(2*64<<10)-4:], strings.Repeat("0", 35))
	if _, err := scanOAuthConfig(bytes.NewReader(data), id, hex.EncodeToString(hash[:])); err == nil {
		t.Fatal("accepted a client identity without its secret")
	}
}
