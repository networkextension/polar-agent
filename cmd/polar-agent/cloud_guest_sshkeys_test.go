//go:build freebsd || linux || darwin || openbsd || netbsd

package main

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA1 a@x"
	keyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB2 b@x"
	keyH = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQH1 hand@laptop" // added by the user
)

func TestMergeAuthorizedKeys(t *testing.T) {
	cur := keyA + "\n# comment\n" + keyH + "\n"
	// replace A with B; the hand-added key and the comment stay
	got := mergeAuthorizedKeys(cur, []string{keyB}, []string{keyA})
	want := "# comment\n" + keyH + "\n" + keyB + "\n"
	if got != want {
		t.Fatalf("replace:\n%s\nwant:\n%s", got, want)
	}
	// a key in both keys and remove is kept, not duplicated (match ignores the comment)
	got = mergeAuthorizedKeys(got, []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB2 other-comment"}, []string{keyB})
	if strings.Count(got, "AAAAIB2") != 1 {
		t.Fatalf("kept key duplicated or dropped:\n%s", got)
	}
	// options prefix is matched on type+blob
	got = mergeAuthorizedKeys(`from="10.0.0.0/8" `+keyA+"\n", nil, []string{keyA})
	if got != "" {
		t.Fatalf("option-prefixed key not removed: %q", got)
	}
}

func TestGuestSSHKeysWritesFile(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	// point the lookup at a temp home by using the current user and HOME override
	home := t.TempDir()
	path := filepath.Join(home, ".ssh", "authorized_keys")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(keyA+"\n"+keyH+"\n"), 0o644)
	orig := guestHomeOverride
	guestHomeOverride = home
	defer func() { guestHomeOverride = orig }()

	raw, _ := json.Marshal(map[string]any{"user": me.Username, "keys": []string{keyB}, "remove": []string{keyA}})
	out, err := guestSSHKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "AAAAIA1") || !strings.Contains(string(b), "AAAAIB2") || !strings.Contains(string(b), "hand@laptop") {
		t.Fatalf("authorized_keys = %q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf(".ssh mode = %v", fi.Mode().Perm())
	}
	if ks := out["keys"].([]string); len(ks) != 1 || ks[0] != keyB {
		t.Fatalf("out keys = %v", out["keys"])
	}
	for _, bad := range []map[string]any{{"user": "../root", "keys": []string{keyB}}, {"user": me.Username, "keys": []string{"not a key"}}} {
		raw, _ := json.Marshal(bad)
		if _, err := guestSSHKeys(raw); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
