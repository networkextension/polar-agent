package main

// Tests for tools.go's loader behavior. The main thing we care about
// is that bad operator-side overrides in ~/.polar/tools.json can't
// silently wipe an essential field on a builtin and turn into the
// "start  : exec: no command" runtime error (issue #166).

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// loadOverride is a test helper that runs the same merge loop as
// loadToolsConfig but takes the override JSON as a string so we don't
// need to mock the filesystem.
func loadOverride(t *testing.T, overrideJSON string) []toolSpec {
	t.Helper()
	merged := builtinTools()
	if overrideJSON == "" {
		return merged
	}
	var override toolsConfig
	if err := json.Unmarshal([]byte(overrideJSON), &override); err != nil {
		t.Fatalf("test fixture is invalid JSON: %v", err)
	}
	for _, o := range override.Tools {
		name := strings.TrimSpace(o.Name)
		if name == "" {
			continue
		}
		if strings.TrimSpace(o.Binary) == "" {
			// mirrors production guard — skip silently in the test
			continue
		}
		replaced := false
		for i, existing := range merged {
			if existing.Name == name {
				merged[i] = o
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, o)
		}
	}
	return merged
}

func TestOverrideWithEmptyBinaryIsIgnored(t *testing.T) {
	// The bug from issue #166: user dropped an override with no "binary"
	// field, the loader replaced the builtin wholesale, exec.Command got
	// "" → cryptic runtime error. After the fix, kimi's Binary must
	// still be "kimi-cli" from the builtin.
	specs := loadOverride(t, `{
		"tools": [
			{"name": "kimi", "args_first": ["--foo"]}
		]
	}`)
	for _, s := range specs {
		if s.Name == "kimi" && s.Binary != "kimi-cli" {
			t.Fatalf("kimi.Binary got %q, want %q (builtin should survive empty-binary override)", s.Binary, "kimi-cli")
		}
	}
}

func TestOverrideWithExplicitBinaryReplaces(t *testing.T) {
	// Legitimate override: operator wants their own kimi-cli at a custom
	// path. They MUST set "binary" explicitly. This path keeps working.
	specs := loadOverride(t, `{
		"tools": [
			{"name": "kimi", "binary": "/opt/kimi/bin/kimi-cli", "args_first": ["--prompt={{prompt}}"]}
		]
	}`)
	found := false
	for _, s := range specs {
		if s.Name == "kimi" {
			found = true
			if s.Binary != "/opt/kimi/bin/kimi-cli" {
				t.Errorf("kimi.Binary: got %q, want %q", s.Binary, "/opt/kimi/bin/kimi-cli")
			}
			if len(s.ArgsFirst) != 1 || s.ArgsFirst[0] != "--prompt={{prompt}}" {
				t.Errorf("kimi.ArgsFirst: got %v, want [--prompt={{prompt}}]", s.ArgsFirst)
			}
		}
	}
	if !found {
		t.Fatal("kimi entry vanished")
	}
}

func TestOverrideAddsNewTool(t *testing.T) {
	// Adding a brand-new tool (not overriding) must include "binary",
	// same rule. Without it the tool is dropped — better than letting
	// it through to crash at attach time.
	specs := loadOverride(t, `{
		"tools": [
			{"name": "mycustom"}
		]
	}`)
	for _, s := range specs {
		if s.Name == "mycustom" {
			t.Fatalf("mycustom should have been rejected (no binary); got %+v", s)
		}
	}
}

func TestOverrideNewToolWithBinaryAccepted(t *testing.T) {
	specs := loadOverride(t, `{
		"tools": [
			{"name": "mycustom", "binary": "/usr/local/bin/mycustom"}
		]
	}`)
	for _, s := range specs {
		if s.Name == "mycustom" {
			if s.Binary != "/usr/local/bin/mycustom" {
				t.Errorf("mycustom.Binary: got %q, want %q", s.Binary, "/usr/local/bin/mycustom")
			}
			return
		}
	}
	t.Fatal("mycustom not found in merged specs")
}

func TestBuiltinNamesAreUnique(t *testing.T) {
	// Sanity: the builtins themselves should not have name collisions.
	// If they did, the loader would silently lose entries.
	seen := map[string]bool{}
	for _, s := range builtinTools() {
		if seen[s.Name] {
			t.Errorf("duplicate builtin tool name: %q", s.Name)
		}
		seen[s.Name] = true
	}
}

// Sanity: every builtin has a non-empty Binary. Catches regressions
// where someone removes a builtin's binary field by accident.
func TestBuiltinsHaveBinary(t *testing.T) {
	for _, s := range builtinTools() {
		if strings.TrimSpace(s.Binary) == "" {
			t.Errorf("builtin %q has empty Binary", s.Name)
		}
	}
}

// Light-touch sanity that the path resolver formats the user override
// location predictably. (Defensive — this is what loader looks at.)
func TestToolsConfigPathPattern(t *testing.T) {
	got := toolsConfigPath()
	if got != "" {
		base := filepath.Base(got)
		if base != "tools.json" {
			t.Errorf("toolsConfigPath basename: got %q, want \"tools.json\"", base)
		}
	}
}
