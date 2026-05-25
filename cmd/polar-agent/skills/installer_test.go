//go:build unix

package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestInstaller returns an installer rooted at a temp dir.
func newTestInstaller(t *testing.T) *Installer {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bundles")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := &bundleSkill{
		rootDir:    root,
		httpClient: http.DefaultClient,
	}
	return NewInstaller(s)
}

func TestInstall_validateRequest(t *testing.T) {
	inst := newTestInstaller(t)
	cases := []struct {
		name, want string
		req        InstallRequest
	}{
		{"missing install_id", "install_id required", InstallRequest{Publisher: "com.x", SkillKind: "k", Version: "1", SHA256: strings.Repeat("a", 64), DownloadURL: "http://x"}},
		{"missing publisher", "publisher required", InstallRequest{InstallID: "i", SkillKind: "k", Version: "1", SHA256: strings.Repeat("a", 64), DownloadURL: "http://x"}},
		{"short sha", "64 hex", InstallRequest{InstallID: "i", Publisher: "com.x", SkillKind: "k", Version: "1", SHA256: "abc", DownloadURL: "http://x"}},
		{"bad kind", "rejected by name validator", InstallRequest{InstallID: "i", Publisher: "com.x", SkillKind: "../etc", Version: "1", SHA256: strings.Repeat("a", 64), DownloadURL: "http://x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := inst.Install(context.Background(), tc.req)
			if res.Status != InstallStatusManifestInvalid {
				t.Errorf("status = %v, want %v", res.Status, InstallStatusManifestInvalid)
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("err = %q, want substring %q", res.Error, tc.want)
			}
		})
	}
}

func TestInstall_downloadFailed(t *testing.T) {
	inst := newTestInstaller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 404)
	}))
	defer srv.Close()
	res := inst.Install(context.Background(), InstallRequest{
		InstallID:   "ins_1",
		Publisher:   "com.example.test",
		SkillKind:   "echo",
		Version:     "0.1.0",
		SHA256:      strings.Repeat("a", 64),
		DownloadURL: srv.URL + "/missing.skill",
	})
	if res.Status != InstallStatusDownloadFailed {
		t.Errorf("status = %v, want %v (err=%q)", res.Status, InstallStatusDownloadFailed, res.Error)
	}
}

func TestInstall_shaMismatch(t *testing.T) {
	zipBytes := makeMinimalSkillZip(t, `publisher: com.example.test
kind: echo
version: 0.1.0
entrypoint: scripts/run.py`, "scripts/run.py", "print('hi')\n")

	inst := newTestInstaller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()
	res := inst.Install(context.Background(), InstallRequest{
		InstallID:   "ins_sha",
		Publisher:   "com.example.test",
		SkillKind:   "echo",
		Version:     "0.1.0",
		SHA256:      strings.Repeat("0", 64),
		DownloadURL: srv.URL + "/x.skill",
	})
	if res.Status != InstallStatusSHAMismatch {
		t.Errorf("status = %v, want %v (err=%q)", res.Status, InstallStatusSHAMismatch, res.Error)
	}
}

func TestInstall_okThenIdempotent(t *testing.T) {
	zipBytes := makeMinimalSkillZip(t, `publisher: com.example.test
kind: echo
version: 0.1.0
entrypoint: scripts/run.py`, "scripts/run.py", "print('hi')\n")
	sha := sha256HexLocal(zipBytes)

	inst := newTestInstaller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	req := InstallRequest{
		InstallID:   "ins_ok",
		Publisher:   "com.example.test",
		SkillKind:   "echo",
		Version:     "0.1.0",
		SHA256:      sha,
		DownloadURL: srv.URL + "/x.skill",
	}
	res := inst.Install(context.Background(), req)
	if res.Status != InstallStatusOK {
		t.Fatalf("first install status = %v err=%q", res.Status, res.Error)
	}
	if res.InstalledPath == "" {
		t.Errorf("installed_path empty")
	}
	res2 := inst.Install(context.Background(), req)
	if res2.Status != InstallStatusOK {
		t.Errorf("idempotent status = %v err=%q", res2.Status, res2.Error)
	}
	if res2.InstalledPath != res.InstalledPath {
		t.Errorf("idempotent path mismatch: %q vs %q", res.InstalledPath, res2.InstalledPath)
	}
	req2 := req
	req2.InstallID = "ins_other"
	res3 := inst.Install(context.Background(), req2)
	if res3.Status != InstallStatusAlreadyInstalled {
		t.Errorf("re-install status = %v, want %v", res3.Status, InstallStatusAlreadyInstalled)
	}
}

func TestInstall_publisherMismatch(t *testing.T) {
	zipBytes := makeMinimalSkillZip(t, `publisher: com.evil.bait
kind: echo
version: 0.1.0
entrypoint: scripts/run.py`, "scripts/run.py", "print('hi')\n")
	sha := sha256HexLocal(zipBytes)

	inst := newTestInstaller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()
	res := inst.Install(context.Background(), InstallRequest{
		InstallID:   "ins_pub",
		Publisher:   "com.example.test",
		SkillKind:   "echo",
		Version:     "0.1.0",
		SHA256:      sha,
		DownloadURL: srv.URL + "/x.skill",
	})
	if res.Status != InstallStatusManifestInvalid {
		t.Errorf("status = %v, want %v err=%q", res.Status, InstallStatusManifestInvalid, res.Error)
	}
	if !strings.Contains(res.Error, "publisher mismatch") {
		t.Errorf("err = %q, want publisher mismatch", res.Error)
	}
}

func TestUninstall(t *testing.T) {
	inst := newTestInstaller(t)
	bundleDir := filepath.Join(inst.rootDir, "echo", "0.1.0")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r1 := inst.Uninstall(UninstallRequest{InstallID: "u1", SkillKind: "nope", Version: "9.9.9"}, nil)
	if r1.Status != InstallStatusNotInstalled {
		t.Errorf("nope status = %v", r1.Status)
	}
	r2 := inst.Uninstall(UninstallRequest{InstallID: "u2", SkillKind: "echo", Version: "0.1.0"}, map[int64]string{42: "echo"})
	if r2.Status != InstallStatusActiveRuns {
		t.Errorf("active status = %v err=%q", r2.Status, r2.Error)
	}
	if r2.RemovedRuns != 1 {
		t.Errorf("removed_runs = %d, want 1", r2.RemovedRuns)
	}
	if _, err := os.Stat(bundleDir); err != nil {
		t.Errorf("bundle dir gone after refused uninstall: %v", err)
	}
	r3 := inst.Uninstall(UninstallRequest{InstallID: "u3", SkillKind: "echo", Version: "0.1.0", Force: true}, map[int64]string{42: "echo"})
	if r3.Status != InstallStatusOK {
		t.Errorf("force status = %v err=%q", r3.Status, r3.Error)
	}
	if _, err := os.Stat(bundleDir); err == nil {
		t.Errorf("bundle dir still present after force uninstall")
	}
}

func TestResultLogPersistence(t *testing.T) {
	inst := newTestInstaller(t)
	r := InstallResult{InstallID: "ins_persist", Status: InstallStatusOK, InstalledPath: "/p"}
	inst.storeResult(r)
	bs := &bundleSkill{rootDir: inst.rootDir}
	inst2 := NewInstaller(bs)
	loaded, ok := inst2.lookupResult("ins_persist")
	if !ok {
		t.Fatal("expected cached result after reload")
	}
	if loaded.Status != InstallStatusOK {
		t.Errorf("status = %v", loaded.Status)
	}
}

// --- helpers ---

func makeMinimalSkillZip(t *testing.T, manifest, scriptPath, scriptBody string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	if err := addZipFile(zw, "manifest.yaml", manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := addZipFile(zw, scriptPath, scriptBody); err != nil {
		t.Fatalf("script: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func addZipFile(zw *zip.Writer, name, body string) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(body))
	return err
}

// sha256HexLocal — name-clash workaround; bundle_test.go also defines a
// sha256Hex helper. Local copy avoids cross-test coupling.
func sha256HexLocal(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
