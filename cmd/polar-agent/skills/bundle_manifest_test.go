package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBundleManifest_missing(t *testing.T) {
	dir := t.TempDir()
	m, err := ReadBundleManifestFromDir(dir)
	if err != nil {
		t.Fatalf("expected nil err for missing manifest, got %v", err)
	}
	if m != nil {
		t.Fatalf("expected nil manifest, got %+v", m)
	}
}

func TestParseBundleManifest_minimal(t *testing.T) {
	dir := t.TempDir()
	yaml := `
publisher: com.example.echo
kind: echo
version: 0.1.0
entrypoint: scripts/run.py
`
	mustWriteFile(t, dir, bundleManifestFilename, yaml)
	m, err := ReadBundleManifestFromDir(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil manifest")
	}
	if m.Publisher != "com.example.echo" {
		t.Errorf("publisher = %q", m.Publisher)
	}
	if m.Kind != "echo" {
		t.Errorf("kind = %q", m.Kind)
	}
	if m.Entrypoint != "scripts/run.py" {
		t.Errorf("entrypoint = %q", m.Entrypoint)
	}
}

func TestValidate_table(t *testing.T) {
	cases := []struct {
		name    string
		m       BundleManifest
		wantErr string
	}{
		{"missing publisher", BundleManifest{Kind: "x", Version: "1", Entrypoint: "scripts/r.py"}, "publisher is required"},
		{"bad publisher", BundleManifest{Publisher: "Example", Kind: "x", Version: "1", Entrypoint: "scripts/r.py"}, "expected reverse-domain"},
		{"missing kind", BundleManifest{Publisher: "com.x.y", Version: "1", Entrypoint: "scripts/r.py"}, "kind is required"},
		{"bad kind", BundleManifest{Publisher: "com.x.y", Kind: "WAT_Bad", Version: "1", Entrypoint: "scripts/r.py"}, "lowercase letters"},
		{"missing version", BundleManifest{Publisher: "com.x.y", Kind: "x", Entrypoint: "scripts/r.py"}, "version is required"},
		{"missing entrypoint", BundleManifest{Publisher: "com.x.y", Kind: "x", Version: "1"}, "entrypoint is required"},
		{"absolute entrypoint", BundleManifest{Publisher: "com.x.y", Kind: "x", Version: "1", Entrypoint: "/etc/passwd"}, "relative"},
		{"traversal entrypoint", BundleManifest{Publisher: "com.x.y", Kind: "x", Version: "1", Entrypoint: "scripts/../../../etc/passwd"}, "relative"},
		{"bad runtime kind", BundleManifest{Publisher: "com.x.y", Kind: "x", Version: "1", Entrypoint: "scripts/r.py", Runtime: BundleManifestRuntime{Kind: "java"}}, "python | shell | binary"},
		{"dup tool", BundleManifest{Publisher: "com.x.y", Kind: "x", Version: "1", Entrypoint: "scripts/r.py", Capabilities: BundleManifestCaps{Tools: []BundleManifestTool{{Name: "a"}, {Name: "a"}}}}, "duplicate name"},
		{"unknown require", BundleManifest{Publisher: "com.x.y", Kind: "x", Version: "1", Entrypoint: "scripts/r.py", Requires: []string{"quantum_entanglement"}}, "unknown requirement"},
		{"valid minimum", BundleManifest{Publisher: "com.x.y", Kind: "x", Version: "1", Entrypoint: "scripts/r.py"}, ""},
		{"valid full", BundleManifest{
			Publisher: "com.x.y", Kind: "x.y", Version: "1.2.3", Entrypoint: "scripts/r.py",
			Runtime:      BundleManifestRuntime{Kind: "python", PythonVersion: ">=3.10", Venv: true, Args: []string{"--verbose"}, Env: map[string]string{"K": "v"}},
			Capabilities: BundleManifestCaps{Tools: []BundleManifestTool{{Name: "do_thing", Description: "x"}}},
			Requires:     []string{"usb", "python: >=3.10"},
			Install:      BundleManifestInstall{SizeMaxMB: 50, TimeoutSeconds: 120},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected ok, got %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tc.wantErr)
				return
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestMergeOntoConfig(t *testing.T) {
	m := &BundleManifest{
		Publisher: "com.x.y", Kind: "x", Version: "1", Entrypoint: "scripts/manifest.py",
		Runtime: BundleManifestRuntime{
			Args: []string{"--from-manifest"},
			Env:  map[string]string{"FROM_MANIFEST": "yes", "OVERRIDE_ME": "manifest"},
		},
	}
	cfg := &BundleConfig{
		Bundle: "x", Version: "1",
		Entrypoint: "scripts/legacy.py", // should be overwritten
		Args:       nil,                   // empty → manifest wins
		Env:        map[string]string{"OVERRIDE_ME": "config", "FROM_CONFIG": "yes"},
	}
	m.MergeOntoConfig(cfg)
	if cfg.Entrypoint != "scripts/manifest.py" {
		t.Errorf("entrypoint not overridden: %q", cfg.Entrypoint)
	}
	if len(cfg.Args) != 1 || cfg.Args[0] != "--from-manifest" {
		t.Errorf("args = %v", cfg.Args)
	}
	if cfg.Env["OVERRIDE_ME"] != "config" {
		t.Errorf("OVERRIDE_ME = %q, want config (BundleConfig should win)", cfg.Env["OVERRIDE_ME"])
	}
	if cfg.Env["FROM_MANIFEST"] != "yes" {
		t.Errorf("FROM_MANIFEST missing")
	}
	if cfg.Env["FROM_CONFIG"] != "yes" {
		t.Errorf("FROM_CONFIG missing")
	}
}

func TestMergeOntoConfig_argsExplicitWins(t *testing.T) {
	m := &BundleManifest{
		Publisher: "com.x.y", Kind: "x", Version: "1", Entrypoint: "r.py",
		Runtime:   BundleManifestRuntime{Args: []string{"--from-manifest"}},
	}
	cfg := &BundleConfig{
		Bundle: "x", Version: "1",
		Entrypoint: "r.py",
		Args:       []string{"--from-config"}, // non-empty → wins
	}
	m.MergeOntoConfig(cfg)
	if len(cfg.Args) != 1 || cfg.Args[0] != "--from-config" {
		t.Errorf("args = %v, want config args to win", cfg.Args)
	}
}

func TestCapabilitiesForAdvertise(t *testing.T) {
	m := &BundleManifest{
		Publisher: "com.x.y", Kind: "x", Version: "1.2.3", Entrypoint: "r.py",
		DisplayName: "X Skill",
		Capabilities: BundleManifestCaps{
			Tools: []BundleManifestTool{{Name: "alpha", Description: "do alpha"}},
		},
		Requires: []string{"usb"},
	}
	caps := m.CapabilitiesForAdvertise()
	if caps["publisher"] != "com.x.y" {
		t.Errorf("publisher = %v", caps["publisher"])
	}
	if caps["version"] != "1.2.3" {
		t.Errorf("version = %v", caps["version"])
	}
	if caps["display_name"] != "X Skill" {
		t.Errorf("display_name = %v", caps["display_name"])
	}
	tools, ok := caps["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools shape wrong: %v", caps["tools"])
	}
	if tools[0]["name"] != "alpha" {
		t.Errorf("tool name = %v", tools[0]["name"])
	}
}

func TestParseRequirement(t *testing.T) {
	cases := []struct {
		raw, name, constraint string
	}{
		{"usb", "usb", ""},
		{"python: >=3.10", "python", ">=3.10"},
		{"  python:  ==3.11  ", "python", "==3.11"},
	}
	for _, tc := range cases {
		got, err := parseManifestRequirement(tc.raw)
		if err != nil {
			t.Errorf("%q: %v", tc.raw, err)
			continue
		}
		if got.Name != tc.name || got.Constraint != tc.constraint {
			t.Errorf("%q → %+v, want name=%q constraint=%q", tc.raw, got, tc.name, tc.constraint)
		}
	}
}

// --- helpers ---

func mustWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
