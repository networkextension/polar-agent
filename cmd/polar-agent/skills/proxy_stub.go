//go:build !unix

package skills

// Windows stub: sing-box has Windows builds but we ship the Proxy
// skill as unix-only for v1. NewProxySkill returns nil and main.go
// skips registration when this stub fires.

func NewProxySkill() Skill { return nil }
