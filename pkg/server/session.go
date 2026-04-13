package server

// session.go — stateless HMAC-SHA256 browser session tokens (ADR-0006).
//
// Token format: base64url(payload_json) + "." + base64url(hmac_sha256(payload))
// No header segment — the algorithm is fixed (HS256) and the token is internal-only.
//
// Sliding expiry: if the session was last touched more than 30 minutes ago,
// the caller re-signs and re-issues the cookie. Absolute TTL is 12 hours.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	sessionTTL      = 12 * time.Hour
	sessionSlideMin = 30 * time.Minute
)

// sessionPayload is the signed JSON body of a browser session token.
type sessionPayload struct {
	// Kid is the first 8 characters of the raw admin key used to log in.
	// Used for debugging only — never used for key lookup.
	Kid   string `json:"kid"`
	Scope string `json:"scope"`
	IAT   int64  `json:"iat"`   // issued-at (unix)
	EXP   int64  `json:"exp"`   // absolute expiry (unix)
	Slide int64  `json:"slide"` // last-activity timestamp (unix)
}

// sessionResult is what the auth middleware returns after a successful cookie validation.
type sessionResult struct {
	payload    sessionPayload
	needsReissue bool // true when caller should re-sign and re-set the cookie
}

// errSessionInvalid is returned by validateSessionToken for any invalid token.
var errSessionInvalid = errors.New("session: invalid or expired token")

// signSessionToken signs a sessionPayload and returns a compact token string.
func signSessionToken(secret []byte, p sessionPayload) (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	sig := hmacSign(secret, encoded)
	return encoded + "." + sig, nil
}

// validateSessionToken parses and verifies a token. Returns errSessionInvalid on
// any failure (tampered, expired, malformed). The returned sessionResult.needsReissue
// is true when the session should be slid forward (>30m since last touch).
func validateSessionToken(secret []byte, token string) (sessionResult, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return sessionResult{}, errSessionInvalid
	}
	encoded, sig := parts[0], parts[1]

	// Verify signature first.
	expected := hmacSign(secret, encoded)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return sessionResult{}, errSessionInvalid
	}

	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return sessionResult{}, errSessionInvalid
	}

	var p sessionPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return sessionResult{}, errSessionInvalid
	}

	now := time.Now().Unix()
	if now > p.EXP {
		return sessionResult{}, errSessionInvalid
	}

	needsReissue := (now - p.Slide) > int64(sessionSlideMin.Seconds())
	return sessionResult{payload: p, needsReissue: needsReissue}, nil
}

// newSessionPayload builds a fresh sessionPayload for the given admin key prefix.
func newSessionPayload(keyPrefix string) sessionPayload {
	now := time.Now().Unix()
	return sessionPayload{
		Kid:   keyPrefix,
		Scope: "admin",
		IAT:   now,
		EXP:   now + int64(sessionTTL.Seconds()),
		Slide: now,
	}
}

// slideSessionPayload returns a copy of p with an updated Slide timestamp and
// a fresh absolute TTL from now. Per ADR-0006, the absolute window resets on
// each slide so active sessions never expire mid-use.
func slideSessionPayload(p sessionPayload) sessionPayload {
	now := time.Now().Unix()
	p.Slide = now
	p.EXP = now + int64(sessionTTL.Seconds())
	return p
}

// hmacSign returns a base64url-encoded HMAC-SHA256 of data using secret.
func hmacSign(secret []byte, data string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// generateSessionSecret generates a 32-byte random secret and returns its
// hex encoding. Used when CLONR_SESSION_SECRET is not set.
func generateSessionSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
