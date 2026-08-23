package main

// priv_exec.go — privilege prefix for the route / firewall executors.
// Root runs bare; otherwise `sudo -n` when sudo exists, else `doas -n`
// (FreeBSD/OpenBSD boxes that only ship doas — e.g. dell3000b with
// `permit nopass keepenv <user>`). Both need a NOPASSWD-style rule.

import (
	"os"
	"os/exec"
)

// privLookPath is injectable for tests.
var privLookPath = exec.LookPath

func privPrefix(argv []string) []string {
	if os.Geteuid() == 0 {
		return argv
	}
	if _, err := privLookPath("sudo"); err == nil {
		return append([]string{"sudo", "-n"}, argv...)
	}
	if _, err := privLookPath("doas"); err == nil {
		return append([]string{"doas", "-n"}, argv...)
	}
	return append([]string{"sudo", "-n"}, argv...) // neither: fail loudly via sudo's error
}
