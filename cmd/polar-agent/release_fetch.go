package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// release_fetch.go — pull a fleet binary (e.g. filmscan) from polar-release and
// keep a verified, cached copy under ~/.polar/bin so the filmscan job runner can
// exec it. Mirrors the SDK SelfUpdate path:
//
//	GET /release/resolve?module&channel&platform
//	  → verify the ed25519 signature over the manifest's canonical bytes
//	  → download dl_url, sha256-check against the (signed) manifest
//	  → atomic-install to ~/.polar/bin/<module> (+0755)
//
// The signature is what makes this safe over an untrusted storage provider: a
// flipped binary fails the sha256 check, and a forged manifest fails the ed25519
// check. Canonical-bytes format MUST match polar-release manifest.go canonicalBytes.

const releaseBinMaxBytes = 512 << 20 // 512 MiB ceiling for a fetched binary

type releaseManifest struct {
	Module     string `json:"module"`
	Version    string `json:"version"`
	Channel    string `json:"channel"`
	Platform   string `json:"platform"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	MinHost    string `json:"min_host"`
	Format     string `json:"format"`
	Entrypoint string `json:"entrypoint"`
}

type resolveResponse struct {
	Version    string          `json:"version"`
	SHA256     string          `json:"sha256"`
	Ed25519Sig string          `json:"ed25519_sig"`
	Manifest   releaseManifest `json:"manifest"`
	DLURL      string          `json:"dl_url"`
	Pubkey     string          `json:"pubkey"`
}

// canonicalBytes reproduces polar-release/internal/release/manifest.go
// canonicalBytes EXACTLY — any drift breaks signature verification.
func (m releaseManifest) canonicalBytes() []byte {
	s := "polar-release/v1\n" +
		"module=" + m.Module + "\n" +
		"version=" + m.Version + "\n" +
		"channel=" + m.Channel + "\n" +
		"platform=" + m.Platform + "\n" +
		"sha256=" + m.SHA256 + "\n" +
		"size=" + strconv.FormatInt(m.Size, 10) + "\n" +
		"min_host=" + m.MinHost + "\n"
	if m.Format != "" {
		s += "format=" + m.Format + "\n"
	}
	if m.Entrypoint != "" {
		s += "entrypoint=" + m.Entrypoint + "\n"
	}
	return []byte(s)
}

// releasePlatform maps the Go runtime arch to a polar-release platform key.
func releasePlatform() string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	}
	return runtime.GOOS + "-" + arch
}

// polarBinDir is ~/.polar/bin (created on demand).
func polarBinDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".polar", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// ensureReleaseBinary fetches module@channel for this host's platform and returns
// the path to a verified, executable local copy. If the cached copy already
// matches the resolved sha256 it is reused without re-downloading.
func ensureReleaseBinary(ctx context.Context, cfg AgentConfig, module, channel string) (string, error) {
	if channel == "" {
		channel = "dev"
	}
	rel, err := resolveRelease(ctx, cfg, module, channel)
	if err != nil {
		return "", err
	}

	// Verify the manifest signature before trusting any of its fields.
	if !verifyReleaseSig(rel) {
		return "", fmt.Errorf("release %s: manifest signature did not verify", module)
	}
	wantSHA := strings.ToLower(strings.TrimSpace(rel.Manifest.SHA256))
	if len(wantSHA) != 64 {
		return "", fmt.Errorf("release %s: bad sha256 in manifest", module)
	}

	binDir, err := polarBinDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(binDir, module)

	// Cache hit: a local copy whose bytes already match the signed sha256.
	if cur, err := os.Open(dest); err == nil {
		h := sha256.New()
		_, _ = io.Copy(h, cur)
		_ = cur.Close()
		if hex.EncodeToString(h.Sum(nil)) == wantSHA {
			return dest, nil
		}
	}

	if rel.DLURL == "" {
		return "", fmt.Errorf("release %s: empty dl_url", module)
	}
	if err := downloadVerified(ctx, rel.DLURL, wantSHA, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// releaseBaseURL is where /release/* lives. On some deployments (e.g. zen) the
// release service is a separate vhost (release.4950.store) from the dock server
// the agent is configured with, so allow an override; default to cfg.Server.
func releaseBaseURL(cfg AgentConfig) string {
	if v := strings.TrimSpace(os.Getenv("POLAR_RELEASE_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(cfg.Server, "/")
}

func resolveRelease(ctx context.Context, cfg AgentConfig, module, channel string) (*resolveResponse, error) {
	base := releaseBaseURL(cfg)
	url := fmt.Sprintf("%s/release/resolve?module=%s&channel=%s&platform=%s",
		base, module, channel, releasePlatform())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resolve %s: http %d: %s", module, resp.StatusCode, truncateForErr(string(body)))
	}
	var rr resolveResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		return nil, fmt.Errorf("resolve %s: decode: %w", module, err)
	}
	return &rr, nil
}

func verifyReleaseSig(rel *resolveResponse) bool {
	pub, err := hex.DecodeString(strings.TrimSpace(rel.Pubkey))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimSpace(rel.Ed25519Sig))
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), rel.Manifest.canonicalBytes(), sig)
}

// downloadVerified streams url → a temp file under the bin dir, checks sha256,
// makes it executable, and atomically renames it into place.
func downloadVerified(ctx context.Context, url, wantSHA, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, releaseBinMaxBytes+1))
	_ = tmp.Close()
	if err != nil {
		return fmt.Errorf("download body: %w", err)
	}
	if n > releaseBinMaxBytes {
		return fmt.Errorf("release binary exceeds %d bytes", releaseBinMaxBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, wantSHA)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpPath, dest)
}
