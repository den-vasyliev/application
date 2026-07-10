package push

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
)

// handshakeCanonical builds the canonical string signed to prove the agent holds its
// tenant's HMAC key. It MUST match the triage receiver's handshakeCanonical exactly
// (internal/remoteagent/hmac.go) — separate Go modules, duplicated logic. Format:
//
//	<tenant>\n<clusterName>\n<unixTimestamp>
func handshakeCanonical(tenant, cluster string, ts int64) []byte {
	return []byte(tenant + "\n" + cluster + "\n" + strconv.FormatInt(ts, 10))
}

// signHandshake returns base64(HMAC-SHA256(key, canonical(tenant,cluster,ts))).
func signHandshake(key []byte, tenant, cluster string, ts int64) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(handshakeCanonical(tenant, cluster, ts))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
