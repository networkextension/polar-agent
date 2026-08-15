//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake polar-vmd: echoes its args as JSON so we can assert the CLI contract.
const fakeVMD = `#!/bin/sh
printf '{"ok":true,"fake":true,"args":"%s"}\n' "$*"
if [ "$1" = "create" ]; then
  # emulate polar-vmd writing config.json with a MAC into --dir
  d=""; while [ $# -gt 0 ]; do [ "$1" = "--dir" ] && d="$2"; shift; done
  mkdir -p "$d"; echo '{"mac":"0a:1b:2c:3d:4e:5f"}' > "$d/config.json"
fi
`

func setupFakeVMD(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("POLAR_AGENT_HOME", home)
	bin := filepath.Join(home, "bin", "polar-vmd")
	_ = os.MkdirAll(filepath.Dir(bin), 0o755)
	if err := os.WriteFile(bin, []byte(fakeVMD), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POLAR_VMD_BIN", bin)
	return home
}

func TestCloudVM_CreateBuildsArgsAndSeed(t *testing.T) {
	home := setupFakeVMD(t)
	img := filepath.Join(t.TempDir(), "base.raw")
	_ = os.WriteFile(img, []byte("IMG"), 0o644)
	sha, _ := fileSHA256(img)
	in := map[string]any{
		"op": "create", "vm_id": "vm-1",
		"image":     map[string]any{"url": "file://" + img, "sha256": sha},
		"cpus":      2, "mem_gib": 3, "disk_size": "8G",
		"seed":      map[string]string{"POLAR_SERVER": "https://x", "POLAR_ENROLL_TOKEN": "t"},
	}
	raw, _ := json.Marshal(in)
	out, err := runCloudVMTask(context.Background(), AgentConfig{}, computeTask{ID: "ct", Skill: "cloud.vm", Input: raw})
	if err != nil {
		t.Fatalf("create: %v (%v)", err, out)
	}
	m := out.(map[string]any)
	if m["dir"] != filepath.Join(home, "vms", "vm-1") {
		t.Fatalf("dir = %v", m["dir"])
	}
	// image cached under images/<sha>.raw
	if _, err := os.Stat(filepath.Join(home, "images", sha+".raw")); err != nil {
		t.Fatalf("image not cached: %v", err)
	}
	// last vmd call was `run --dir ... --detach` (create falls through to start)
	vmd := m["vmd"].(map[string]any)
	args, _ := vmd["args"].(string)
	if !strings.HasPrefix(args, "run --dir ") || !strings.Contains(args, "--detach") {
		t.Fatalf("expected run --detach, got %q", args)
	}
	if m["mac"] != "0a:1b:2c:3d:4e:5f" {
		t.Fatalf("mac not read from config.json: %v", m["mac"])
	}
}

func TestCloudVM_BadOpAndID(t *testing.T) {
	setupFakeVMD(t)
	raw, _ := json.Marshal(map[string]any{"op": "explode", "vm_id": "ok-1"})
	if _, err := runCloudVMTask(context.Background(), AgentConfig{}, computeTask{Input: raw}); err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("want unknown op, got %v", err)
	}
	raw, _ = json.Marshal(map[string]any{"op": "status", "vm_id": "../etc"})
	if _, err := runCloudVMTask(context.Background(), AgentConfig{}, computeTask{Input: raw}); err == nil || !strings.Contains(err.Error(), "bad vm_id") {
		t.Fatalf("want bad vm_id, got %v", err)
	}
}

func TestSeedDirRendering(t *testing.T) {
	dir, err := writeSeedDir("vm-x", map[string]string{"B": "2", "A": "1"}, map[string]string{"polar/extra.txt": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	env, _ := os.ReadFile(filepath.Join(dir, "polar", "agent.env"))
	if string(env) != "A=1\nB=2\n" {
		t.Fatalf("env = %q", env)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "polar", "extra.txt")); string(b) != "hi" {
		t.Fatal("extra file missing")
	}
	if err := writeSeedFile(dir, "../escape", "x"); err == nil {
		t.Fatal("path traversal must fail")
	}
}

func TestNormalizeMAC(t *testing.T) {
	if got := normalizeMAC("0a:1b:0c:3d:00:5f"); got != "a:1b:c:3d:0:5f" {
		t.Fatalf("got %s", got)
	}
}
