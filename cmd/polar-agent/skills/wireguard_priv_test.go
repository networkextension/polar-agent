//go:build unix

package skills

// Unit tests for privilege detection. cmdProber is injected so we
// exercise every branch without touching a real wg-quick binary or
// requiring sudoers configuration on the test host. The real probe
// helper just shells out via exec.Command — already covered by the
// `go run ./cmd/polar-agent ...` smoke path; not duplicated here.

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// probeBuilder constructs a cmdProber that returns nil for matching
// commands and a fake "permission denied" error otherwise. matchers
// is a list of name + first-arg pairs; "*" wildcards the binary path.
type probeBuilder struct {
	allow map[string]bool
}

func (b probeBuilder) probe(name string, args ...string) error {
	key := name
	if len(args) > 0 {
		key += " " + args[0]
	}
	if b.allow[key] {
		return nil
	}
	// Wildcard: any path ending in wg-quick.
	for k := range b.allow {
		if strings.HasPrefix(k, "*wg-quick ") && strings.HasSuffix(name, "wg-quick") {
			if len(args) > 0 && strings.TrimPrefix(k, "*wg-quick ") == args[0] {
				return nil
			}
		}
		if strings.HasPrefix(k, "sudo -n *wg-quick ") && name == "sudo" && len(args) >= 3 && args[0] == "-n" && strings.HasSuffix(args[1], "wg-quick") {
			if strings.TrimPrefix(k, "sudo -n *wg-quick ") == args[2] {
				return nil
			}
		}
	}
	return &exec.ExitError{} // any non-nil error stands in for "exec failed / permission denied"
}

func newTestSkillWithProbe(allow ...string) *wireguardSkill {
	b := probeBuilder{allow: map[string]bool{}}
	for _, k := range allow {
		b.allow[k] = true
	}
	return newWireGuardSkillForTest(b.probe)
}

func TestDetectWGPrivilege_NotInstalled(t *testing.T) {
	// If wg-quick isn't on PATH, detection short-circuits before
	// probing. We can't easily simulate "missing from PATH" without
	// mutating env, but we can verify the post-detection state is
	// consistent when no wg-quick anywhere: the prober shouldn't
	// even be called. Use a prober that panics on call.
	panicProbe := func(string, ...string) error {
		t.Fatal("prober called even though wg-quick lookup should short-circuit")
		return nil
	}
	// Stash any wg-quick on $PATH (CI runners may not have it; if they
	// do, just skip this case rather than fight the environment).
	if _, err := exec.LookPath("wg-quick"); err == nil {
		t.Skip("wg-quick is on PATH; skip the NotInstalled branch")
	}
	d := detectWGPrivilege(panicProbe)
	if !d.NotInstalled {
		t.Error("want NotInstalled=true when wg-quick missing")
	}
	if d.Mode != privilegeNone {
		t.Errorf("Mode: want none, got %s", d.Mode)
	}
}

func TestDetectWGPrivilege_DirectExec(t *testing.T) {
	if _, err := exec.LookPath("wg-quick"); err != nil {
		t.Skip("wg-quick not on PATH; this test needs the LookPath to succeed")
	}
	// Allow direct probe; never get to sudo. Use the wildcard match
	// since we don't know the exact resolved path on this CI runner.
	sk := newTestSkillWithProbe("*wg-quick --help")
	if sk.priv.Mode != privilegeDirect {
		t.Errorf("Mode: want direct, got %s (path=%q sudoers=%q)", sk.priv.Mode, sk.priv.WgQuickPath, sk.priv.SudoersHint)
	}
	if len(sk.priv.ExecPrefix) != 0 {
		t.Errorf("ExecPrefix: want empty for direct, got %v", sk.priv.ExecPrefix)
	}
}

func TestDetectWGPrivilege_SudoFallback(t *testing.T) {
	if _, err := exec.LookPath("wg-quick"); err != nil {
		t.Skip("wg-quick not on PATH")
	}
	// Direct fails; sudo -n succeeds. Verify prefix is set.
	sk := newTestSkillWithProbe("sudo -n *wg-quick --help")
	if sk.priv.Mode != privilegeSudo {
		t.Errorf("Mode: want sudo, got %s", sk.priv.Mode)
	}
	if len(sk.priv.ExecPrefix) != 2 || sk.priv.ExecPrefix[0] != "sudo" || sk.priv.ExecPrefix[1] != "-n" {
		t.Errorf("ExecPrefix: want [sudo -n], got %v", sk.priv.ExecPrefix)
	}
}

func TestDetectWGPrivilege_NoneProducesSudoersHint(t *testing.T) {
	if _, err := exec.LookPath("wg-quick"); err != nil {
		t.Skip("wg-quick not on PATH")
	}
	// Both probes fail.
	sk := newTestSkillWithProbe()
	if sk.priv.Mode != privilegeNone {
		t.Errorf("Mode: want none, got %s", sk.priv.Mode)
	}
	if sk.priv.SudoersHint == "" {
		t.Fatal("SudoersHint must be non-empty when Mode=none")
	}
	if !strings.Contains(sk.priv.SudoersHint, "NOPASSWD") {
		t.Errorf("SudoersHint missing NOPASSWD literal: %q", sk.priv.SudoersHint)
	}
	if !strings.Contains(sk.priv.SudoersHint, sk.priv.WgQuickPath) {
		t.Errorf("SudoersHint should pin the resolved wg-quick path %q: %q", sk.priv.WgQuickPath, sk.priv.SudoersHint)
	}
}

func TestCapabilities_ReflectsPrivilegeState(t *testing.T) {
	if _, err := exec.LookPath("wg-quick"); err != nil {
		t.Skip("wg-quick not on PATH")
	}
	// none → capabilities should include sudoers_hint, privilege_mode=none.
	sk := newTestSkillWithProbe()
	caps := sk.Capabilities()
	if caps["privilege_mode"] != "none" {
		t.Errorf("privilege_mode: %v", caps["privilege_mode"])
	}
	if _, ok := caps["sudoers_hint"]; !ok {
		t.Error("expected sudoers_hint in capabilities for mode=none")
	}

	// direct → no hint.
	sk = newTestSkillWithProbe("*wg-quick --help")
	caps = sk.Capabilities()
	if caps["privilege_mode"] != "direct" {
		t.Errorf("privilege_mode: %v", caps["privilege_mode"])
	}
	if _, ok := caps["sudoers_hint"]; ok {
		t.Error("sudoers_hint should be absent in direct mode")
	}
}

func TestValidate_RefusesWhenNoPrivilege(t *testing.T) {
	if _, err := exec.LookPath("wg-quick"); err != nil {
		t.Skip("wg-quick not on PATH")
	}
	sk := newTestSkillWithProbe()
	err := sk.Validate(nil)
	if err == nil {
		t.Fatal("Validate should fail when Mode=none")
	}
	if !strings.Contains(err.Error(), "sudoers") {
		t.Errorf("Validate err should mention sudoers, got %q", err.Error())
	}
}

func TestWgCmdArgs_DirectVsSudo(t *testing.T) {
	// Pure-function test; no environment dependency.
	direct := detectedPrivilege{Mode: privilegeDirect}
	name, args := direct.wgCmdArgs("/usr/bin/wg-quick", "up", "wg0")
	if name != "/usr/bin/wg-quick" || len(args) != 2 || args[0] != "up" || args[1] != "wg0" {
		t.Errorf("direct: got name=%q args=%v", name, args)
	}

	sudo := detectedPrivilege{Mode: privilegeSudo, ExecPrefix: []string{"sudo", "-n"}}
	name, args = sudo.wgCmdArgs("/usr/bin/wg-quick", "up", "wg0")
	if name != "sudo" {
		t.Errorf("sudo: name=%q want sudo", name)
	}
	want := []string{"-n", "/usr/bin/wg-quick", "up", "wg0"}
	if len(args) != len(want) {
		t.Fatalf("sudo: args=%v want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("sudo args[%d]=%q want %q", i, args[i], want[i])
		}
	}
}

func TestBuildSudoersHint_Format(t *testing.T) {
	got := buildSudoersHint("polar", "/usr/bin/wg-quick", "/usr/bin/wg")
	expectedSubs := []string{
		"/etc/sudoers.d/polar-wg",
		"polar",
		"NOPASSWD",
		"/usr/bin/wg-quick",
		"/usr/bin/wg",
	}
	for _, s := range expectedSubs {
		if !strings.Contains(got, s) {
			t.Errorf("sudoers hint missing %q: %s", s, got)
		}
	}

	// Empty username → fallback placeholder.
	got = buildSudoersHint("", "/usr/bin/wg-quick", "")
	if !strings.Contains(got, "<your-agent-user>") {
		t.Errorf("empty username should fall back to placeholder, got: %s", got)
	}
	if strings.Contains(got, ", ") {
		t.Errorf("no wg path: comma separator should be absent: %s", got)
	}
}

// guard: ensure the panic prober isn't triggered by some unrelated
// test mutation of PATH.
var _ = errors.New
