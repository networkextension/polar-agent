package main

// download.go — resumable, sha256-verified download for large blobs (VM
// images, helper binaries). Writes dest+".part", resumes with Range on
// retry, verifies the whole file at the end, then renames into place.
// file:// URLs are copied locally (APFS clone when possible via cp -c).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	downloadMaxAttempts  = 5
	downloadProgressStep = 256 << 20
)

// downloadResumable fetches src into dest. wantSHA256 (hex, optional) is
// verified over the finished file; a mismatch removes it and errors.
// If dest exists and matches wantSHA256, nothing is downloaded.
func downloadResumable(ctx context.Context, src, dest, wantSHA256 string) error {
	wantSHA256 = strings.ToLower(strings.TrimSpace(wantSHA256))
	if wantSHA256 != "" {
		if got, err := fileSHA256(dest); err == nil && got == wantSHA256 {
			return nil
		}
	} else if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	u, err := url.Parse(src)
	if err != nil {
		return fmt.Errorf("download: bad url: %w", err)
	}
	part := dest + ".part"
	switch u.Scheme {
	case "file", "":
		if err := copyLocal(u.Path, part); err != nil {
			return err
		}
	case "http", "https":
		if err := httpDownloadWithResume(ctx, src, part); err != nil {
			return err
		}
	default:
		return fmt.Errorf("download: unsupported scheme %q", u.Scheme)
	}
	if wantSHA256 != "" {
		got, err := fileSHA256(part)
		if err != nil {
			return err
		}
		if got != wantSHA256 {
			_ = os.Remove(part)
			return fmt.Errorf("download: sha256 mismatch: got %s want %s", got, wantSHA256)
		}
	}
	return os.Rename(part, dest)
}

func copyLocal(srcPath, dest string) error {
	_ = os.Remove(dest)
	// APFS clone first (instant, shares blocks); fall back to a stream copy.
	if out, err := exec.Command("cp", "-c", srcPath, dest).CombinedOutput(); err == nil {
		return nil
	} else {
		log.Printf("[download] cp -c failed (%s); copying", strings.TrimSpace(string(out)))
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func httpDownloadWithResume(ctx context.Context, src, part string) error {
	var lastErr error
	client := &http.Client{} // no overall timeout: multi-GB pulls; ctx bounds it
	for attempt := 1; attempt <= downloadMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var have int64
		if st, err := os.Stat(part); err == nil {
			have = st.Size()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return err
		}
		if have > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			lastErr = consumeDownloadResponse(resp, part, have)
			resp.Body.Close()
			if lastErr == nil {
				return nil
			}
			if errors.Is(lastErr, errDownloadFatal) {
				return lastErr
			}
		}
		log.Printf("[download] attempt %d/%d failed: %v", attempt, downloadMaxAttempts, lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt*attempt) * time.Second):
		}
	}
	return fmt.Errorf("download: giving up after %d attempts: %w", downloadMaxAttempts, lastErr)
}

var errDownloadFatal = errors.New("download: fatal")

// consumeDownloadResponse appends (206) or rewrites (200) the part file.
func consumeDownloadResponse(resp *http.Response, part string, have int64) error {
	var f *os.File
	var err error
	switch resp.StatusCode {
	case http.StatusPartialContent:
		f, err = os.OpenFile(part, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	case http.StatusOK:
		if have > 0 {
			log.Printf("[download] server ignored Range (200); restarting from 0")
		}
		f, err = os.Create(part)
		have = 0
	case http.StatusRequestedRangeNotSatisfiable:
		// Already have everything the server thinks exists.
		return nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("%w: http %d: %s", errDownloadFatal, resp.StatusCode, truncateForErr(string(body)))
		}
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncateForErr(string(body)))
	}
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	written := have
	nextMark := (written/downloadProgressStep + 1) * downloadProgressStep
	total := int64(-1)
	if resp.ContentLength > 0 {
		total = have + resp.ContentLength
	}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if written >= nextMark {
				if total > 0 {
					log.Printf("[download] %d/%d MiB", written>>20, total>>20)
				} else {
					log.Printf("[download] %d MiB", written>>20)
				}
				nextMark += downloadProgressStep
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if total > 0 && written < total {
		return fmt.Errorf("short body: %d/%d", written, total)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
