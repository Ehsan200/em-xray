package xray

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

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

// TLSCert is a self-signed TLS keypair, PEM-encoded. Certificate carries the
// public key (server leaf cert), PrivateKey the matching key. Used for a plain
// vless/vmess/trojan + TLS server that has no CA-issued cert: the client trusts
// it via allowInsecure. "private and public key, nothing else."
type TLSCert struct {
	Certificate string // PEM cert (public)
	PrivateKey  string // PEM EC private key
}

// NewSelfSignedTLS generates a self-signed ECDSA (P-256) cert for sni, valid for
// ~10 years. Deterministic-at-rest: generated once at Materialize and stored, so
// config generation stays byte-stable across reconciles.
func NewSelfSignedTLS(sni string) (TLSCert, error) {
	if sni == "" {
		sni = "www.microsoft.com"
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return TLSCert{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return TLSCert{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: sni},
		DNSNames:              []string{sni},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return TLSCert{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return TLSCert{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return TLSCert{Certificate: string(certPEM), PrivateKey: string(keyPEM)}, nil
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
