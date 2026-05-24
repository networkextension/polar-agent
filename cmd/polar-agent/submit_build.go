package main

// `polar-agent submit-build <unsigned.ipa>` — coder calls this at
// the end of an iOS task. The orchestrator:
//
//  1. POST /api/iosdist/agent/builds/init   (multipart: project_id + ipa)
//  2. POST /api/iosdist/agent/builds/:id/start  → creds + bundle id
//  3. local sign (zsign first, codesign fallback) — see ios_sign.go
//  4. xcrun altool --upload-app …            → ASC build_id
//  5. POST /api/iosdist/agent/builds/:id/result (multipart: signed ipa + meta)
//
// Logs from sign + altool are written to `/tmp/polar-build-<task_id>/*.log`
// and the tail is sent in the result POST so dock can show "what happened"
// without us having a full log streaming system yet (Phase 2 of this PR).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func runSubmitBuild(args []string) int {
	fs := flag.NewFlagSet("submit-build", flag.ContinueOnError)
	project := fs.String("project", "", "project_id; default = basename(cwd) which matches polar-agent attach workdir_subpath convention")
	signMethod := fs.String("sign-method", "auto", "auto | zsign | codesign")
	verbose := fs.Bool("verbose", false, "log each subprocess invocation")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: polar-agent submit-build <path-to-unsigned-ipa> [--project=<id>] [--sign-method=auto|zsign|codesign]")
		return exitUsage
	}
	ipaPath := fs.Arg(0)
	if _, err := os.Stat(ipaPath); err != nil {
		fmt.Fprintf(os.Stderr, "ipa not found: %v\n", err)
		return exitConfig
	}

	cfg, err := LoadAgentConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no agent config (run `polar-agent login` first): %v\n", err)
		return exitConfig
	}
	projectID := strings.TrimSpace(*project)
	if projectID == "" {
		cwd, _ := os.Getwd()
		projectID = filepath.Base(cwd)
	}
	if projectID == "" || projectID == "." || projectID == "/" {
		fmt.Fprintln(os.Stderr, "project_id empty (cwd has no usable basename); pass --project=<id>")
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	logDir, _ := os.MkdirTemp("", "polar-build-")
	defer func() { _ = os.RemoveAll(logDir) }()

	bl := newBuildLogger(logDir)
	bl.Logf("submit-build project=%s ipa=%s sign-method=%s", projectID, ipaPath, *signMethod)

	cli := newAgentAPIClient(cfg, *verbose)

	// 1. init upload
	initResp, err := cli.initBuild(ctx, projectID, ipaPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init upload failed: %v\n", err)
		return exitNet
	}
	bl.Logf("init ok task_id=%d source_ipa_url=%s", initResp.TaskID, initResp.SourceIPAUrl)

	// 2. fetch credentials
	startResp, err := cli.startBuild(ctx, initResp.TaskID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start failed: %v\n", err)
		_ = cli.postResult(ctx, initResp.TaskID, false, "", "", "fetch creds: "+err.Error(), bl.Tail())
		return exitNet
	}
	bl.Logf("start ok bundle_id=%s asc_app_id=%s cert=%s profile=%s", startResp.TargetBundleID, startResp.ASCAppID, startResp.CertFilename, startResp.ProfileFilename)

	// download credential blobs to temp dir
	credsDir := filepath.Join(logDir, "creds")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		_ = cli.postResult(ctx, initResp.TaskID, false, "", "", "mkdir creds: "+err.Error(), bl.Tail())
		return exitConfig
	}
	certPath := filepath.Join(credsDir, "cert.p12")
	if err := downloadFile(ctx, cli.absURL(startResp.CertURL), certPath); err != nil {
		_ = cli.postResult(ctx, initResp.TaskID, false, "", "", "download cert: "+err.Error(), bl.Tail())
		return exitNet
	}
	profilePath := filepath.Join(credsDir, "profile.mobileprovision")
	if err := downloadFile(ctx, cli.absURL(startResp.ProfileURL), profilePath); err != nil {
		_ = cli.postResult(ctx, initResp.TaskID, false, "", "", "download profile: "+err.Error(), bl.Tail())
		return exitNet
	}
	p8Path := filepath.Join(credsDir, fmt.Sprintf("AuthKey_%s.p8", startResp.ASCKeyID))
	if err := os.WriteFile(p8Path, []byte(startResp.ASCP8PEM), 0o600); err != nil {
		_ = cli.postResult(ctx, initResp.TaskID, false, "", "", "write p8: "+err.Error(), bl.Tail())
		return exitConfig
	}

	// 3. sign
	signedIPA := filepath.Join(logDir, "signed.ipa")
	signRes, signErr := signIPA(ctx, signOptions{
		Method:        *signMethod,
		Unsigned:      ipaPath,
		Output:        signedIPA,
		CertP12:       certPath,
		CertPassword:  startResp.CertPassword,
		CertCommonCN:  startResp.CertCommonName,
		Profile:       profilePath,
		BundleID:      startResp.TargetBundleID,
		Logger:        bl,
		WorkspaceTemp: logDir,
	})
	if signErr != nil {
		fmt.Fprintf(os.Stderr, "sign failed: %v\n", signErr)
		_ = cli.postResult(ctx, initResp.TaskID, false, signRes.MethodUsed, "", "sign: "+signErr.Error(), bl.Tail())
		return exitToolFailed
	}
	bl.Logf("sign ok method=%s output=%s", signRes.MethodUsed, signedIPA)

	// 4. altool upload to TestFlight
	ascBuildID, upErr := uploadToTestFlight(ctx, signedIPA, startResp.ASCIssuerID, startResp.ASCKeyID, p8Path, bl)
	if upErr != nil {
		fmt.Fprintf(os.Stderr, "altool upload failed: %v\n", upErr)
		_ = cli.postResult(ctx, initResp.TaskID, false, signRes.MethodUsed, "", "altool: "+upErr.Error(), bl.Tail())
		return exitToolFailed
	}
	bl.Logf("altool ok asc_build_id=%s", ascBuildID)

	// 5. result POST (with signed_ipa as a multipart file)
	if err := cli.postResultWithFile(ctx, initResp.TaskID, true, signRes.MethodUsed, ascBuildID, "", bl.Tail(), signedIPA); err != nil {
		fmt.Fprintf(os.Stderr, "post result failed: %v\n", err)
		return exitNet
	}
	fmt.Printf("✅ build %d uploaded to TestFlight, asc_build_id=%s, sign=%s\n", initResp.TaskID, ascBuildID, signRes.MethodUsed)
	return exitOK
}

// ---- HTTP client ----

type agentAPIClient struct {
	cfg     AgentConfig
	verbose bool
	http    *http.Client
}

func newAgentAPIClient(cfg AgentConfig, verbose bool) *agentAPIClient {
	return &agentAPIClient{cfg: cfg, verbose: verbose, http: &http.Client{Timeout: 30 * time.Minute}}
}

func (c *agentAPIClient) absURL(p string) string {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	return strings.TrimRight(c.cfg.Server, "/") + p
}

type initBuildResponse struct {
	TaskID       int64  `json:"task_id"`
	SourceIPAUrl string `json:"source_ipa_url"`
}

func (c *agentAPIClient) initBuild(ctx context.Context, projectID, ipaPath string) (*initBuildResponse, error) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("project_id", projectID); err != nil {
		return nil, err
	}
	fw, err := mw.CreateFormFile("ipa", filepath.Base(ipaPath))
	if err != nil {
		return nil, err
	}
	f, err := os.Open(ipaPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := io.Copy(fw, f); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.absURL("/api/iosdist/agent/builds/init"), body)
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("init: status %d body %s", resp.StatusCode, string(b))
	}
	var out initBuildResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

type startBuildResponse struct {
	TaskID          int64  `json:"task_id"`
	TargetBundleID  string `json:"target_bundle_id"`
	ASCAppID        string `json:"asc_app_id"`
	CertURL         string `json:"cert_url"`
	CertFilename    string `json:"cert_filename"`
	CertPassword    string `json:"cert_password"`
	CertCommonName  string `json:"cert_common_name"`
	ProfileURL      string `json:"profile_url"`
	ProfileFilename string `json:"profile_filename"`
	ASCIssuerID     string `json:"asc_issuer_id"`
	ASCKeyID        string `json:"asc_key_id"`
	ASCP8PEM        string `json:"asc_p8_pem"`
}

func (c *agentAPIClient) startBuild(ctx context.Context, taskID int64) (*startBuildResponse, error) {
	url := c.absURL("/api/iosdist/agent/builds/" + strconv.FormatInt(taskID, 10) + "/start")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("start: status %d body %s", resp.StatusCode, string(b))
	}
	var out startBuildResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *agentAPIClient) postResult(ctx context.Context, taskID int64, ok bool, signMethod, ascBuildID, errMsg, logTail string) error {
	return c.postResultWithFile(ctx, taskID, ok, signMethod, ascBuildID, errMsg, logTail, "")
}

func (c *agentAPIClient) postResultWithFile(ctx context.Context, taskID int64, ok bool, signMethod, ascBuildID, errMsg, logTail, signedIPAPath string) error {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if ok {
		_ = mw.WriteField("ok", "true")
	} else {
		_ = mw.WriteField("ok", "false")
		_ = mw.WriteField("error", errMsg)
	}
	if signMethod != "" {
		_ = mw.WriteField("sign_method", signMethod)
	}
	if ascBuildID != "" {
		_ = mw.WriteField("asc_build_id", ascBuildID)
	}
	if logTail != "" {
		_ = mw.WriteField("log_tail", logTail)
	}
	if signedIPAPath != "" {
		fw, err := mw.CreateFormFile("signed_ipa", filepath.Base(signedIPAPath))
		if err != nil {
			return err
		}
		f, err := os.Open(signedIPAPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(fw, f); err != nil {
			return err
		}
	}
	_ = mw.Close()
	url := c.absURL("/api/iosdist/agent/builds/" + strconv.FormatInt(taskID, 10) + "/result")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("result: status %d body %s", resp.StatusCode, string(b))
	}
	return nil
}

// downloadFile streams an HTTP GET to a local path. Used to pull
// cert.p12 and profile.mobileprovision; both are public-URL-served
// from chatStorage today (Phase 2 may add signed URLs).
func downloadFile(ctx context.Context, url, dst string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

// uploadToTestFlight runs `xcrun altool --upload-app` against the
// signed IPA. The resulting ASC build id may not be in stdout (altool
// is async) — we capture whatever we get and return it; dock can poll
// later (M4.1 work, separate PR).
func uploadToTestFlight(ctx context.Context, signedIPA, issuer, kid, p8Path string, bl *buildLogger) (string, error) {
	// altool reads p8 from $HOME/.appstoreconnect/private_keys/AuthKey_<kid>.p8
	// or via --apiKeyDir. We use --apiKeyDir to keep the temp dir self-contained.
	apiKeyDir := filepath.Dir(p8Path)
	cmd := exec.CommandContext(ctx, "xcrun", "altool",
		"--upload-app",
		"-f", signedIPA,
		"-t", "ios",
		"--apiKey", kid,
		"--apiIssuer", issuer,
		"--apiKeyDir", apiKeyDir,
		"--output-format", "xml",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	bl.Logf("altool: %s", strings.Join(cmd.Args, " "))
	if err := cmd.Run(); err != nil {
		bl.Logf("altool stderr:\n%s", stderr.String())
		return "", fmt.Errorf("altool: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	bl.Logf("altool stdout:\n%s", stdout.String())
	// Parse build id from altool's plist output; this is best-effort.
	// altool format varies across Xcode versions; accept any UUID-ish string.
	buildID := extractALToolBuildID(stdout.String())
	return buildID, nil
}

func extractALToolBuildID(out string) string {
	// Look for "build-id" or any UUID-ish token in the output.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "build-id") || strings.Contains(line, "uuid") || strings.Contains(line, "Apple-ID") {
			// Extract from <string>UUID</string>
			start := strings.Index(line, "<string>")
			end := strings.Index(line, "</string>")
			if start >= 0 && end > start {
				return strings.TrimSpace(line[start+len("<string>") : end])
			}
		}
	}
	return ""
}

// ---- build logger (temp file + tail) ----
//
// Reserved BuildLogger interface so the next iteration can swap in
// "stream to dock" / "send to ELK" without touching sign/upload
// callers. Phase 1 implementation: write to a single text file
// under a temp dir; Tail() returns the last ~50 lines (≤4 KB) for
// the result POST.

type buildLogger struct {
	path string
	f    *os.File
}

func newBuildLogger(dir string) *buildLogger {
	path := filepath.Join(dir, "build.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		// Fall back to stderr-only logger when the file can't be
		// created — don't fail the build over a log path issue.
		return &buildLogger{path: ""}
	}
	return &buildLogger{path: path, f: f}
}

func (b *buildLogger) Logf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	if b.f != nil {
		fmt.Fprintln(b.f, line)
	}
	fmt.Fprintln(os.Stderr, line)
}

func (b *buildLogger) Tail() string {
	if b.path == "" {
		return ""
	}
	data, err := os.ReadFile(b.path)
	if err != nil {
		return ""
	}
	const maxBytes = 4096
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	return string(data)
}

var _ = errors.New // silence unused import if errors removed elsewhere
