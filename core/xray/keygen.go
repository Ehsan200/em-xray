package xray

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

// NewUUID returns a random RFC-4122 v4 UUID string (for vless/vmess client ids).
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// RealityKeys is an X25519 keypair for REALITY, base64 (raw-url) encoded exactly
// as `xray x25519` emits them: the server keeps PrivateKey, the client link
// carries PublicKey (pbk).
type RealityKeys struct {
	PrivateKey string
	PublicKey  string
}

// NewRealityKeys generates a fresh REALITY keypair in pure Go (no exec of xray).
func NewRealityKeys() (RealityKeys, error) {
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return RealityKeys{}, err
	}
	// Clamp per RFC 7748 so the stored private key matches what xray derives.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return RealityKeys{}, err
	}
	enc := base64.RawURLEncoding
	return RealityKeys{
		PrivateKey: enc.EncodeToString(priv[:]),
		PublicKey:  enc.EncodeToString(pub),
	}, nil
}

// NewShortID returns a random REALITY shortId (n bytes, hex). xray accepts 0-8
// byte short ids; 8 is the common default.
func NewShortID(n int) (string, error) {
	if n <= 0 || n > 8 {
		n = 8
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewPassword returns a random URL-safe password (for trojan/ss inbounds).
func NewPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
