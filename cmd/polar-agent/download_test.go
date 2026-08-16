package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// rangeServer serves blob with Range support; the first `cutFirst` bytes of
// the FIRST response are sent and then the connection is dropped, forcing a
// resume.
func rangeServer(t *testing.T, blob []byte, cutFirst int, supportRange bool) (*httptest.Server, *int32) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		start := 0
		if rh := r.Header.Get("Range"); rh != "" && supportRange {
			s := strings.TrimSuffix(strings.TrimPrefix(rh, "bytes="), "-")
			start, _ = strconv.Atoi(s)
			if start >= len(blob) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(blob)-1, len(blob)))
			w.Header().Set("Content-Length", strconv.Itoa(len(blob)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
			w.WriteHeader(http.StatusOK)
		}
		if n == 1 && cutFirst > 0 && cutFirst < len(blob)-start {
			_, _ = w.Write(blob[start : start+cutFirst])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// abort the connection mid-body
			hj, ok := w.(http.Hijacker)
			if ok {
				c, _, _ := hj.Hijack()
				c.Close()
			}
			return
		}
		_, _ = w.Write(blob[start:])
	}))
	return srv, &calls
}

func TestDownloadResumable_ResumesAndVerifies(t *testing.T) {
	blob := make([]byte, 3<<20)
	_, _ = rand.Read(blob)
	sum := sha256.Sum256(blob)
	want := hex.EncodeToString(sum[:])
	srv, calls := rangeServer(t, blob, 1<<20, true)
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "img.raw")
	if err := downloadResumable(context.Background(), srv.URL+"/img", dest, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := fileSHA256(dest); got != want {
		t.Fatalf("sha mismatch after resume")
	}
	if atomic.LoadInt32(calls) < 2 {
		t.Fatalf("expected a resume request, calls=%d", *calls)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part should be gone")
	}
	// second call: cached, no request
	before := atomic.LoadInt32(calls)
	if err := downloadResumable(context.Background(), srv.URL+"/img", dest, want); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(calls) != before {
		t.Fatalf("cached download should not hit the server")
	}
}

func TestDownloadResumable_NoRangeServerRestarts(t *testing.T) {
	blob := make([]byte, 2<<20)
	_, _ = rand.Read(blob)
	sum := sha256.Sum256(blob)
	srv, _ := rangeServer(t, blob, 512<<10, false)
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "img.raw")
	if err := downloadResumable(context.Background(), srv.URL+"/img", dest, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if got, _ := fileSHA256(dest); got != hex.EncodeToString(sum[:]) {
		t.Fatal("sha mismatch")
	}
}

func TestDownloadResumable_ShaMismatch(t *testing.T) {
	blob := []byte("hello world")
	srv, _ := rangeServer(t, blob, 0, true)
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "x.bin")
	err := downloadResumable(context.Background(), srv.URL+"/x", dest, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha mismatch, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("dest must not exist after mismatch")
	}
}

func TestDownloadResumable_FileScheme(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.raw")
	blob := []byte(strings.Repeat("A", 4096))
	_ = os.WriteFile(src, blob, 0o644)
	sum := sha256.Sum256(blob)
	dest := filepath.Join(t.TempDir(), "dst.raw")
	if err := downloadResumable(context.Background(), "file://"+src, dest, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(blob) {
		t.Fatal("content mismatch")
	}
}
