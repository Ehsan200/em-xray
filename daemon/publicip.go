package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// publicIPServices are queried in order until one returns a valid IP.
var publicIPServices = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
}

// detectPublicIP best-effort resolves this host's public IP by asking a few
// echo services. Returns "" if none answer with a valid address.
func detectPublicIP(ctx context.Context) string {
	client := &http.Client{Timeout: 3 * time.Second}
	for _, url := range publicIPServices {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	return ""
}

// publicIP returns the cached detected public IP, resolving it on first use.
// A failed detection is not cached, so a later call retries. Returns "" when
// undetectable (share links then carry the SERVER_IP placeholder).
func (s *Server) publicIP() string {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.pubIP != "" {
		return s.pubIP
	}
	if ip := detectPublicIP(context.Background()); ip != "" {
		s.pubIP = ip
	}
	return s.pubIP
}
