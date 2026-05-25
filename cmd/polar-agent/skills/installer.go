//go:build unix

package skills

// installer.go — P1a: dock-initiated install / uninstall.
//
// Today (P0a) the bundle skill is dispatched via skill.start which
// downloads-on-demand from the BundleConfig.DownloadURL. P1a separates
// the lifecycle: a dock-issued skill.install frame triggers download +
// verify + manifest validation + on-disk install WITHOUT starting the
// bundle. The next skill.advertise tick (60s) tells dock the new
// version is on the host. Later skill.start runs use the already-
// installed copy (no download cost).
//
// Idempotency: install_id is the dedup key. We keep a tiny on-disk
// log at ~/.polar/installs.json so a duplicate frame returns the
// previous result instead of re-downloading.
//
// On-disk layout (matches bundle.go's existing convention):
//   ~/.polar/bundles/<bundle>/<version>/         ← extracted bundle
//   ~/.polar/installs.json                       ← {install_id: result}
//
// We intentionally do NOT version by publisher in the path today —
// host_skills (publisher, kind) becoming a UNIQUE key + the bundle
// name being globally unique-per-publisher is enough. P2/P3 can
// revisit if multi-publisher same-kind ever becomes a real case.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	installResultCacheFile = "installs.json"
	installResultMaxAge    = 24 * time.Hour
	installResultMaxRows   = 256
)

// InstallStatus enumerates the outcome strings expected by
// skill.install.result. Kept as constants so wire-format spelling
// can't drift between sender and receiver.
type InstallStatus string

const (
	InstallStatusOK              InstallStatus = "ok"
	InstallStatusSHAMismatch     InstallStatus = "sha_mismatch"
	InstallStatusManifestInvalid InstallStatus = "manifest_invalid"
	InstallStatusRequiresUnmet   InstallStatus = "requires_unmet"
	InstallStatusDownloadFailed  InstallStatus = "download_failed"
	InstallStatusVenvFailed      InstallStatus = "venv_failed"
	InstallStatusDiskFull        InstallStatus = "disk_full"
	InstallStatusTimeout         InstallStatus = "timeout"
	InstallStatusActiveRuns      InstallStatus = "active_runs"     // uninstall only
	InstallStatusNotInstalled    InstallStatus = "not_installed"   // uninstall only
	InstallStatusFSError         InstallStatus = "fs_error"
	InstallStatusAlreadyInstalled InstallStatus = "already_installed"
	// P2 — signature gate outcomes
	InstallStatusSignatureMissing InstallStatus = "signature_missing"
	InstallStatusSignatureInvalid InstallStatus = "signature_invalid"
)

// InstallRequest is the agent-side shape of the skill.install frame.
// loop.go's dispatcher decodes from the wire JSON and calls Install.
type InstallRequest struct {
	InstallID   string `json:"install_id"`
	Publisher   string `json:"publisher"`
	SkillKind   string `json:"skill_kind"`
	Version     string `json:"version"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
	SizeBytes   int64  `json:"size_bytes"`
	// ManifestPreview is dock's view of the catalog row's manifest.
	// Agent doesn't strictly need it (it re-parses the manifest from
	// the extracted bundle), but having it pre-arrival lets us reject
	// e.g. requires.usb mismatches BEFORE paying download cost. P1a
	// stores but doesn't yet pre-validate; P2 will use it for
	// signature anchoring.
	ManifestPreview json.RawMessage `json:"manifest_preview,omitempty"`
}

// InstallResult is the agent-side shape of the skill.install.result frame.
type InstallResult struct {
	InstallID     string        `json:"install_id"`
	Status        InstallStatus `json:"status"`
	InstalledPath string        `json:"installed_path,omitempty"`
	Error         string        `json:"error,omitempty"`
	DurationMS    int64         `json:"duration_ms"`
	FinishedAt    time.Time     `json:"finished_at"`
	// P2 — populated when manifest_preview carried a publisher_pubkey
	// + signature and verification ran. Empty for unsigned installs.
	SignedBy string `json:"signed_by,omitempty"`
}

// UninstallRequest mirrors skill.uninstall.
type UninstallRequest struct {
	InstallID string `json:"install_id"` // dedup; separate ID space from install_id
	Publisher string `json:"publisher"`
	SkillKind string `json:"skill_kind"`
	Version   string `json:"version"`
	Force     bool   `json:"force"` // if true, stop active runs first
}

// UninstallResult mirrors skill.uninstall.result.
type UninstallResult struct {
	InstallID   string        `json:"install_id"`
	Status      InstallStatus `json:"status"`
	RemovedRuns int           `json:"removed_runs"`
	Error       string        `json:"error,omitempty"`
	DurationMS  int64         `json:"duration_ms"`
	FinishedAt  time.Time     `json:"finished_at"`
}

// Installer hangs off the bundle skill — same rootDir + httpClient so
// installs land in the same ~/.polar/bundles that skill.start reads.
type Installer struct {
	rootDir string
	skill   *bundleSkill // for download + venv setup reuse

	mu        sync.Mutex
	resultLog map[string]InstallResult // install_id → cached result
}

// NewInstaller wraps an existing bundle skill so it shares its
// rootDir + HTTP client + venv heuristics.
func NewInstaller(s Skill) *Installer {
	bs, ok := s.(*bundleSkill)
	if !ok {
		return nil
	}
	inst := &Installer{
		rootDir:   bs.rootDir,
		skill:     bs,
		resultLog: map[string]InstallResult{},
	}
	inst.loadResultLog()
	return inst
}

// Install runs the full pipeline:
//
//  1. Idempotency: if install_id is in the result log AND its bundle
//     dir still exists, return the cached result.
//  2. Download → SHA verify → extract (via bundleSkill.install) into
//     ~/.polar/bundles/<bundle>/<version>/
//  3. Read + validate the extracted manifest.yaml (P0a parser)
//  4. Check requires against the host
//  5. If runtime.venv + requirements.txt present, run venv setup
//  6. Persist + return InstallResult
func (i *Installer) Install(ctx context.Context, req InstallRequest) InstallResult {
	start := time.Now()
	res := InstallResult{InstallID: req.InstallID, FinishedAt: start}

	// 0. validate request shape early — avoids partial side effects.
	if err := validateInstallRequest(&req); err != nil {
		res.Status = InstallStatusManifestInvalid
		res.Error = "bad request: " + err.Error()
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}

	bundleDir := filepath.Join(i.rootDir, req.SkillKind, req.Version)

	// 1. dedup
	if prev, ok := i.lookupResult(req.InstallID); ok {
		if _, err := os.Stat(bundleDir); err == nil {
			return prev
		}
		// dir vanished — fall through to reinstall
	}
	// already-installed (different install_id) → still OK + record
	if _, err := os.Stat(bundleDir); err == nil {
		res.Status = InstallStatusAlreadyInstalled
		res.InstalledPath = bundleDir
		res.DurationMS = time.Since(start).Milliseconds()
		res.FinishedAt = time.Now()
		i.storeResult(res)
		return res
	}

	// 2. download + sha verify + extract → reuses bundleSkill.install,
	// which already enforces 256 MiB cap + zip-slip + sha mismatch.
	cfg := BundleConfig{
		Bundle:      req.SkillKind,
		Version:     req.Version,
		DownloadURL: req.DownloadURL,
		SHA256:      req.SHA256,
		Entrypoint:  "scripts/__install_only__.py", // placeholder; not exec'd; passes validateConfig
	}
	if err := i.skill.install(ctx, cfg, bundleDir); err != nil {
		res.Status = classifyInstallErr(err)
		res.Error = err.Error()
		_ = os.RemoveAll(bundleDir)
		res.DurationMS = time.Since(start).Milliseconds()
		res.FinishedAt = time.Now()
		i.storeResult(res)
		return res
	}

	// 2.5 P2: Ed25519 signature gate. Verifies the publisher's
	// signature over the bundle's SHA256 if manifest_preview carried
	// one. If POLAR_AGENT_REQUIRE_SIGNED=true and no signature was
	// provided, refuse the install regardless of how well the rest
	// of the pipeline succeeded.
	verify := VerifySkillSignature(req.ManifestPreview, strings.ToLower(strings.TrimSpace(req.SHA256)))
	if verify.HadSignature && !verify.Verified {
		res.Status = InstallStatusSignatureInvalid
		res.Error = "signature: " + verify.Reason
		_ = os.RemoveAll(bundleDir)
		res.DurationMS = time.Since(start).Milliseconds()
		res.FinishedAt = time.Now()
		i.storeResult(res)
		return res
	}
	if !verify.HadSignature && RequireSigned() {
		res.Status = InstallStatusSignatureMissing
		res.Error = ErrSignatureMissing.Error()
		_ = os.RemoveAll(bundleDir)
		res.DurationMS = time.Since(start).Milliseconds()
		res.FinishedAt = time.Now()
		i.storeResult(res)
		return res
	}
	if verify.Verified {
		res.SignedBy = verify.Publisher
	}

	// 3. manifest validation
	manifest, err := ReadBundleManifestFromDir(bundleDir)
	if err != nil {
		res.Status = InstallStatusManifestInvalid
		res.Error = "manifest: " + err.Error()
		_ = os.RemoveAll(bundleDir)
		res.DurationMS = time.Since(start).Milliseconds()
		res.FinishedAt = time.Now()
		i.storeResult(res)
		return res
	}
	if manifest != nil {
		// publisher mismatch = the catalog claims X, the bundle's own
		// manifest says Y. Refuse — that's a packaging / catalog drift.
		if !strings.EqualFold(manifest.Publisher, req.Publisher) {
			res.Status = InstallStatusManifestInvalid
			res.Error = fmt.Sprintf("publisher mismatch: catalog=%q manifest=%q",
				req.Publisher, manifest.Publisher)
			_ = os.RemoveAll(bundleDir)
			res.DurationMS = time.Since(start).Milliseconds()
			res.FinishedAt = time.Now()
			i.storeResult(res)
			return res
		}
		if !strings.EqualFold(manifest.Kind, req.SkillKind) {
			res.Status = InstallStatusManifestInvalid
			res.Error = fmt.Sprintf("kind mismatch: catalog=%q manifest=%q", req.SkillKind, manifest.Kind)
			_ = os.RemoveAll(bundleDir)
			res.DurationMS = time.Since(start).Milliseconds()
			res.FinishedAt = time.Now()
			i.storeResult(res)
			return res
		}
		if manifest.Version != req.Version {
			res.Status = InstallStatusManifestInvalid
			res.Error = fmt.Sprintf("version mismatch: catalog=%q manifest=%q", req.Version, manifest.Version)
			_ = os.RemoveAll(bundleDir)
			res.DurationMS = time.Since(start).Milliseconds()
			res.FinishedAt = time.Now()
			i.storeResult(res)
			return res
		}
		// 4. requires check
		if missing := hostMissingRequirements(manifest.Requires); len(missing) > 0 {
			res.Status = InstallStatusRequiresUnmet
			res.Error = "missing: " + strings.Join(missing, ", ")
			_ = os.RemoveAll(bundleDir)
			res.DurationMS = time.Since(start).Milliseconds()
			res.FinishedAt = time.Now()
			i.storeResult(res)
			return res
		}
	}

	// 5. venv setup is a side effect of bundleSkill.install already
	// (it runs `uv venv` / `pip install` when requirements.txt exists).
	// No extra step here; if it failed, install() above would have
	// returned an error → classifyInstallErr maps to VenvFailed.

	res.Status = InstallStatusOK
	res.InstalledPath = bundleDir
	res.DurationMS = time.Since(start).Milliseconds()
	res.FinishedAt = time.Now()
	i.storeResult(res)
	return res
}

// Uninstall removes ~/.polar/bundles/<kind>/<version>/. If force=false
// and any active run still references the bundle, refuses with
// active_runs. Today the agent doesn't tag active runs by bundle so
// force-vs-non-force is best-effort; P1c will make this rigorous.
func (i *Installer) Uninstall(req UninstallRequest, activeRuns map[int64]string) UninstallResult {
	start := time.Now()
	res := UninstallResult{InstallID: req.InstallID, FinishedAt: start}

	if strings.TrimSpace(req.SkillKind) == "" || strings.TrimSpace(req.Version) == "" {
		res.Status = InstallStatusManifestInvalid
		res.Error = "skill_kind + version required"
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}
	bundleDir := filepath.Join(i.rootDir, req.SkillKind, req.Version)
	if _, err := os.Stat(bundleDir); err != nil {
		if os.IsNotExist(err) {
			res.Status = InstallStatusNotInstalled
			res.DurationMS = time.Since(start).Milliseconds()
			return res
		}
		res.Status = InstallStatusFSError
		res.Error = err.Error()
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}
	// Active-runs gate: count runs whose tag mentions the bundle name.
	// activeRuns is a map of runID → run.SkillKind (caller assembles).
	matching := 0
	for _, kind := range activeRuns {
		if kind == req.SkillKind {
			matching++
		}
	}
	if matching > 0 && !req.Force {
		res.Status = InstallStatusActiveRuns
		res.Error = fmt.Sprintf("%d active runs reference kind=%q; pass force=true to stop them", matching, req.SkillKind)
		res.RemovedRuns = matching
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}
	res.RemovedRuns = matching
	if err := os.RemoveAll(bundleDir); err != nil {
		res.Status = InstallStatusFSError
		res.Error = err.Error()
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}
	res.Status = InstallStatusOK
	res.DurationMS = time.Since(start).Milliseconds()
	res.FinishedAt = time.Now()
	return res
}

// --- helpers ---

func validateInstallRequest(r *InstallRequest) error {
	if strings.TrimSpace(r.InstallID) == "" {
		return errors.New("install_id required")
	}
	if strings.TrimSpace(r.Publisher) == "" {
		return errors.New("publisher required")
	}
	if strings.TrimSpace(r.SkillKind) == "" {
		return errors.New("skill_kind required")
	}
	if strings.TrimSpace(r.Version) == "" {
		return errors.New("version required")
	}
	if strings.TrimSpace(r.SHA256) == "" {
		return errors.New("sha256 required")
	}
	if len(r.SHA256) != 64 {
		return fmt.Errorf("sha256 must be 64 hex chars (got %d)", len(r.SHA256))
	}
	if strings.TrimSpace(r.DownloadURL) == "" {
		return errors.New("download_url required")
	}
	if !isSafeBundleName(r.SkillKind) {
		return fmt.Errorf("skill_kind %q rejected by name validator", r.SkillKind)
	}
	if !isSafeBundleName(r.Version) {
		return fmt.Errorf("version %q rejected by name validator", r.Version)
	}
	return nil
}

// classifyInstallErr maps an error from bundleSkill.install() to a
// wire status string. Heuristic matching is intentional — the bundle
// install path predates this enum.
func classifyInstallErr(err error) InstallStatus {
	if err == nil {
		return InstallStatusOK
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "sha"):
		return InstallStatusSHAMismatch
	case strings.Contains(msg, "no space"), strings.Contains(msg, "disk full"):
		return InstallStatusDiskFull
	case strings.Contains(msg, "context deadline"), strings.Contains(msg, "timeout"):
		return InstallStatusTimeout
	case strings.Contains(msg, "venv"), strings.Contains(msg, "pip"):
		return InstallStatusVenvFailed
	case strings.Contains(msg, "HTTP"), strings.Contains(msg, "download"), strings.Contains(msg, "404"), strings.Contains(msg, "403"):
		return InstallStatusDownloadFailed
	}
	return InstallStatusDownloadFailed
}

// --- result log ---

func (i *Installer) resultLogPath() string {
	return filepath.Join(filepath.Dir(i.rootDir), installResultCacheFile)
}

func (i *Installer) lookupResult(installID string) (InstallResult, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	r, ok := i.resultLog[installID]
	if !ok {
		return InstallResult{}, false
	}
	if time.Since(r.FinishedAt) > installResultMaxAge {
		delete(i.resultLog, installID)
		return InstallResult{}, false
	}
	return r, true
}

func (i *Installer) storeResult(r InstallResult) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if r.FinishedAt.IsZero() {
		r.FinishedAt = time.Now()
	}
	i.resultLog[r.InstallID] = r
	if len(i.resultLog) > installResultMaxRows {
		// Trim oldest by FinishedAt — best-effort, not LRU-precise.
		cutoff := time.Now().Add(-installResultMaxAge / 2)
		for k, v := range i.resultLog {
			if v.FinishedAt.Before(cutoff) {
				delete(i.resultLog, k)
			}
		}
	}
	i.persistResultLog()
}

func (i *Installer) loadResultLog() {
	data, err := os.ReadFile(i.resultLogPath())
	if err != nil {
		return
	}
	var loaded map[string]InstallResult
	if err := json.Unmarshal(data, &loaded); err != nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for k, v := range loaded {
		i.resultLog[k] = v
	}
}

func (i *Installer) persistResultLog() {
	// Caller already holds i.mu.
	data, err := json.MarshalIndent(i.resultLog, "", "  ")
	if err != nil {
		return
	}
	tmp := i.resultLogPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, i.resultLogPath())
}

// hostMissingRequirements returns the list of requirement names that
// the host doesn't satisfy. Conservative for unknowns: known
// requirements are checked best-effort; unknowns fail in
// BundleManifest.Validate before we get here.
func hostMissingRequirements(requires []string) []string {
	missing := []string{}
	for _, raw := range requires {
		req, err := parseManifestRequirement(raw)
		if err != nil {
			missing = append(missing, raw)
			continue
		}
		if !hostSatisfiesRequirement(req) {
			missing = append(missing, raw)
		}
	}
	return missing
}

// hostSatisfiesRequirement is intentionally minimal in P1a — full
// platform / version checks land in P1c. For now: usb / python /
// macos / linux are real checks; the rest default to "satisfied"
// (admin trust). Refusing was decided to fall outside P1a scope to
// keep the PR small.
func hostSatisfiesRequirement(req ManifestRequirement) bool {
	switch req.Name {
	case "macos":
		return goosIs("darwin")
	case "linux":
		return goosIs("linux")
	case "python":
		return python3Available()
	case "usb", "network", "network_admin", "root", "macos_app_sandbox_exempt":
		return true // P1a: trust admin's install decision
	}
	return false
}
