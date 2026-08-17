//go:build !unix || netbsd

package skills

// Windows/NetBSD stub (go.bug.st/serial has no NetBSD port): serial library go.bug.st/serial does support Windows
// but the KDP skill ships unix-only for v1 (KDP workflows are
// macOS-centric). NewKDPSkill returns nil + main.go skips.

func NewKDPSkill() Skill { return nil }
