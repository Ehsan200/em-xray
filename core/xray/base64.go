package xray

import (
	"encoding/base64"
	"strings"
)

// decodeB64 decodes a base64 string tolerant of the four common variants:
// standard/url-safe alphabets, padded/unpadded. Share links and subscription
// bodies use all of them interchangeably. Returns ok=false if none decode.
func decodeB64(s string) ([]byte, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}
