package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// SourceAdapter is one source provider's inbound edge (issue #21): it
// authenticates the raw request and translates the payload into a
// normalized TriggerEvent. Every adapter feeds the same Handler gate, so
// dedup, policy, and the durable decision stay identical across sources.
// Adding a provider (GitLab, Jira) means one adapter plus one route
// registration — policy and storage never change.
type SourceAdapter interface {
	// Verify authenticates the raw request before anything is recorded.
	// A non-nil error rejects the delivery (401): nothing is parsed or
	// persisted, so forged traffic leaves no audit noise. Adapters with
	// no configured credential return nil (documented dev mode).
	Verify(r *http.Request, body []byte) error
	// Parse translates the verified request into a TriggerEvent. A
	// non-empty ignoreReason means the delivery is legitimate but not
	// work (wrong event type, non-triggering action): the caller answers
	// 202 and records nothing. A non-nil error means the delivery is
	// malformed (400) and likewise records nothing.
	Parse(r *http.Request, body []byte) (ev TriggerEvent, ignoreReason string, err error)
}

// errBadSignature rejects deliveries whose signature is missing or
// mismatched; the message stays generic so probes learn nothing.
var errBadSignature = errors.New("ingest: invalid webhook signature")

// errBadBearer rejects API submissions with a missing or wrong token.
var errBadBearer = errors.New("ingest: invalid bearer token")

// verifyHMACSHA256 reports whether header carries the hex HMAC-SHA256 of
// body under secret, compared in constant time. An empty secret means no
// credential is configured: verification is skipped (dev mode).
// Inputs: signature header value, the required prefix ("sha256=" for
// GitHub, "" for Linear's bare hex), the raw body, and the configured
// secret. Returns errBadSignature on a missing, malformed, or mismatched
// signature.
func verifyHMACSHA256(header, prefix string, body []byte, secret string) error {
	if secret == "" {
		return nil
	}
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, prefix) {
		return errBadSignature
	}
	got, err := hex.DecodeString(header[len(prefix):])
	if err != nil || len(got) != sha256.Size {
		return errBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if subtle.ConstantTimeCompare(mac.Sum(nil), got) != 1 {
		return errBadSignature
	}
	return nil
}

// verifyBearer reports whether the Authorization header carries exactly
// "Bearer <secret>", compared in constant time. The Bearer scheme is
// required: a bare secret without the scheme is rejected. An empty
// secret is a configuration error: the route must not be reachable.
func verifyBearer(r *http.Request, secret string) error {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return errBadBearer
	}
	if subtle.ConstantTimeCompare([]byte(header[len("Bearer "):]), []byte(secret)) != 1 {
		return errBadBearer
	}
	return nil
}

// labelName is the shared {name} shape both webhook providers use for
// issue labels inside their payloads.
type labelName struct {
	Name string `json:"name"`
}

// labelNames extracts unique non-empty label names in payload order.
func labelNames(labels []labelName) []string {
	seen := make(map[string]bool, len(labels))
	var out []string
	for _, l := range labels {
		if l.Name != "" && !seen[l.Name] {
			seen[l.Name] = true
			out = append(out, l.Name)
		}
	}
	return out
}
