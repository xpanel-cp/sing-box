package remotestore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestSignRequestMatchesIndependentHMAC verifies SignRequest produces the exact
// signature the manager's verifier expects, by recomputing it independently:
// lowercase-hex HMAC-SHA256, over the canonical string
// method + timestamp + nonce + hex(sha256(body)), keyed by the node secret.
// This is the same scheme as session-manager/auth.go's SignRequest, so a client
// signature is byte-identical to what the manager recomputes and compares.
func TestSignRequestMatchesIndependentHMAC(t *testing.T) {
	secret := "secret-aaa"
	method := "/xnet.sessionmanager.v1.SessionManager/AcquireSession"
	ts := "1700000000"
	nonce := "cnonce-nodeA-1-abcdef"
	body := []byte("some deterministic proto bytes")

	// Independent reference computation.
	bodySum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(bodySum[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(method + ts + nonce + bodyHash))
	want := hex.EncodeToString(mac.Sum(nil))

	got := SignRequest(secret, method, ts, nonce, body)
	if got != want {
		t.Fatalf("SignRequest = %s, want %s (canonical string / hashing order diverged)", got, want)
	}
}

// TestSignRequestEmptyBody covers the empty-body case (hash of no bytes).
func TestSignRequestEmptyBody(t *testing.T) {
	secret := "s"
	method := "/m"
	ts := "1"
	nonce := "n"

	bodySum := sha256.Sum256(nil)
	bodyHash := hex.EncodeToString(bodySum[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(method + ts + nonce + bodyHash))
	want := hex.EncodeToString(mac.Sum(nil))

	if got := SignRequest(secret, method, ts, nonce, nil); got != want {
		t.Fatalf("SignRequest(empty body) = %s, want %s", got, want)
	}
}
