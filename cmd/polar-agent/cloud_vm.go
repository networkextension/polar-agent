//go:build darwin

package main

// cloud_vm.go — polar-cloud worker side: compute-task skill `cloud.vm`.
// Drives one polar-vmd (Swift, Virtualization.framework, one process per VM)
// per VM directory under ~/.polar/vms/<vm_id>. See polar-cloud/doc/p1-freebsd-vm.md §2.2.
//
// input:
//   {"op":"create|start|stop|kill|status|destroy", "vm_id":"...",
//    "image":{"url":"https://…|file:///…","sha256":"…","size_bytes":N},
//    "cpus":4, "mem_gib":4, "disk_size":"8G",
//    "seed":{"POLAR_SERVER":"…","POLAR_ENROLL_TOKEN":"…","POLAR_AGENT_NAME":"vm-x"},   → polar/agent.env
//    "seed_files":{"polar/extra.conf":"…"},                                          → extra files on the seed disk
//    "seed_label":"cidata",                                                          → FAT volume label (cloud-init NoCloud); default POLARSEED
//    "ready_regex":"POLAR_READY|login:",                                             → polar-vmd serial ready marker (default login:)
//    "timeout_sec":60,                                                               → stop grace
//    "vmd":{"url":"…","sha256":"…"}}                                                 → optional polar-vmd fetch
// output: {"vm_id","op","dir","mac","ip","vmd":<polar-vmd JSON>}

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type cloudVMInput struct {
	Op         string            `json:"op"`
	VMID       string            `json:"vm_id"`
	Image      cloudBlobRef      `json:"image"`
	CPUs       int               `json:"cpus"`
	MemGiB     int               `json:"mem_gib"`
	DiskSize   string            `json:"disk_size"`
	Seed       map[string]string `json:"seed"`
	SeedFiles  map[string]string `json:"seed_files"`
	SeedLabel  string            `json:"seed_label"`  // FAT label of the seed disk ("" = polar-vmd default POLARSEED; "cidata" for cloud-init)
	ReadyRegex string            `json:"ready_regex"` // polar-vmd --ready-regex ("" = default)
	Timeout    int               `json:"timeout_sec"`
	VMD        cloudBlobRef      `json:"vmd"`
}

type cloudBlobRef struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	// Kind: "" / "raw" = single raw disk file; "bundle" = macOS golden bundle
	// directory (Disk.img + AuxiliaryStorage + HardwareModel + MachineIdentifier),
	// file:// only, sha256 = sha of Disk.img.
	Kind string `json:"kind"`
}

var cloudVMIDRx = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func registerCloudVMHandler() {
	registerComputeHandler("cloud.vm", runCloudVMTask)
}

func polarHome() string {
	if h := os.Getenv("POLAR_AGENT_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".polar")
}

func cloudVMDir(vmID string) string    { return filepath.Join(polarHome(), "vms", vmID) }
func cloudImagesDir() string           { return filepath.Join(polarHome(), "images") }
func cloudImagePath(sha string) string { return filepath.Join(cloudImagesDir(), sha+".raw") }

// cloudBundlePath: cache dir for a macOS golden bundle keyed by its Disk.img sha.
func cloudBundlePath(sha string) string { return filepath.Join(cloudImagesDir(), sha+".bundle") }

// resolveVMDBinary: POLAR_VMD_BIN → ~/.polar/bin/polar-vmd → PATH → download.
func resolveVMDBinary(ctx context.Context, ref cloudBlobRef) (string, error) {
	if p := strings.TrimSpace(os.Getenv("POLAR_VMD_BIN")); p != "" {
		return p, nil
	}
	local := filepath.Join(polarHome(), "bin", "polar-vmd")
	if st, err := os.Stat(local); err == nil && st.Mode().IsRegular() {
		if ref.SHA256 == "" {
			return local, nil
		}
		if got, err := fileSHA256(local); err == nil && strings.EqualFold(got, ref.SHA256) {
			return local, nil
		}
	}
	if ref.URL == "" {
		if p, err := exec.LookPath("polar-vmd"); err == nil {
			return p, nil
		}
		return "", errors.New("polar-vmd not found (set POLAR_VMD_BIN, install ~/.polar/bin/polar-vmd, or pass input.vmd{url,sha256})")
	}
	if err := downloadResumable(ctx, ref.URL, local, ref.SHA256); err != nil {
		return "", fmt.Errorf("fetch polar-vmd: %w", err)
	}
	_ = os.Chmod(local, 0o755)
	return local, nil
}

func runCloudVMTask(ctx context.Context, cfg AgentConfig, t computeTask) (any, error) {
	var in cloudVMInput
	if err := json.Unmarshal(t.Input, &in); err != nil {
		return nil, fmt.Errorf("cloud.vm: bad input: %w", err)
	}
	in.Op = strings.ToLower(strings.TrimSpace(in.Op))
	if !cloudVMIDRx.MatchString(in.VMID) {
		return nil, fmt.Errorf("cloud.vm: bad vm_id %q", in.VMID)
	}
	dir := cloudVMDir(in.VMID)
	out := map[string]any{"vm_id": in.VMID, "op": in.Op, "dir": dir}
	vmd, err := resolveVMDBinary(ctx, in.VMD)
	if err != nil {
		return out, err
	}
	log.Printf("[cloud.vm] op=%s vm=%s", in.Op, in.VMID)
	switch in.Op {
	case "create":
		if in.Image.URL == "" {
			return out, errors.New("cloud.vm create: image.url required")
		}
		if in.Image.SHA256 == "" {
			return out, errors.New("cloud.vm create: image.sha256 required")
		}
		var img string
		if strings.EqualFold(in.Image.Kind, "bundle") {
			img = cloudBundlePath(strings.ToLower(in.Image.SHA256))
			if err := fetchBundle(ctx, in.Image.URL, img, in.Image.SHA256); err != nil {
				return out, fmt.Errorf("cloud.vm create: bundle: %w", err)
			}
		} else {
			img = cloudImagePath(strings.ToLower(in.Image.SHA256))
			if err := downloadResumable(ctx, in.Image.URL, img, in.Image.SHA256); err != nil {
				return out, fmt.Errorf("cloud.vm create: image: %w", err)
			}
		}
		args := []string{"create", "--dir", dir, "--image", img}
		if in.DiskSize != "" {
			args = append(args, "--disk-size", in.DiskSize)
		}
		if in.CPUs > 0 {
			args = append(args, "--cpus", strconv.Itoa(in.CPUs))
		}
		if in.MemGiB > 0 {
			args = append(args, "--mem-gib", strconv.Itoa(in.MemGiB))
		}
		if len(in.Seed) > 0 || len(in.SeedFiles) > 0 {
			seedDir, err := writeSeedDir(in.VMID, in.Seed, in.SeedFiles)
			if err != nil {
				return out, err
			}
			defer os.RemoveAll(seedDir)
			args = append(args, "--seed-dir", seedDir)
			if in.SeedLabel != "" {
				args = append(args, "--seed-label", in.SeedLabel)
			}
		}
		if in.ReadyRegex != "" {
			args = append(args, "--ready-regex", in.ReadyRegex)
		}
		if res, err := runVMD(ctx, vmd, 10*time.Minute, args...); err != nil {
			out["vmd"] = res
			return out, fmt.Errorf("cloud.vm create: %w", err)
		}
		fallthrough
	case "start":
		res, err := runVMD(ctx, vmd, 2*time.Minute, "run", "--dir", dir, "--detach")
		out["vmd"] = res
		if err != nil {
			return out, fmt.Errorf("cloud.vm %s: %w", in.Op, err)
		}
	case "stop":
		to := in.Timeout
		if to <= 0 {
			to = 60
		}
		res, err := runVMD(ctx, vmd, time.Duration(to+30)*time.Second, "stop", "--dir", dir, "--timeout", strconv.Itoa(to))
		out["vmd"] = res
		if err != nil {
			return out, fmt.Errorf("cloud.vm stop: %w", err)
		}
	case "kill":
		res, err := runVMD(ctx, vmd, time.Minute, "kill", "--dir", dir)
		out["vmd"] = res
		if err != nil {
			return out, fmt.Errorf("cloud.vm kill: %w", err)
		}
	case "status":
		res, err := runVMD(ctx, vmd, 30*time.Second, "status", "--dir", dir)
		out["vmd"] = res
		if err != nil {
			return out, fmt.Errorf("cloud.vm status: %w", err)
		}
	case "destroy":
		args := []string{"destroy", "--dir", dir, "--force"}
		res, err := runVMD(ctx, vmd, 2*time.Minute, args...)
		out["vmd"] = res
		if err != nil {
			return out, fmt.Errorf("cloud.vm destroy: %w", err)
		}
		return out, nil
	default:
		return out, fmt.Errorf("cloud.vm: unknown op %q", in.Op)
	}
	// Enrich with mac (config.json) + NAT lease ip when we can.
	if mac := readVMMAC(dir); mac != "" {
		out["mac"] = mac
		if ip := natLeaseIP(mac); ip != "" {
			out["ip"] = ip
		}
	}
	// qemu guests have no bridged NAT lease — report the worker-local ssh hostfwd
	// (from polar-vmd status/run output) so `guest_ip` is still a usable address.
	if _, ok := out["ip"]; !ok {
		if vmdRes, ok := out["vmd"].(map[string]any); ok {
			if fwd, _ := vmdRes["ssh_forward"].(string); fwd != "" {
				out["ip"] = fwd
			}
		}
	}
	return out, nil
}

// runVMD executes polar-vmd and parses its (single-line JSON) stdout.
func runVMD(ctx context.Context, bin string, timeout time.Duration, args ...string) (map[string]any, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := map[string]any{}
	if b := bytes.TrimSpace(stdout.Bytes()); len(b) > 0 {
		var parsed map[string]any
		if json.Unmarshal(lastJSONLine(b), &parsed) == nil {
			res = parsed
		} else {
			res["stdout"] = truncateForErr(string(b))
		}
	}
	if err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 1500 {
			tail = tail[len(tail)-1500:]
		}
		res["stderr"] = tail
		return res, fmt.Errorf("polar-vmd %s: %v: %s", strings.Join(args[:1], " "), err, tail)
	}
	return res, nil
}

func lastJSONLine(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		if len(l) > 0 && l[0] == '{' {
			return l
		}
	}
	return b
}

// writeSeedDir renders seed env + files into a temp dir for polar-vmd --seed-dir.
func writeSeedDir(vmID string, env map[string]string, files map[string]string) (string, error) {
	dir, err := os.MkdirTemp("", "polar-seed-"+vmID+"-")
	if err != nil {
		return "", err
	}
	if len(env) > 0 {
		var b strings.Builder
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(env[k])
			b.WriteString("\n")
		}
		if err := writeSeedFile(dir, "polar/agent.env", b.String()); err != nil {
			return "", err
		}
	}
	for p, content := range files {
		if err := writeSeedFile(dir, p, content); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func writeSeedFile(root, rel, content string) error {
	if rel == "" || strings.Contains(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("seed file: bad path %q", rel)
	}
	rel = filepath.Clean(rel)
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func readVMMAC(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return ""
	}
	var c struct {
		MAC string `json:"mac"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.MAC
}

// natLeaseIP resolves the VZ NAT DHCP lease for a MAC from
// /var/db/dhcpd_leases (Apple's bootpd; hw_address is written without
// leading zeros, e.g. "1,4a:13:84:ec:77:da").
func natLeaseIP(mac string) string {
	raw, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}
	want := "hw_address=1," + normalizeMAC(mac)
	var ip string
	for _, block := range strings.Split(string(raw), "}") {
		if !strings.Contains(block, want) {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "ip_address=") {
				ip = strings.TrimPrefix(line, "ip_address=")
			}
		}
		if ip != "" {
			return ip // first block wins (file is newest-first)
		}
	}
	return ""
}

func normalizeMAC(mac string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(mac)), ":")
	for i, p := range parts {
		p = strings.TrimLeft(p, "0")
		if p == "" {
			p = "0"
		}
		parts[i] = p
	}
	return strings.Join(parts, ":")
}
