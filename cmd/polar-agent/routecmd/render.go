package routecmd

import "fmt"

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
