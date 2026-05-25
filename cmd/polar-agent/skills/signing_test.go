//go:build unix

package skills

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifySkillSignature_empty(t *testing.T) {
	r := VerifySkillSignature(nil, "")
	if r.HadSignature || r.Verified {
		t.Errorf("empty manifest_preview should yield no-signature result, got %+v", r)
	}
}

func TestVerifySkillSignature_partial(t *testing.T) {
	manifest := json.RawMessage(`{"publisher_pubkey":"abc"}`)
	r := VerifySkillSignature(manifest, "00")
	if !r.HadSignature {
		t.Errorf("HadSignature should be true when one of pub/sig present")
	}
	if r.Verified {
		t.Errorf("partial should not verify")
	}
	if !strings.Contains(r.Reason, "BOTH") {
		t.Errorf("reason should explain BOTH needed, got %q", r.Reason)
	}
}

func TestVerifySkillSignature_validRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	bundle := []byte("pretend this is a .skill ZIP payload")
	sha := sha256.Sum256(bundle)
	sig := ed25519.Sign(priv, sha[:])

	manifest, _ := json.Marshal(map[string]string{
		"publisher_pubkey": base64.StdEncoding.EncodeToString(pub),
		"signature":        base64.StdEncoding.EncodeToString(sig),
	})

	r := VerifySkillSignature(manifest, hex.EncodeToString(sha[:]))
	if !r.HadSignature {
		t.Fatalf("HadSignature = false")
	}
	if !r.Verified {
		t.Errorf("Verified = false, reason=%q", r.Reason)
	}
	if r.Publisher == "" {
		t.Errorf("Publisher empty")
	}
}

func TestVerifySkillSignature_tamperedBundle(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	original := sha256.Sum256([]byte("original"))
	sig := ed25519.Sign(priv, original[:])
	manifest, _ := json.Marshal(map[string]string{
		"publisher_pubkey": base64.StdEncoding.EncodeToString(pub),
		"signature":        base64.StdEncoding.EncodeToString(sig),
	})
	tampered := sha256.Sum256([]byte("TAMPERED"))
	r := VerifySkillSignature(manifest, hex.EncodeToString(tampered[:]))
	if r.Verified {
		t.Errorf("expected verify=false on tampered SHA, got Verified=true")
	}
	if !strings.Contains(r.Reason, "does not verify") {
		t.Errorf("reason = %q, want 'does not verify'", r.Reason)
	}
}

func TestVerifySkillSignature_wrongPubkey(t *testing.T) {
	_, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	sha := sha256.Sum256([]byte("bytes"))
	sig := ed25519.Sign(priv1, sha[:]) // signed with priv1
	manifest, _ := json.Marshal(map[string]string{
		"publisher_pubkey": base64.StdEncoding.EncodeToString(pub2), // but claims pub2
		"signature":        base64.StdEncoding.EncodeToString(sig),
	})
	r := VerifySkillSignature(manifest, hex.EncodeToString(sha[:]))
	if r.Verified {
		t.Errorf("expected verify=false with mismatched key, got Verified=true")
	}
}

func TestVerifySkillSignature_badBase64(t *testing.T) {
	manifest := json.RawMessage(`{"publisher_pubkey":"@@@","signature":"!!!"}`)
	r := VerifySkillSignature(manifest, hex.EncodeToString(make([]byte, 32)))
	if r.Verified {
		t.Errorf("malformed b64 should not verify")
	}
	if !strings.Contains(r.Reason, "base64") {
		t.Errorf("reason = %q, want 'base64'", r.Reason)
	}
}

func TestVerifySkillSignature_wrongSize(t *testing.T) {
	manifest, _ := json.Marshal(map[string]string{
		"publisher_pubkey": base64.StdEncoding.EncodeToString(make([]byte, 16)),    // too short
		"signature":        base64.StdEncoding.EncodeToString(make([]byte, 64)),
	})
	r := VerifySkillSignature(manifest, hex.EncodeToString(make([]byte, 32)))
	if r.Verified {
		t.Errorf("wrong pubkey size should not verify")
	}
}

func TestRequireSigned_envParse(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"no", false},
		{"0", false},
		{"on", false},
	}
	for _, tc := range cases {
		t.Setenv("POLAR_AGENT_REQUIRE_SIGNED", tc.val)
		if got := RequireSigned(); got != tc.want {
			t.Errorf("RequireSigned(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}
