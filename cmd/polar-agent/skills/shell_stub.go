//go:build !unix

package skills

// Build-tag stub: shell skill isn't supported on Windows yet (creack/pty
// has only nominal Windows support and bash -i semantics don't apply).
// We expose a NewShellSkill returning nil so main.go can call it
// unconditionally and skip registration when this stub returns nil.

// NewShellSkill returns nil on non-unix platforms — agent main()
// checks for nil and skips skills.Register.
func NewShellSkill(defaultWorkdir string) Skill { return nil }
