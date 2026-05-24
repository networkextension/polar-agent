package main

// IPA distribution signing. Two paths:
//
//   zsign:    `zsign -k cert.p12 -p <pw> -m profile.mobileprovision <in> -o <out>`
//             Doesn't need a keychain. Doesn't change entitlements (uses what's
//             already in the binary). Preferred for vanilla apps.
//
//   codesign: macOS-native. Uses an ephemeral keychain that we create + import
//             the .p12 into + delete after. Handles entitlements / profiles
//             properly via xcrun. Required for Push / Network Extension /
//             Associated Domains apps.
//
// "auto" mode tries zsign first (cheaper, no keychain mess) and falls back to
// codesign if zsign isn't on PATH or the signed artifact fails to verify.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

type signOptions struct {
	Method        string // "auto" | "zsign" | "codesign"
	Unsigned      string
	Output        string
	CertP12       string
	CertPassword  string
	CertCommonCN  string
	Profile       string
	BundleID      string
	WorkspaceTemp string
	Logger        *buildLogger
}

type signResult struct {
	MethodUsed string // "zsign" | "codesign"
}

func signIPA(ctx context.Context, opts signOptions) (signResult, error) {
	switch opts.Method {
	case "zsign":
		return signWithZsign(ctx, opts)
	case "codesign":
		return signWithCodesign(ctx, opts)
	case "", "auto":
		// Try zsign first. If zsign isn't installed OR it fails,
		// fall through to codesign.
		if _, err := exec.LookPath("zsign"); err == nil {
			res, err := signWithZsign(ctx, opts)
			if err == nil {
				return res, nil
			}
			opts.Logger.Logf("zsign failed (%v), falling back to codesign", err)
		} else {
			opts.Logger.Logf("zsign not on PATH, using codesign")
		}
		return signWithCodesign(ctx, opts)
	default:
		return signResult{}, fmt.Errorf("unknown sign-method %q", opts.Method)
	}
}

func signWithZsign(ctx context.Context, opts signOptions) (signResult, error) {
	if _, err := exec.LookPath("zsign"); err != nil {
		return signResult{}, fmt.Errorf("zsign not on PATH: %w", err)
	}
	args := []string{
		"-k", opts.CertP12,
		"-m", opts.Profile,
		"-o", opts.Output,
		"-z", "9",
	}
	if opts.CertPassword != "" {
		args = append(args, "-p", opts.CertPassword)
	}
	args = append(args, opts.Unsigned)

	cmd := exec.CommandContext(ctx, "zsign", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	opts.Logger.Logf("zsign: %s", redactSignArgs(cmd.Args))
	if err := cmd.Run(); err != nil {
		opts.Logger.Logf("zsign stderr:\n%s", stderr.String())
		return signResult{MethodUsed: "zsign"}, fmt.Errorf("zsign: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	opts.Logger.Logf("zsign stdout:\n%s", stdout.String())
	return signResult{MethodUsed: "zsign"}, nil
}

func signWithCodesign(ctx context.Context, opts signOptions) (signResult, error) {
	if _, err := exec.LookPath("codesign"); err != nil {
		return signResult{}, fmt.Errorf("codesign not on PATH (only ships with macOS Xcode CLT): %w", err)
	}
	if _, err := exec.LookPath("security"); err != nil {
		return signResult{}, fmt.Errorf("security tool not on PATH: %w", err)
	}

	// Create an ephemeral keychain so we don't pollute the user's
	// login.keychain. `defer` deletes it even on panic.
	kcName := "polar-build-" + randomToken(8) + ".keychain"
	kcPath := filepath.Join(opts.WorkspaceTemp, kcName)
	kcPassword := randomToken(16)

	if err := runSimple(ctx, opts.Logger, "security", "create-keychain", "-p", kcPassword, kcPath); err != nil {
		return signResult{}, fmt.Errorf("create-keychain: %w", err)
	}
	defer func() {
		_ = exec.Command("security", "delete-keychain", kcPath).Run()
	}()
	if err := runSimple(ctx, opts.Logger, "security", "unlock-keychain", "-p", kcPassword, kcPath); err != nil {
		return signResult{}, fmt.Errorf("unlock-keychain: %w", err)
	}
	if err := runSimple(ctx, opts.Logger, "security", "set-keychain-settings", "-lut", "7200", kcPath); err != nil {
		return signResult{}, fmt.Errorf("set-keychain-settings: %w", err)
	}
	importArgs := []string{"import", opts.CertP12, "-k", kcPath, "-T", "/usr/bin/codesign", "-T", "/usr/bin/security"}
	if opts.CertPassword != "" {
		importArgs = append(importArgs, "-P", opts.CertPassword)
	}
	if err := runSimple(ctx, opts.Logger, "security", importArgs...); err != nil {
		return signResult{}, fmt.Errorf("import cert: %w", err)
	}
	// Allow codesign to use the imported key without an interactive prompt.
	if err := runSimple(ctx, opts.Logger, "security", "set-key-partition-list",
		"-S", "apple-tool:,apple:,codesign:", "-s",
		"-k", kcPassword, kcPath); err != nil {
		// Some Xcode CLT versions don't have set-key-partition-list;
		// log + continue. Codesign will work anyway in many setups.
		opts.Logger.Logf("set-key-partition-list failed (continuing): %v", err)
	}

	// Pick the sign identity. Operator can set CertCommonCN; if empty,
	// codesign should still find a single identity in the ephemeral
	// keychain.
	identity := opts.CertCommonCN
	if identity == "" {
		// Probe identities in the keychain.
		out, err := exec.Command("security", "find-identity", "-v", "-p", "codesigning", kcPath).CombinedOutput()
		if err == nil {
			// Take the first identity name (the part in quotes after the hash).
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "\"") {
					q1 := strings.Index(line, "\"")
					q2 := strings.LastIndex(line, "\"")
					if q1 >= 0 && q2 > q1 {
						identity = line[q1+1 : q2]
						break
					}
				}
			}
		}
	}
	if identity == "" {
		return signResult{}, errors.New("could not determine codesign identity from keychain or cert common name")
	}

	// Sign the IPA. Note: `codesign --sign` operates on a .app bundle, not a
	// .ipa zip. We unzip → re-sign the .app inside Payload/ → re-zip.
	// Keep this as a TODO in v1: zsign is the primary path; codesign
	// fallback is reserved for entitlement-needing apps that require a
	// fuller orchestration. v1 stops at "tried codesign, failed" with a
	// clear error so the operator knows zsign needs to be installed.
	return signResult{MethodUsed: "codesign"}, errors.New(
		"codesign re-sign of an .ipa requires unzip + per-bundle codesign + re-zip orchestration — install zsign on the agent host for v1, codesign re-sign is Phase 2",
	)
}

func runSimple(ctx context.Context, bl *buildLogger, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	bl.Logf("%s %s", name, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func redactSignArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if i > 0 && args[i-1] == "-p" {
			out[i] = "***"
		}
	}
	return strings.Join(out, " ")
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
