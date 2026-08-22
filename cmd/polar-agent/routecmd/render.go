package routecmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Render turns one Op into the argv to run on `goos`. No shell is involved —
// callers exec the argv directly (prefixed with `sudo -n` when not root).
//
//	darwin/freebsd: route -n add|change|delete -inet|-inet6 DST ...
//	linux:          ip -4|-6 route replace|del ...
//	openbsd:        route -n add|change|delete -inet|-inet6 DST ...  (-reject for blackhole)
func Render(goos string, op Op) ([]string, error) {
	switch goos {
	case "darwin", "freebsd":
		return renderBSD(op, false)
	case "openbsd":
		return renderBSD(op, true)
	case "linux":
		return renderLinux(op)
	}
	return nil, fmt.Errorf("routecmd: unsupported OS %q", goos)
}

// RenderAll renders a whole op list; the first failure aborts.
func RenderAll(goos string, ops []Op) ([][]string, error) {
	out := make([][]string, 0, len(ops))
	for _, op := range ops {
		argv, err := Render(goos, op)
		if err != nil {
			return nil, err
		}
		out = append(out, argv)
	}
	return out, nil
}

func bsdFamily(r Route) string {
	if r.Family == 6 {
		return "-inet6"
	}
	return "-inet"
}

func bsdDst(r Route) string {
	if r.IsDefault() {
		return "default"
	}
	return r.Dst
}

func renderBSD(op Op, openbsd bool) ([]string, error) {
	r := op.Route
	var verb string
	switch op.Action {
	case "add":
		verb = "add"
	case "change":
		verb = "change"
	case "delete":
		verb = "delete"
	default:
		return nil, fmt.Errorf("routecmd: bad action %q", op.Action)
	}
	argv := []string{"route", "-n", verb, bsdFamily(r), bsdDst(r)}
	if verb == "delete" {
		return argv, nil
	}
	switch r.Kind {
	case KindVia:
		argv = append(argv, r.Gateway)
	case KindIface:
		argv = append(argv, "-interface", r.Iface)
	case KindBlackhole:
		loop := "127.0.0.1"
		if r.Family == 6 {
			loop = "::1"
		}
		if openbsd {
			argv = append(argv, loop, "-reject")
		} else {
			argv = append(argv, loop, "-blackhole")
		}
	default:
		return nil, fmt.Errorf("routecmd: bad kind %q", r.Kind)
	}
	return argv, nil
}

func renderLinux(op Op) ([]string, error) {
	r := op.Route
	fam := "-4"
	if r.Family == 6 {
		fam = "-6"
	}
	argv := []string{"ip", fam, "route"}
	switch op.Action {
	case "add", "change":
		argv = append(argv, "replace")
	case "delete":
		return append(argv, "del", r.Dst), nil
	default:
		return nil, fmt.Errorf("routecmd: bad action %q", op.Action)
	}
	switch r.Kind {
	case KindVia:
		argv = append(argv, r.Dst, "via", r.Gateway)
		if r.Iface != "" {
			argv = append(argv, "dev", r.Iface)
		}
	case KindIface:
		argv = append(argv, r.Dst, "dev", r.Iface)
	case KindBlackhole:
		argv = append(argv, "blackhole", r.Dst)
	default:
		return nil, fmt.Errorf("routecmd: bad kind %q", r.Kind)
	}
	if r.Metric > 0 {
		argv = append(argv, "metric", fmt.Sprint(r.Metric))
	}
	return argv, nil
}

// ListArgv is the command that dumps the kernel table for ParseTable.
func ListArgv(goos string, family int) ([]string, error) {
	switch goos {
	case "darwin", "freebsd", "openbsd":
		f := "inet"
		if family == 6 {
			f = "inet6"
		}
		return []string{"netstat", "-rn", "-f", f}, nil
	case "linux":
		f := "-4"
		if family == 6 {
			f = "-6"
		}
		return []string{"ip", f, "route", "show"}, nil
	}
	return nil, fmt.Errorf("routecmd: unsupported OS %q", goos)
}

// binCandidates — absolute fallbacks for the tools Render/ListArgv name.
// launchd-started agents have a minimal PATH (no /usr/sbin, /sbin), so
// "netstat"/"route"/"ip" must be resolved explicitly at exec time.
var binCandidates = map[string][]string{
	"netstat": {"/usr/sbin/netstat", "/usr/bin/netstat", "/bin/netstat"},
	"route":   {"/sbin/route", "/usr/sbin/route", "/bin/route"},
	"ip":      {"/sbin/ip", "/usr/sbin/ip", "/bin/ip", "/usr/bin/ip"},
}

// ResolveBin returns argv with argv[0] replaced by an absolute path when
// the bare name is not on PATH but one of the known locations exists.
// `exists` is injectable for tests (nil = os.Stat).
func ResolveBin(argv []string, exists func(string) bool) []string {
	if len(argv) == 0 || strings.Contains(argv[0], "/") {
		return argv
	}
	if _, err := exec.LookPath(argv[0]); err == nil {
		return argv
	}
	if exists == nil {
		exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	for _, c := range binCandidates[argv[0]] {
		if exists(c) {
			out := append([]string{c}, argv[1:]...)
			return out
		}
	}
	return argv
}
