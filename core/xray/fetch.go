package xray

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Fetch tunables.
const (
	MaxSubBytes   = 8 << 20 // 8 MiB body cap
	SubSchedTick  = 60 * time.Second
	SubErrorRetry = 5 * time.Minute
)

// Quota is the usage/limit info from a Subscription-Userinfo response header.
// HasData is false when the header was absent, so callers can avoid clobbering
// previously-persisted figures with zeros.
type Quota struct {
	Upload   int64
	Download int64
	Total    int64
	Expire   int64 // unix seconds
	HasData  bool
}

// FetchSubscriptionBody GETs a subscription URL with the given User-Agent
// (defaulting to v2rayN), enforcing the 8 MiB cap, and parses the quota header.
func FetchSubscriptionBody(ctx context.Context, client *http.Client, rawURL, userAgent string) ([]byte, Quota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, Quota{}, err
	}
	req.Header.Set("User-Agent", orDefault(userAgent, DefaultSubUserAgent))

	resp, err := client.Do(req)
	if err != nil {
		return nil, Quota{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Quota{}, fmt.Errorf("subscription fetch: status %s", resp.Status)
	}

	// LimitReader to cap+1 so we can detect an over-size body without buffering it all.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxSubBytes+1))
	if err != nil {
		return nil, Quota{}, err
	}
	if len(body) > MaxSubBytes {
		return nil, Quota{}, fmt.Errorf("subscription body exceeds %d bytes", MaxSubBytes)
	}
	return body, ParseUserinfo(resp.Header.Get("Subscription-Userinfo")), nil
}

// ParseUserinfo parses `upload=..; download=..; total=..; expire=..`.
func ParseUserinfo(header string) Quota {
	q := Quota{}
	header = strings.TrimSpace(header)
	if header == "" {
		return q
	}
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			q.Upload, q.HasData = n, true
		case "download":
			q.Download, q.HasData = n, true
		case "total":
			q.Total, q.HasData = n, true
		case "expire":
			q.Expire, q.HasData = n, true
		}
	}
	return q
}

// DecodeSubscription returns the list of share-link lines from a subscription
// body, transparently base64-decoding a wholly-encoded body. A plaintext body
// (link lines contain "://", which is not valid base64) is used as-is.
func DecodeSubscription(body []byte) []string {
	text := strings.TrimSpace(string(body))
	if dec, ok := decodeB64(text); ok && strings.Contains(string(dec), "://") {
		text = string(dec)
	}
	var links []string
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "://") {
			continue
		}
		links = append(links, line)
	}
	return links
}

// ParseSubscriptionBody decodes a body, parses each link, dedupes by content
// fingerprint, and prefixes each node's display name with the subscription name.
// Unparseable links are skipped (a subscription commonly mixes a few unsupported
// schemes). Insertion order is preserved for the active-cap computation.
func ParseSubscriptionBody(subName string, body []byte) ([]SubNode, error) {
	links := DecodeSubscription(body)
	nodes := make([]SubNode, 0, len(links))
	seen := make(map[string]bool, len(links))
	for _, link := range links {
		pl, err := ParseLink(link)
		if err != nil {
			continue
		}
		fp, err := Fingerprint(pl.Outbound)
		if err != nil {
			continue
		}
		if seen[fp] {
			continue
		}
		seen[fp] = true
		name := pl.Name
		if subName != "" {
			name = subName + " / " + pl.Name
		}
		nodes = append(nodes, SubNode{
			Name:        name,
			Fingerprint: fp,
			Outbound:    string(pl.Outbound),
		})
	}
	return nodes, nil
}

// IsDue reports whether a subscription should be refreshed at time now: never
// fetched, past its interval, or (after an error) past the error-retry window.
// Disabled subscriptions are never due.
func IsDue(sub Subscription, now time.Time) bool {
	if !sub.Enabled {
		return false
	}
	if sub.LastFetched.IsZero() {
		return true
	}
	if sub.LastError != "" {
		return now.After(sub.LastFetched.Add(SubErrorRetry))
	}
	return now.After(sub.LastFetched.Add(sub.EffectiveInterval()))
}
