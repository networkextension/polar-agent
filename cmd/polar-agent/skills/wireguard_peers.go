//go:build unix

package skills

// WireGuard peer monitor — parses `wg show <iface> dump` and emits
// EventMetric frames so dock can render per-peer state (last handshake,
// endpoint, transfer counters) without each WG peer phoning home.
//
// `wg show <iface> dump` output (tab-separated, identical on Linux +
// FreeBSD because both ship the same wireguard-tools userland):
//
//   line 1 (interface): <private-key>\t<public-key>\t<listen-port>\t<fwmark>
//   line N (peer):      <public-key>\t<preshared-key>\t<endpoint>\t
//                       <allowed-ips>\t<latest-handshake-unix>\t
//                       <rx-bytes>\t<tx-bytes>\t<persistent-keepalive>
//
// Tokens that conceptually mean "absent":
//   - "(none)" — preshared, endpoint, allowed_ips
//   - "off"    — persistent-keepalive
//   - "0"      — latest-handshake (= never handshook)
//
// We DELIBERATELY discard the interface's private key before it leaves
// the agent process. The metric only carries the public key + listen
// port + fwmark.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// Default poll cadence. WG handshakes happen ~every 2 min during
	// keepalive; 30s gives a smooth UI without burning CPU exec'ing
	// `wg show` constantly. Operators can tune via PollIntervalSec.
	wgDefaultPollIntervalSec = 30
	wgMinPollIntervalSec     = 5
	wgMaxPollIntervalSec     = 300
	wgPollExecTimeout        = 3 * time.Second
)

// wgIfaceStatus captures the per-interface fields from `wg show ... dump`
// line 1 — minus the private key, which we never propagate.
type wgIfaceStatus struct {
	PublicKey  string `json:"public_key"`
	ListenPort int    `json:"listen_port"`
	Fwmark     string `json:"fwmark,omitempty"` // hex string ("off" or e.g. "0xca6c"); kept verbatim
}

// wgPeerStatus is one peer row from the dump, normalized for direct
// rendering by dock UI.
type wgPeerStatus struct {
	PublicKey           string `json:"public_key"`
	HasPresharedKey     bool   `json:"has_preshared_key"`
	Endpoint            string `json:"endpoint,omitempty"`    // "host:port"; empty when "(none)"
	AllowedIPs          string `json:"allowed_ips,omitempty"` // comma-separated CIDRs; empty when "(none)"
	LatestHandshakeUnix int64  `json:"latest_handshake_unix"` // 0 = never
	HandshakeAgeSec     int64  `json:"handshake_age_sec"`     // derived; -1 when never
	BytesRx             int64  `json:"bytes_rx"`
	BytesTx             int64  `json:"bytes_tx"`
	KeepaliveSec        int    `json:"keepalive_sec"` // 0 = off
}

// parseWGShowDump turns the TSV output of `wg show <iface> dump` into
// a structured snapshot. Returns an error only on syntactic failures
// (wrong column count); a peer line with bogus numeric fields gets
// zero values for those and is otherwise still parsed.
func parseWGShowDump(out []byte, now time.Time) (wgIfaceStatus, []wgPeerStatus, error) {
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return wgIfaceStatus{}, nil, errors.New("wg show dump: empty output")
	}
	lines := strings.Split(text, "\n")

	ifaceFields := strings.Split(lines[0], "\t")
	if len(ifaceFields) < 4 {
		return wgIfaceStatus{}, nil, fmt.Errorf("wg show dump: iface line has %d cols, want 4", len(ifaceFields))
	}
	// ifaceFields[0] = private key — INTENTIONALLY DROPPED here.
	iface := wgIfaceStatus{
		PublicKey:  ifaceFields[1],
		ListenPort: atoiSafe(ifaceFields[2]),
		Fwmark:     normalizeFwmark(ifaceFields[3]),
	}

	peers := make([]wgPeerStatus, 0, len(lines)-1)
	nowUnix := now.Unix()
	for i := 1; i < len(lines); i++ {
		row := lines[i]
		if row == "" {
			continue
		}
		f := strings.Split(row, "\t")
		if len(f) < 8 {
			// Skip malformed rows rather than abort the whole sample — a
			// kernel-side oddity for one peer shouldn't kill the metric.
			continue
		}
		hs := atoi64Safe(f[4])
		age := int64(-1)
		if hs > 0 {
			age = nowUnix - hs
			if age < 0 {
				age = 0
			}
		}
		peers = append(peers, wgPeerStatus{
			PublicKey:           f[0],
			HasPresharedKey:     f[1] != "(none)" && f[1] != "",
			Endpoint:            noneToEmpty(f[2]),
			AllowedIPs:          noneToEmpty(f[3]),
			LatestHandshakeUnix: hs,
			HandshakeAgeSec:     age,
			BytesRx:             atoi64Safe(f[5]),
			BytesTx:             atoi64Safe(f[6]),
			KeepaliveSec:        parseKeepalive(f[7]),
		})
	}
	return iface, peers, nil
}

func noneToEmpty(s string) string {
	if s == "(none)" {
		return ""
	}
	return s
}

func parseKeepalive(s string) int {
	if s == "off" || s == "(none)" || s == "" {
		return 0
	}
	return atoiSafe(s)
}

func normalizeFwmark(s string) string {
	if s == "off" || s == "(none)" || s == "0" || s == "" {
		return ""
	}
	return s
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func atoi64Safe(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// runWGShowDump exec's `wg show <iface> dump` with a short timeout.
// Returns ("", err) if `wg` isn't on PATH OR if the iface isn't up
// (wg-quick failure already emitted its own log line). Caller decides
// whether to skip the sample or surface a warning.
func runWGShowDump(ctx context.Context, iface string) ([]byte, error) {
	wgBin, err := exec.LookPath("wg")
	if err != nil {
		return nil, fmt.Errorf("wg not on PATH: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, wgPollExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, wgBin, "show", iface, "dump")
	out, err := cmd.Output()
	if err != nil {
		// stderr from `wg show` carries the kernel reason (e.g. "Unable
		// to access interface: No such device") — propagate it.
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("wg show %s: %s", iface, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("wg show %s: %w", iface, err)
	}
	return out, nil
}

// pollPeers ticks every interval, runs wg show dump, parses, and emits
// an EventMetric. Returns when ctx is cancelled (Run stop). Emits one
// EventLog warning if `wg` is missing — subsequent ticks silently skip
// so dock isn't spammed.
//
// Designed to be cheap: ~one fork per tick, output is < 1 KiB per peer.
func (r *wireguardRun) pollPeers(ctx context.Context) {
	interval := time.Duration(r.pollIntervalSec) * time.Second
	if interval <= 0 {
		interval = wgDefaultPollIntervalSec * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Take one immediate sample so dock sees peer state right after
	// the iface comes up instead of waiting a full interval.
	r.samplePeersOnce(ctx)

	wgMissingWarned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !r.samplePeersOnce(ctx) && !wgMissingWarned {
				wgMissingWarned = true
				// One-shot warning — if `wg` isn't on PATH (wg-quick
				// magically isn't either but we got past Start somehow),
				// don't spam the log every interval.
				r.send(Event{Kind: EventLog, Data: map[string]any{
					"channel": "monitor",
					"line":    "peer monitor disabled: `wg` not on PATH (install wireguard-tools to enable)",
				}})
			}
		}
	}
}

// samplePeersOnce runs one poll cycle. Returns false ONLY when `wg`
// itself is missing — that's the case where the caller should emit a
// one-time warning. Other errors (iface gone, exec timeout) are
// surfaced as EventLog{monitor} per tick because they're transient
// and the operator wants to see them.
func (r *wireguardRun) samplePeersOnce(ctx context.Context) bool {
	out, err := runWGShowDump(ctx, r.iface)
	if err != nil {
		if strings.Contains(err.Error(), "wg not on PATH") {
			return false
		}
		r.send(Event{Kind: EventLog, Data: map[string]any{
			"channel": "monitor",
			"line":    err.Error(),
		}})
		return true
	}
	iface, peers, err := parseWGShowDump(out, time.Now().UTC())
	if err != nil {
		r.send(Event{Kind: EventLog, Data: map[string]any{
			"channel": "monitor",
			"line":    "parse wg show: " + err.Error(),
		}})
		return true
	}
	r.send(Event{Kind: EventMetric, Data: map[string]any{
		"kind":             "wg_peer_status",
		"iface":            r.iface,
		"iface_public_key": iface.PublicKey,
		"listen_port":      iface.ListenPort,
		"fwmark":           iface.Fwmark,
		"peer_count":       len(peers),
		"peers":            peers,
		"sampled_at":       time.Now().UTC().Format(time.RFC3339),
	}})
	return true
}
