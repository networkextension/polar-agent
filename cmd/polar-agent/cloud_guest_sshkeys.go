//go:build freebsd || linux || darwin || openbsd || netbsd

package main

// cloud_guest_sshkeys.go — cloud.guest op `ssh_keys`: polar-cloud edits the
// login user's authorized_keys after create (the seed disk only covers first
// boot).
//
// input: {"op":"ssh_keys","user":"polar","keys":["ssh-ed25519 AAAA… name"],"remove":["…"]}
// Lines whose key (type + base64, comment ignored) is in `remove` are dropped,
// `keys` not yet present are appended; any other line — keys the user added by
// hand — is kept as is. output adds {"user","path","keys"}.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// sshKeyID: "<type> <base64>" of an authorized_keys line, "" for blanks/comments.
// Lines with options (from="…" ssh-ed25519 …) are matched on the type+blob pair.
func sshKeyID(line string) string {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) == 0 || strings.HasPrefix(f[0], "#") {
		return ""
	}
	for i := 0; i+1 < len(f); i++ {
		if strings.HasPrefix(f[i], "ssh-") || strings.HasPrefix(f[i], "ecdsa-") || strings.HasPrefix(f[i], "sk-") {
			return f[i] + " " + f[i+1]
		}
	}
	return ""
}

// mergeAuthorizedKeys returns the new authorized_keys content.
func mergeAuthorizedKeys(cur string, keys, remove []string) string {
	drop := map[string]bool{}
	for _, k := range remove {
		if id := sshKeyID(k); id != "" {
			drop[id] = true
		}
	}
	want := map[string]bool{}
	for _, k := range keys {
		if id := sshKeyID(k); id != "" {
			want[id] = true
			delete(drop, id) // still wanted → keep
		}
	}
	var out []string
	have := map[string]bool{}
	for _, l := range strings.Split(cur, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		id := sshKeyID(l)
		if id != "" && drop[id] {
			continue
		}
		if id != "" {
			have[id] = true
		}
		out = append(out, l)
	}
	for _, k := range keys {
		id := sshKeyID(k)
		if id == "" || have[id] {
			continue
		}
		have[id] = true
		out = append(out, strings.TrimSpace(k))
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// guestHomeOverride replaces the looked-up home (tests only).
var guestHomeOverride string

// guestUserHome: home + uid/gid of the login user. macOS users made with dscl
// are not in /etc/passwd (the pure-Go os/user fallback) → /Users/<name>, ids
// from the directory's owner.
func guestUserHome(name string) (home string, uid, gid int, err error) {
	if u, e := user.Lookup(name); e == nil && u.HomeDir != "" {
		uid, _ = strconv.Atoi(u.Uid)
		gid, _ = strconv.Atoi(u.Gid)
		if guestHomeOverride != "" {
			return guestHomeOverride, uid, gid, nil
		}
		return u.HomeDir, uid, gid, nil
	}
	home = filepath.Join("/Users", name)
	fi, e := os.Stat(home)
	if e != nil {
		return "", 0, 0, fmt.Errorf("user %q: not found", name)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", 0, 0, fmt.Errorf("user %q: cannot stat %s", name, home)
	}
	return home, int(st.Uid), int(st.Gid), nil
}

func guestSSHKeys(raw json.RawMessage) (map[string]any, error) {
	var in struct {
		User   string   `json:"user"`
		Keys   []string `json:"keys"`
		Remove []string `json:"remove"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("cloud.guest ssh_keys: bad input: %w", err)
	}
	if in.User == "" || strings.ContainsAny(in.User, "/ \t\n") || in.User == "." || in.User == ".." {
		return nil, errors.New("cloud.guest ssh_keys: bad user")
	}
	for _, k := range in.Keys {
		if sshKeyID(k) == "" || strings.ContainsAny(k, "\r\n") {
			return nil, fmt.Errorf("cloud.guest ssh_keys: bad key %q", k)
		}
	}
	home, uid, gid, err := guestUserHome(in.User)
	if err != nil {
		return nil, fmt.Errorf("cloud.guest ssh_keys: %w", err)
	}
	dir := filepath.Join(home, ".ssh")
	path := filepath.Join(dir, "authorized_keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700)
	_ = os.Lchown(dir, uid, gid)
	cur, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	tmp := path + ".polar-tmp"
	if err := os.WriteFile(tmp, []byte(mergeAuthorizedKeys(string(cur), in.Keys, in.Remove)), 0o600); err != nil {
		return nil, err
	}
	_ = os.Lchown(tmp, uid, gid)
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	keys := in.Keys
	if keys == nil {
		keys = []string{}
	}
	return map[string]any{"user": in.User, "path": path, "keys": keys}, nil
}
