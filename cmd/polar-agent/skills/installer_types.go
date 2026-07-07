package skills

// installer_types.go — wire-format types for skill.install /
// skill.uninstall frames. Untagged (built on every platform) because
// loop.go decodes these frames even where the unix-only Installer
// implementation is stubbed out.

import (
	"encoding/json"
	"time"
)

// InstallStatus enumerates the outcome strings expected by
// skill.install.result. Kept as constants so wire-format spelling
// can't drift between sender and receiver.
type InstallStatus string

const (
	InstallStatusOK               InstallStatus = "ok"
	InstallStatusSHAMismatch      InstallStatus = "sha_mismatch"
	InstallStatusManifestInvalid  InstallStatus = "manifest_invalid"
	InstallStatusRequiresUnmet    InstallStatus = "requires_unmet"
	InstallStatusDownloadFailed   InstallStatus = "download_failed"
	InstallStatusVenvFailed       InstallStatus = "venv_failed"
	InstallStatusDiskFull         InstallStatus = "disk_full"
	InstallStatusTimeout          InstallStatus = "timeout"
	InstallStatusActiveRuns       InstallStatus = "active_runs"   // uninstall only
	InstallStatusNotInstalled     InstallStatus = "not_installed" // uninstall only
	InstallStatusFSError          InstallStatus = "fs_error"
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
