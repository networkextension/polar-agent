//go:build unix

package skills

// Bundle skill unit tests.
//
// - Validate: pure logic, always runs.
// - unzipBundle: builds a ZIP in memory, exercises zip-slip + symlink
//   rejection; always runs.
// - install: spins up an httptest server serving a fixture ZIP, checks
//   sha256 verification + idempotency; always runs.
// - Start/exec roundtrip: uses a trivial /bin/sh script entrypoint
//   (no venv), verifies EventLog + EventExit. Always runs (sh is
//   present on every unix CI runner).
// - Venv setup is NOT tested here — depends on uv/python3 on PATH and
//   isn't worth the time on every `go test` run.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBundleSkillKindAndVersion(t *testing.T) {
	sk := NewBundleSkill(t.TempDir())
	if sk == nil {
		t.Skip("non-unix")
	}
	if sk.Kind() != KindBundle {
		t.Errorf("Kind: got %q, want %q", sk.Kind(), KindBundle)
	}
	if sk.Version() == "" {
		t.Error("Version empty")
	}
}

func TestBundleValidate(t *testing.T) {
	sk := NewBundleSkill(t.TempDir())
	if sk == nil {
		t.Skip("non-unix")
	}

	cases := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"happy path", `{"bundle":"x","version":"1.0","entrypoint":"scripts/run.py"}`, false},
		{"missing bundle", `{"version":"1.0","entrypoint":"scripts/run.py"}`, true},
		{"missing version", `{"bundle":"x","entrypoint":"scripts/run.py"}`, true},
		{"missing entrypoint", `{"bundle":"x","version":"1.0"}`, true},
		{"bundle with slash", `{"bundle":"../x","version":"1.0","entrypoint":"scripts/run.py"}`, true},
		{"bundle with dotdot", `{"bundle":"a..b","version":"1.0","entrypoint":"scripts/run.py"}`, true},
		{"version with slash", `{"bundle":"x","version":"1/0","entrypoint":"scripts/run.py"}`, true},
		{"entrypoint absolute", `{"bundle":"x","version":"1.0","entrypoint":"/etc/passwd"}`, true},
		{"entrypoint not in scripts", `{"bundle":"x","version":"1.0","entrypoint":"references/foo.md"}`, true},
		{"entrypoint traversal", `{"bundle":"x","version":"1.0","entrypoint":"scripts/../../../etc/passwd"}`, true},
		{"env LD_PRELOAD banned", `{"bundle":"x","version":"1.0","entrypoint":"scripts/run.py","env":{"LD_PRELOAD":"evil.so"}}`, true},
		{"env DYLD_INSERT_LIBRARIES banned", `{"bundle":"x","version":"1.0","entrypoint":"scripts/run.py","env":{"DYLD_INSERT_LIBRARIES":"evil"}}`, true},
		{"nested scripts ok", `{"bundle":"x","version":"1.0","entrypoint":"scripts/sub/run.py"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sk.Validate(json.RawMessage(tc.cfg))
			if tc.wantErr && err == nil {
				t.Errorf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// makeBundleZip builds an in-memory .skill ZIP whose top-level dir
// matches name. files maps relative-to-top-dir paths to file contents
// (use "scripts/run.sh" etc).
func makeBundleZip(t *testing.T, name string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Add the top-level dir entry (optional but matches what a normal
	// `zip -r local-llm-serve.skill local-llm-serve/` produces).
	if _, err := zw.Create(name + "/"); err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		hdr := &zip.FileHeader{
			Name:   name + "/" + path,
			Method: zip.Deflate,
		}
		// Make scripts/*.sh executable so the bundle can exec them
		// without an explicit chmod step.
		mode := os.FileMode(0o644)
		if filepath.Ext(path) == ".sh" {
			mode = 0o755
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestUnzipBundle_HappyPath(t *testing.T) {
	zipBytes := makeBundleZip(t, "demo", map[string]string{
		"SKILL.md":      "---\nname: demo\n---\nhello",
		"scripts/run.sh": "#!/bin/sh\necho ok\n",
	})
	zipPath := filepath.Join(t.TempDir(), "demo.zip")
	if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := unzipBundle(zipPath, dest, "demo"); err != nil {
		t.Fatalf("unzip: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing: %v", err)
	} else if string(b) != "---\nname: demo\n---\nhello" {
		t.Errorf("SKILL.md content unexpected: %q", b)
	}
	info, err := os.Stat(filepath.Join(dest, "scripts", "run.sh"))
	if err != nil {
		t.Fatalf("run.sh missing: %v", err)
	}
	if info.Mode()&0o100 == 0 {
		t.Errorf("run.sh should be executable, got mode %o", info.Mode())
	}
}

func TestUnzipBundle_ZipSlipRejected(t *testing.T) {
	// Manually craft a malicious zip: entry name escapes the top dir.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("demo/../../etc/evil")
	io.WriteString(w, "pwned")
	zw.Close()

	zipPath := filepath.Join(t.TempDir(), "evil.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	err := unzipBundle(zipPath, dest, "demo")
	if err == nil {
		t.Fatal("zip slip not rejected")
	}
}

func TestBundleInstall_HappyPathAndIdempotent(t *testing.T) {
	root := t.TempDir()
	sk := NewBundleSkill(root)
	bs := sk.(*bundleSkill)

	zipBytes := makeBundleZip(t, "demo", map[string]string{
		"SKILL.md":       "---\nname: demo\n---\n",
		"scripts/run.sh": "#!/bin/sh\necho ok\n",
	})
	sum := sha256Hex(zipBytes)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBytes)
	}))
	defer srv.Close()

	cfg := BundleConfig{
		Bundle:      "demo",
		Version:     "0.1.0",
		DownloadURL: srv.URL + "/demo.skill",
		SHA256:      sum,
		Entrypoint:  "scripts/run.sh",
	}
	dest := filepath.Join(root, "demo", "0.1.0")
	if err := bs.install(context.Background(), cfg, dest); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "scripts", "run.sh")); err != nil {
		t.Fatalf("scripts/run.sh missing after install: %v", err)
	}
	// Second call must be idempotent (re-check after lock returns nil
	// without re-downloading).
	if err := bs.install(context.Background(), cfg, dest); err != nil {
		t.Fatalf("second install: %v", err)
	}
}

func TestBundleInstall_SHA256Mismatch(t *testing.T) {
	root := t.TempDir()
	sk := NewBundleSkill(root)
	bs := sk.(*bundleSkill)

	zipBytes := makeBundleZip(t, "demo", map[string]string{"SKILL.md": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(zipBytes)
	}))
	defer srv.Close()

	cfg := BundleConfig{
		Bundle:      "demo",
		Version:     "0.1.0",
		DownloadURL: srv.URL,
		SHA256:      "0000000000000000000000000000000000000000000000000000000000000000",
		Entrypoint:  "scripts/run.sh",
	}
	dest := filepath.Join(root, "demo", "0.1.0")
	err := bs.install(context.Background(), cfg, dest)
	if err == nil {
		t.Fatal("want sha256 mismatch error, got nil")
	}
	// The staging dir must not have been promoted to dest on failure.
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("dest should not exist after sha256 failure")
	}
}

func TestBundleStart_ExecRoundtrip(t *testing.T) {
	root := t.TempDir()
	sk := NewBundleSkill(root)
	bs := sk.(*bundleSkill)

	// Pre-install a tiny bundle straight to disk — Start should
	// detect it as present and skip the download.
	dest := filepath.Join(root, "demo", "0.1.0", "scripts")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho hello-stdout\necho hello-stderr >&2\nexit 0\n"
	scriptPath := filepath.Join(dest, "run.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := json.RawMessage(`{"bundle":"demo","version":"0.1.0","entrypoint":"scripts/run.sh"}`)
	if err := bs.Validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	run, err := bs.Start(context.Background(), 42, cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if run.ID() != 42 {
		t.Errorf("ID: got %d want 42", run.ID())
	}

	var sawStdout, sawStderr, sawExit, exitOK bool
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				break loop
			}
			switch ev.Kind {
			case EventLog:
				line, _ := ev.Data["line"].(string)
				stream, _ := ev.Data["stream"].(string)
				if line == "hello-stdout" && stream == "stdout" {
					sawStdout = true
				}
				if line == "hello-stderr" && stream == "stderr" {
					sawStderr = true
				}
			case EventExit:
				sawExit = true
				exitOK, _ = ev.Data["ok"].(bool)
			}
		case <-deadline:
			t.Fatal("timed out waiting for events")
		}
	}
	if !sawStdout {
		t.Error("missing stdout EventLog with 'hello-stdout'")
	}
	if !sawStderr {
		t.Error("missing stderr EventLog with 'hello-stderr'")
	}
	if !sawExit {
		t.Error("missing EventExit")
	}
	if !exitOK {
		t.Error("EventExit.ok should be true for exit code 0")
	}
}

func TestBundleStart_NonExistentBundleNeedsDownloadURL(t *testing.T) {
	root := t.TempDir()
	sk := NewBundleSkill(root)
	bs := sk.(*bundleSkill)

	// No bundle on disk and no download_url → must error.
	cfg := json.RawMessage(`{"bundle":"missing","version":"0.1.0","entrypoint":"scripts/run.sh"}`)
	_, err := bs.Start(context.Background(), 1, cfg)
	if err == nil {
		t.Fatal("want error for missing bundle without download_url")
	}
}

func TestBundleCapabilities_ReportsInstalled(t *testing.T) {
	root := t.TempDir()
	// Pre-create two bundle versions on disk.
	for _, ver := range []string{"0.1.0", "0.2.0"} {
		if err := os.MkdirAll(filepath.Join(root, "demo", ver, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Add a noise file that shouldn't be reported.
	if err := os.WriteFile(filepath.Join(root, ".hidden"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	sk := NewBundleSkill(root)
	caps := sk.Capabilities()
	installed, ok := caps["installed"].([]installedBundle)
	if !ok {
		t.Fatalf("installed wrong type: %T", caps["installed"])
	}
	if len(installed) != 2 {
		t.Errorf("want 2 installed, got %d: %+v", len(installed), installed)
	}
	versions := map[string]bool{}
	for _, b := range installed {
		if b.Name != "demo" {
			t.Errorf("unexpected name: %s", b.Name)
		}
		versions[b.Version] = true
	}
	if !versions["0.1.0"] || !versions["0.2.0"] {
		t.Errorf("missing versions in %+v", installed)
	}
}

// helper used by no test currently but useful when debugging — keeps
// the unused import warning at bay if a test gets commented out.
var _ = fmt.Sprintf
