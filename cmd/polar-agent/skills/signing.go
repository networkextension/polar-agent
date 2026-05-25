//go:build unix

package skills

// signing.go — P2: Ed25519 signature verification for installed bundles.
//
// The signature attests: "publisher P signed bundle B with this SHA256".
// pubkey + signature ride in the install frame's manifest_preview JSON
// (operator put them there when POST /api/skill-catalog'ing). The
// catalog row is the trust root — agent doesn't fetch keys from
// anywhere else.
//
// What the signature covers:
//   - signed message = exactly the 32 raw sha256 bytes of the .skill ZIP
//     (i.e. agent does ed25519.Verify(pubkey, sha256_bytes, sig))
//   - this is the same bytes the catalog publishes as `sha256` (hex)
//
// Why sign the SHA, not the manifest:
//   - bundle content integrity is what we ultimately need
//   - manifest IS in the bundle, so signing the bundle's SHA covers it
//   - keeps signing tooling trivial: `openssl pkeyutl -sign -in sha.bin`
//
// Gate:
//   POLAR_AGENT_REQUIRE_SIGNED=true → reject install unless
//   manifest_preview has BOTH publisher_pubkey AND signature, and the
//   signature verifies.
//   POLAR_AGENT_REQUIRE_SIGNED=false (default) → if pubkey+sig present,
//   verify and reject on mismatch; if absent, allow install.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// signedManifestEnvelope is the subset of manifest_preview the
// signature verifier cares about. Operator-set when registering the
// catalog row; flows through dock + skill.install frame unchanged.
type signedManifestEnvelope struct {
	PublisherPubkey string `json:"publisher_pubkey,omitempty"` // base64 (raw 32 bytes)
	Signature       string `json:"signature,omitempty"`        // base64 (raw 64 bytes)
}

// VerifyResult captures whether signature verification ran + its
// outcome. Used by the installer to decide install/reject and by
// install.result to surface "signed by <publisher>" in the UI.
type VerifyResult struct {
	HadSignature bool   // manifest_preview carried pubkey + sig
	Verified     bool   // ed25519.Verify returned true
	Publisher    string // base64 pubkey, echoed back for audit
	Reason       string // human-readable when !Verified
}

// VerifySkillSignature runs the verification pipeline.
//
//   - manifestPreview: raw JSON from skill.install frame's manifest_preview
//   - sha256Hex: hex-encoded SHA256 of the downloaded .skill ZIP
//
// Returns VerifyResult. Caller checks HadSignature + Verified +
// the global RequireSigned() gate to decide install vs reject.
func VerifySkillSignature(manifestPreview json.RawMessage, sha256Hex string) VerifyResult {
	if len(manifestPreview) == 0 {
		return VerifyResult{}
	}
	var env signedManifestEnvelope
	if err := json.Unmarshal(manifestPreview, &env); err != nil {
		return VerifyResult{Reason: "manifest_preview json: " + err.Error()}
	}
	pubB64 := strings.TrimSpace(env.PublisherPubkey)
	sigB64 := strings.TrimSpace(env.Signature)
	if pubB64 == "" && sigB64 == "" {
		return VerifyResult{}
	}
	if pubB64 == "" || sigB64 == "" {
		return VerifyResult{
			HadSignature: true,
			Reason:       "manifest_preview must include BOTH publisher_pubkey AND signature, not one without the other",
		}
	}
	pubBytes, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return VerifyResult{HadSignature: true, Publisher: pubB64, Reason: "publisher_pubkey base64: " + err.Error()}
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		return VerifyResult{HadSignature: true, Publisher: pubB64,
			Reason: fmt.Sprintf("publisher_pubkey: expected %d bytes, got %d", ed25519.PublicKeySize, len(pubBytes))}
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return VerifyResult{HadSignature: true, Publisher: pubB64, Reason: "signature base64: " + err.Error()}
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return VerifyResult{HadSignature: true, Publisher: pubB64,
			Reason: fmt.Sprintf("signature: expected %d bytes, got %d", ed25519.SignatureSize, len(sigBytes))}
	}
	shaRaw, err := hex.DecodeString(sha256Hex)
	if err != nil || len(shaRaw) != 32 {
		return VerifyResult{HadSignature: true, Publisher: pubB64, Reason: "sha256 hex decode failed"}
	}
	if !ed25519.Verify(ed25519.PublicKey(pubBytes), shaRaw, sigBytes) {
		return VerifyResult{HadSignature: true, Publisher: pubB64,
			Reason: "ed25519 signature does not verify against bundle sha256"}
	}
	return VerifyResult{HadSignature: true, Verified: true, Publisher: pubB64}
}

// RequireSigned reports whether unsigned bundles must be rejected.
// Reads POLAR_AGENT_REQUIRE_SIGNED — true / 1 / yes (case-insensitive)
// turns the gate on. Default off so existing catalog rows without
// signature continue to install.
func RequireSigned() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("POLAR_AGENT_REQUIRE_SIGNED"))) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// ErrSignatureMissing indicates the install gate fired because the
// bundle lacked a publisher_pubkey + signature and the agent is
// configured to require them.
var ErrSignatureMissing = errors.New("bundle is unsigned and POLAR_AGENT_REQUIRE_SIGNED is on")
