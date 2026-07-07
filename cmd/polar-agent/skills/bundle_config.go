package skills

// BundleConfig is the skill.start config blob for KindBundle.
//
// Dock has already validated args against the bundle's declared
// schema; agent just sanity-checks the bundle/version/entrypoint
// triple is well-formed and not path-malicious.
//
// Lives in its own untagged file (bundle.go is unix-only) because
// bundle_manifest.go merges manifests onto it on every platform.
type BundleConfig struct {
	Bundle      string            `json:"bundle"`                 // e.g. "local-llm-serve"
	Version     string            `json:"version"`                // e.g. "0.1.0"
	DownloadURL string            `json:"download_url,omitempty"` // dock-hosted; omitted if already installed
	SHA256      string            `json:"sha256,omitempty"`       // verified after download
	Entrypoint  string            `json:"entrypoint"`             // e.g. "scripts/detect_hardware.py"
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}
