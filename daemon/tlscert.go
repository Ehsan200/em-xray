package daemon

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// installTLSCertGen points xray.TLSCertFunc at the embedded xray binary's
// `xray tls cert` command (per project direction), replacing the pure-Go
// fallback. The self-signed keypair xray emits is exactly what tlsSettings
// wants, so no re-encoding is needed beyond joining the PEM lines.
func installTLSCertGen(p paths.Paths) {
	xray.TLSCertFunc = func(sni string) (xray.TLSCert, error) {
		return xrayTLSCert(p, sni)
	}
}

// xrayTLSCert shells out to `xray tls cert` and returns the generated keypair.
// sni is optional: passed as -domain only when non-empty (self-signed TLS works
// by IP with allowInsecure, so a domain isn't required).
func xrayTLSCert(p paths.Paths, sni string) (xray.TLSCert, error) {
	bin, err := xraybin.Extract(p.Cache)
	if err != nil {
		return xray.TLSCert{}, err
	}
	// Default output is JSON arrays of PEM lines; default expiry is only 3 months,
	// so pin ~10 years for a long-lived server cert.
	args := []string{"tls", "cert", "-expire=87600h"}
	if sni != "" {
		args = append(args, "-domain="+sni)
	}
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		return xray.TLSCert{}, fmt.Errorf("xray tls cert: %w", err)
	}
	var parsed struct {
		Certificate []string `json:"certificate"`
		Key         []string `json:"key"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return xray.TLSCert{}, fmt.Errorf("parse xray tls cert output: %w", err)
	}
	if len(parsed.Certificate) == 0 || len(parsed.Key) == 0 {
		return xray.TLSCert{}, fmt.Errorf("xray tls cert returned empty cert/key")
	}
	return xray.TLSCert{
		Certificate: strings.Join(parsed.Certificate, "\n"),
		PrivateKey:  strings.Join(parsed.Key, "\n"),
	}, nil
}
