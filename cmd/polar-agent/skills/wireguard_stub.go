//go:build !unix

package skills

// Windows stub: wg-quick is a bash script; the WireGuard skill ships
// unix-only. NewWireGuardSkill returns nil and main.go skips
// registration.

func NewWireGuardSkill() Skill { return nil }
