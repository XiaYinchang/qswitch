package zcode

func windowsCredentialTarget(service, account string) string { return account + "." + service }

// A native ZCode key is exactly 43 base64url ASCII characters stored as
// UTF-16LE, without a NUL terminator. Reject any other encoding, rather than
// trimming or guessing whether the blob belongs to a different keyring API.
func decodeWindowsCredential(raw []byte) (string, error) {
	if len(raw) != 86 {
		return "", ErrOSCredentialFormat
	}
	value := make([]byte, 43)
	for i := range value {
		if raw[2*i+1] != 0 || raw[2*i] > 127 {
			return "", ErrOSCredentialFormat
		}
		value[i] = raw[2*i]
	}
	return string(value), nil
}
