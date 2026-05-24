//go:build !unix

package skills

// Windows stub: the VNC skill ships unix-only for v1 since the primary
// target is macOS Screen Sharing on the agent host. NewVncSkill returns
// nil and main.go skips registration when this stub fires.

func NewVncSkill() Skill { return nil }
