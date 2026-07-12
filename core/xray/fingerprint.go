package xray

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Fingerprint is the stable content identity of an outbound: sha256 of the
// outbound JSON with the top-level "tag" removed and all object keys sorted,
// truncated to the first 16 hex chars. Two links that differ only by name/tag
// produce the same fingerprint and dedupe.
//
// Go's encoding/json marshals map[string]any keys in sorted order at every
// depth, so unmarshalling into a map and re-marshalling canonicalises key order
// for free. Array order is preserved (it is semantically meaningful).
func Fingerprint(outboundJSON []byte) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(outboundJSON, &m); err != nil {
		return "", fmt.Errorf("fingerprint: %w", err)
	}
	delete(m, "tag")
	canon, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("fingerprint: %w", err)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])[:16], nil
}
