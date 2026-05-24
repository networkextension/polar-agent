package main

// Background watcher for iOS devices in DFU/recovery mode.
//
// Goal: zero operator config. Operator already runs polar-agent on a
// Mac; plug in an iPhone/iPad in recovery; ≤5s later the device shows
// up in /api/library/devices on dock. No manual host_skill enable.
//
// How:
//   - Probe for `irecovery` (libimobiledevice's CLI) on PATH at start
//   - If absent: log + skip silently (don't fail agent boot — the
//     watcher is opt-in by environment, not declared)
//   - If present: every 5s, run `irecovery -q`. Parse ECID/CPID/...
//     When the state changes (device plugged in / unplugged / swapped),
//     POST to /api/agent/library/devices/upsert
//
// Idempotency:
//   - sync.Once guards startRecoveryWatcher so multiple `polar-agent
//     attach` processes in the same OS user don't multi-watch the same
//     irecovery
//   - The dock endpoint upserts by ECID; duplicate POSTs are harmless

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	recoveryPollInterval = 5 * time.Second
	recoveryHTTPTimeout  = 10 * time.Second
)

var recoveryStartOnce sync.Once

// startRecoveryWatcher spawns the polling goroutine if irecovery is
// available. Safe to call multiple times; only the first wins.
// Disabled by env POLAR_AGENT_RECOVERY_DISABLED=true.
func startRecoveryWatcher(cfg AgentConfig) {
	recoveryStartOnce.Do(func() {
		if !haveIrecovery() {
			log.Printf("recovery-watcher: irecovery not on PATH, skipping (install libimobiledevice to enable)")
			return
		}
		log.Printf("recovery-watcher: started (poll every %v)", recoveryPollInterval)
		go recoveryLoop(context.Background(), cfg)
	})
}

func haveIrecovery() bool {
	_, err := exec.LookPath("irecovery")
	return err == nil
}

type recoveryDeviceInfo struct {
	ECID int64
	CPID int
	BDID int
	CPRV int
	SRTG string // iBoot version like "iBoot-7459.40.10"
	SRNM string // serial number
	IMEI string
}

func (d recoveryDeviceInfo) isPresent() bool { return d.ECID != 0 || d.CPID != 0 }

func (d recoveryDeviceInfo) equals(other recoveryDeviceInfo) bool {
	return d.ECID == other.ECID && d.CPID == other.CPID && d.BDID == other.BDID
}

func recoveryLoop(ctx context.Context, cfg AgentConfig) {
	var prev recoveryDeviceInfo
	ticker := time.NewTicker(recoveryPollInterval)
	defer ticker.Stop()
	for {
		cur, ok := pollIrecovery()
		if !ok {
			// No device or irecovery error — treat as "absent". If
			// previously present, just log the transition; we don't
			// have a "device left" endpoint yet (would be a future
			// last_seen_at refresh, but absence already lapses it).
			if prev.isPresent() {
				log.Printf("recovery-watcher: device ECID=%#x gone", prev.ECID)
				prev = recoveryDeviceInfo{}
			}
		} else if !cur.equals(prev) {
			log.Printf("recovery-watcher: device detected ECID=%#x CPID=%#x BDID=%#x SRTG=%q", cur.ECID, cur.CPID, cur.BDID, cur.SRTG)
			if err := postRecoveryDevice(cfg, cur); err != nil {
				log.Printf("recovery-watcher: post failed: %v", err)
			} else {
				prev = cur
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollIrecovery runs `irecovery -q` and parses the key=value-ish
// output. Returns (info, true) when a device is present, (zero, false)
// otherwise.
//
// Sample output (newer libimobiledevice):
//
//	CPID: 0x8101
//	CPRV: 0x11
//	BDID: 0x0A
//	ECID: 0xABCD1234567890
//	SRTG: [iBoot-7459.40.10]
//	SRNM: [F2LXX12345]
//	IMEI: [123456789012345]
func pollIrecovery() (recoveryDeviceInfo, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "irecovery", "-q").Output()
	if err != nil {
		return recoveryDeviceInfo{}, false
	}
	return parseIrecoveryOutput(string(out)), true
}

func parseIrecoveryOutput(s string) recoveryDeviceInfo {
	var d recoveryDeviceInfo
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Bracketed values like "[iBoot-7459.40.10]" — strip the brackets.
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			v = strings.TrimPrefix(v, "[")
			v = strings.TrimSuffix(v, "]")
		}
		switch k {
		case "ECID":
			d.ECID = parseMaybeHex64(v)
		case "CPID":
			d.CPID = int(parseMaybeHex64(v))
		case "BDID":
			d.BDID = int(parseMaybeHex64(v))
		case "CPRV":
			d.CPRV = int(parseMaybeHex64(v))
		case "SRTG":
			d.SRTG = v
		case "SRNM":
			d.SRNM = v
		case "IMEI":
			d.IMEI = v
		}
	}
	return d
}

func parseMaybeHex64(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		n, _ := strconv.ParseInt(s[2:], 16, 64)
		return n
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// postRecoveryDevice POSTs the freshly-detected device to dock. Server
// upserts by ECID (idempotent), so a temporary network blip is harmless
// — next poll re-posts.
func postRecoveryDevice(cfg AgentConfig, d recoveryDeviceInfo) error {
	if cfg.Server == "" || cfg.Token == "" {
		return fmt.Errorf("agent config has no server/token")
	}
	payload := map[string]any{
		"ecid":       d.ECID,
		"cpid":       d.CPID,
		"bdid":       d.BDID,
		"cprv":       d.CPRV,
		"os_running": "recovery",
		"metadata_json": map[string]any{
			"srtg":      d.SRTG,
			"srnm":      d.SRNM,
			"imei":      d.IMEI,
			"source":    "polar-agent recovery-watcher",
			"seen_at":   time.Now().UTC().Format(time.RFC3339),
		},
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(cfg.Server, "/") + "/api/agent/library/devices/upsert"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: recoveryHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("upsert status %d", resp.StatusCode)
	}
	return nil
}
