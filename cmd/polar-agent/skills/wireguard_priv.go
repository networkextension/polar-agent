//go:build unix

package skills

// Privilege detection for the WireGuard skill.
//
// wg-quick needs root for `ip link add type wireguard`, address +
// route plumbing, and /etc/wireguard writes. `wg show <iface> dump`
// also needs CAP_NET_ADMIN on Linux (and is effectively root-only on
// FreeBSD). polar-agent typically runs as an unprivileged user, so
// every exec for these has to be lifted somehow.
//
// At skill construction (process start) we probe two paths in order:
//
//   1. Direct exec: `wg-quick --help` — works iff agent is root.
//   2. sudo -n exec: `sudo -n <abs-wg-quick> --help` — works iff
//      operator has installed a NOPASSWD sudoers entry like:
//        polar  ALL=(root) NOPASSWD: /usr/bin/wg-quick, /usr/bin/wg
//
// Whichever wins gets remembered as a string prefix slice that every
// later exec.Cmd is built from. If both fail we still register the
// skill (so `skill.advertise` carries the diagnostic capabilities to
// dock) but Validate refuses subsequent skill.start calls with a
// message that includes the exact sudoers line to paste.
//
// `--help` is the probe because:
//   - wg-quick's help branch is at the very top of the script and
//     exits 0 before any privileged op runs, so it's safe to run
//     repeatedly during boot.
//   - It returns 0 deterministically on both Linux and FreeBSD
//     wireguard-tools.
//   - It doesn't touch /etc/wireguard, so it works in containers
//     without that directory.

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// cmdProber is an injection point for tests. It returns nil iff the
// given command exits 0. defaultProber wraps exec.Command + Run.
type cmdProber func(name string, args ...string) error

func defaultProber(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// privilegeMode is the user-facing label for what we detected.
type privilegeMode string

const (
	privilegeDirect privilegeMode = "direct" // agent is root (or has CAP_NET_ADMIN)
	privilegeSudo   privilegeMode = "sudo"   // NOPASSWD sudoers is configured
	privilegeNone   privilegeMode = "none"   // can't reach wg-quick under root
)

// detectedPrivilege captures the result of one probe run. Filled in
// at NewWireGuardSkill; immutable afterwards. ExecPrefix is what every
// wg-quick / wg exec.Cmd is built from (nil = no prefix needed).
type detectedPrivilege struct {
	Mode         privilegeMode
	ExecPrefix   []string // nil when Mode == privilegeDirect, ["sudo","-n"] when sudo
	WgQuickPath  string   // absolute path resolved via LookPath; "" if not installed
	WgPath       string   // absolute path of `wg`; "" if not installed
	ExecUser     string   // os/user.Current().Username; informational
	SudoersHint  string   // ready-to-paste line for the "none" case
	NotInstalled bool     // true when wg-quick itself is missing on PATH
}

// detectWGPrivilege resolves binary paths and probes for the cheapest
// working exec path. Pure function modulo `probe` — never touches
// global state.
func detectWGPrivilege(probe cmdProber) detectedPrivilege {
	d := detectedPrivilege{
		ExecUser: currentUsername(),
	}
	wgQuick, qerr := exec.LookPath("wg-quick")
	wg, _ := exec.LookPath("wg")
	d.WgQuickPath = wgQuick
	d.WgPath = wg

	if qerr != nil || wgQuick == "" {
		// Tool not installed. Skip privilege probing entirely — the
		// fix is "install wireguard-tools", not "configure sudo".
		d.NotInstalled = true
		d.Mode = privilegeNone
		return d
	}

	// 1. Direct exec — works if agent has root or CAP_NET_ADMIN.
	if probe(wgQuick, "--help") == nil {
		d.Mode = privilegeDirect
		return d
	}

	// 2. sudo -n with the absolute path. Sudoers rules match on the
	// real path, so probing with the path we resolved is the only
	// way to know whether the operator's allowlist will match at
	// run time.
	if probe("sudo", "-n", wgQuick, "--help") == nil {
		d.Mode = privilegeSudo
		d.ExecPrefix = []string{"sudo", "-n"}
		return d
	}

	// Both failed. Construct a sudoers hint pinned to the paths we
	// actually found, so the operator pastes the right thing.
	d.Mode = privilegeNone
	d.SudoersHint = buildSudoersHint(d.ExecUser, wgQuick, wg)
	return d
}

// buildSudoersHint emits the exact line the operator should drop into
// /etc/sudoers.d/polar-wg. The wg path is included when present even
// though `wg show` is the only command that needs it — keeping both
// in one file matches how operators reason about WireGuard
// permissions.
func buildSudoersHint(username, wgQuickPath, wgPath string) string {
	if username == "" {
		username = "<your-agent-user>"
	}
	paths := []string{wgQuickPath}
	if wgPath != "" {
		paths = append(paths, wgPath)
	}
	return fmt.Sprintf("/etc/sudoers.d/polar-wg:  %s ALL=(root) NOPASSWD: %s",
		username, strings.Join(paths, ", "))
}

// currentUsername returns the effective username, or "" on error. Used
// in capabilities + sudoers hint generation — informational, never
// authoritative.
func currentUsername() string {
	u, err := user.Current()
	if err != nil || u == nil {
		return ""
	}
	return u.Username
}

// wgCmd builds an exec.Cmd that respects the detected privilege
// prefix. argv[0] is the absolute binary path; the prefix (if any) is
// prepended. Caller still owns Stdout/Stderr/Context wiring.
//
// Why a helper rather than inline string concatenation: every call
// site needs the same prefix logic (Start/Stop/poll), and a slip-up
// at any one of them silently breaks unprivileged operation. One
// helper, one place to read.
func (d detectedPrivilege) wgCmdArgs(binary string, args ...string) (name string, fullArgs []string) {
	if len(d.ExecPrefix) == 0 {
		return binary, args
	}
	full := make([]string, 0, len(d.ExecPrefix)+1+len(args))
	full = append(full, d.ExecPrefix[1:]...)
	full = append(full, binary)
	full = append(full, args...)
	return d.ExecPrefix[0], full
}
