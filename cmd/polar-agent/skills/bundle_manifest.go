package skills

// bundle_manifest.go — manifest.yaml parser for .skill bundles.
//
// Spec lives in doc/bundle-format.md. The fields here intentionally
// mirror the doc 1:1 so the doc stays authoritative — when changing
// either, change both.
//
// Manifest is OPTIONAL. Bundles without manifest.yaml continue to
// work using the BundleConfig blob the dock sends with skill.start
// (legacy path). When manifest.yaml is present, fields it declares
// take priority over what the dock sent. Concretely:
//
//   - entrypoint:  manifest wins
//   - args:        manifest wins (operator overrides via BundleConfig.Args
//                  if explicitly set; resolve at install time)
//   - env:         shallow-merge, manifest provides defaults,
//                  BundleConfig.Env overrides per key
//   - runtime:     manifest only (no equivalent on BundleConfig)
//   - requires:    manifest only — agent checks at install, rejects on mismatch
//   - capabilities: manifest only — published to dock via skill.advertise

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// BundleManifest mirrors manifest.yaml. See doc/bundle-format.md §1.
type BundleManifest struct {
	Publisher   string                  `yaml:"publisher"`
	Kind        string                  `yaml:"kind"`
	Version     string                  `yaml:"version"`
	Entrypoint  string                  `yaml:"entrypoint"`
	DisplayName string                  `yaml:"display_name,omitempty"`
	Description string                  `yaml:"description,omitempty"`
	License     string                  `yaml:"license,omitempty"`
	Homepage    string                  `yaml:"homepage,omitempty"`
	Runtime     BundleManifestRuntime   `yaml:"runtime,omitempty"`
	Capabilities BundleManifestCaps     `yaml:"capabilities,omitempty"`
	Requires    []string                `yaml:"requires,omitempty"`
	Install     BundleManifestInstall   `yaml:"install,omitempty"`
}

type BundleManifestRuntime struct {
	Kind          string            `yaml:"kind,omitempty"`           // python | shell | binary; default = inferred from entrypoint extension
	PythonVersion string            `yaml:"python_version,omitempty"` // e.g. ">=3.10"
	Venv          bool              `yaml:"venv,omitempty"`
	Args          []string          `yaml:"args,omitempty"`
	Env           map[string]string `yaml:"env,omitempty"`
}

type BundleManifestCaps struct {
	Tools []BundleManifestTool `yaml:"tools,omitempty"`
}

type BundleManifestTool struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

type BundleManifestInstall struct {
	SizeMaxMB      int `yaml:"size_max_mb,omitempty"`
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
}

// ManifestRequirement is one entry in manifest.Requires. Today entries
// are simple strings (`usb`, `python: ">=3.10"`). The constraint form
// is parsed by hostSatisfiesRequirement; unknown requirement names
// cause Validate to fail closed (don't silently allow installs that
// declare requirements the agent doesn't understand).
type ManifestRequirement struct {
	Name       string
	Constraint string // optional; e.g. ">=3.10" for python
}

const bundleManifestFilename = "manifest.yaml"

// ParseBundleManifest reads + validates manifest.yaml at the given
// path. Returns nil + nil if the file doesn't exist (manifest is
// optional). Returns an error if the file exists but is malformed.
func ParseBundleManifest(path string) (*BundleManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m BundleManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest yaml: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	return &m, nil
}

// ReadBundleManifestFromDir is a convenience wrapper around
// ParseBundleManifest. Returns nil + nil when no manifest is present.
func ReadBundleManifestFromDir(bundleDir string) (*BundleManifest, error) {
	return ParseBundleManifest(filepath.Join(bundleDir, bundleManifestFilename))
}

// Validate enforces the spec's required fields + light syntactic
// checks. Network-side validation (publisher key, sha256 match) is
// not this function's job — those run earlier in the install pipeline.
func (m *BundleManifest) Validate() error {
	if strings.TrimSpace(m.Publisher) == "" {
		return errors.New("publisher is required")
	}
	if !isValidPublisher(m.Publisher) {
		return fmt.Errorf("publisher %q: expected reverse-domain form like com.example.foo", m.Publisher)
	}
	if strings.TrimSpace(m.Kind) == "" {
		return errors.New("kind is required")
	}
	if !isValidSkillKindString(m.Kind) {
		return fmt.Errorf("kind %q: lowercase letters, digits, dots, hyphens only", m.Kind)
	}
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("version is required")
	}
	if strings.TrimSpace(m.Entrypoint) == "" {
		return errors.New("entrypoint is required")
	}
	// Entrypoint must be a relative path. Reject path traversal so
	// a malicious manifest can't escape the bundle dir on extract.
	if filepath.IsAbs(m.Entrypoint) || strings.Contains(m.Entrypoint, "..") {
		return fmt.Errorf("entrypoint %q: must be relative + cannot contain ..", m.Entrypoint)
	}
	switch m.Runtime.Kind {
	case "", "python", "shell", "binary":
		// ok
	default:
		return fmt.Errorf("runtime.kind %q: must be python | shell | binary", m.Runtime.Kind)
	}
	// Tool names must be unique within capabilities — dock UI uses
	// the name as a stable handle for tool dispatch.
	seen := map[string]bool{}
	for _, t := range m.Capabilities.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return errors.New("capabilities.tools entry: name required")
		}
		if seen[t.Name] {
			return fmt.Errorf("capabilities.tools: duplicate name %q", t.Name)
		}
		seen[t.Name] = true
	}
	// Requirements: parse + reject unknown names so we don't silently
	// allow a bundle that needs a permission the agent has no idea
	// how to grant.
	for _, raw := range m.Requires {
		req, err := parseManifestRequirement(raw)
		if err != nil {
			return fmt.Errorf("requires: %w", err)
		}
		if !knownRequirements[req.Name] {
			return fmt.Errorf("requires: unknown requirement %q (known: %s)",
				req.Name, knownRequirementsList())
		}
	}
	if m.Install.SizeMaxMB < 0 {
		return errors.New("install.size_max_mb must be >= 0")
	}
	if m.Install.TimeoutSeconds < 0 {
		return errors.New("install.timeout_seconds must be >= 0")
	}
	return nil
}

// MergeOntoConfig applies manifest values to a BundleConfig in-place.
// Precedence:
//   - entrypoint: manifest always wins (canonical source of truth)
//   - args: BundleConfig wins if non-empty; else manifest
//   - env: manifest provides defaults, BundleConfig overrides per key
func (m *BundleManifest) MergeOntoConfig(cfg *BundleConfig) {
	if m == nil || cfg == nil {
		return
	}
	cfg.Entrypoint = m.Entrypoint
	if len(cfg.Args) == 0 && len(m.Runtime.Args) > 0 {
		cfg.Args = append([]string{}, m.Runtime.Args...)
	}
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	for k, v := range m.Runtime.Env {
		if _, set := cfg.Env[k]; !set {
			cfg.Env[k] = v
		}
	}
}

// CapabilitiesForAdvertise returns the manifest contents serialized
// into the shape that skill.advertise expects (already JSON-friendly).
// Bundle skill's Capabilities() calls into this when advertising
// installed bundles.
func (m *BundleManifest) CapabilitiesForAdvertise() map[string]any {
	if m == nil {
		return nil
	}
	tools := make([]map[string]any, 0, len(m.Capabilities.Tools))
	for _, t := range m.Capabilities.Tools {
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
		})
	}
	out := map[string]any{
		"publisher": m.Publisher,
		"version":   m.Version,
	}
	if m.DisplayName != "" {
		out["display_name"] = m.DisplayName
	}
	if m.Description != "" {
		out["description"] = m.Description
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if len(m.Requires) > 0 {
		out["requires"] = m.Requires
	}
	return out
}

// --- requirement parsing ---

// knownRequirements enumerates every requirement string the agent
// knows how to check. Add an entry here when implementing the
// corresponding hostSatisfiesRequirement branch.
var knownRequirements = map[string]bool{
	"usb":                       true,
	"network":                   true,
	"network_admin":             true,
	"python":                    true, // followed by version constraint
	"macos":                     true,
	"linux":                     true,
	"root":                      true,
	"macos_app_sandbox_exempt":  true,
}

func knownRequirementsList() string {
	xs := make([]string, 0, len(knownRequirements))
	for k := range knownRequirements {
		xs = append(xs, k)
	}
	return strings.Join(xs, ", ")
}

func parseManifestRequirement(raw string) (ManifestRequirement, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ManifestRequirement{}, errors.New("empty requirement")
	}
	if i := strings.Index(raw, ":"); i > 0 {
		return ManifestRequirement{
			Name:       strings.TrimSpace(raw[:i]),
			Constraint: strings.TrimSpace(raw[i+1:]),
		}, nil
	}
	return ManifestRequirement{Name: raw}, nil
}

// --- light validators ---

func isValidPublisher(s string) bool {
	if !strings.Contains(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

func isValidSkillKindString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}
