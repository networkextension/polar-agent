//go:build darwin || freebsd || linux || openbsd

package main

// route_collector.go — 60s state reporter for polar-routing: kernel route
// table (both families) + managed set + interface CIDRs + default gateway →
// POST {rt_base}/agent/state (Bearer agent token). Process-lifetime, one
// instance per box (flock), like fw_collector.
//
// Dial-back base: POLAR_AGENT_ROUTING_BASE, else ~/.polar/routing/base_url
// (persisted by the first task). Unlike fw, the collector is useful before
// any task ran (read-only fleet visibility), so POLAR_AGENT_ROUTING_BASE is
// the expected fleet-wide config.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/networkextension/polar-agent/cmd/polar-agent/routecmd"
)

const (
	routeCollectorInterval = 60 * time.Second
	routeCollectorHTTPTO   = 10 * time.Second
)

var routeCollectorOnce sync.Once

func startRoutingCollector(cfg AgentConfig) {
	routeCollectorOnce.Do(func() {
		go routeCollectorLoop(context.Background(), cfg)
	})
}

type routeCollector struct {
	cfg      AgentConfig
	stateDir string
	goos     string
	client   *http.Client
	lockFile *os.File

	warnMu sync.Mutex
	warned map[string]bool
}

func newRouteCollector(cfg AgentConfig) (*routeCollector, error) {
	dir := strings.TrimSpace(os.Getenv("POLAR_AGENT_ROUTING_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".polar", "routing")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &routeCollector{cfg: cfg, stateDir: dir, goos: runtime.GOOS,
		client: &http.Client{Timeout: routeCollectorHTTPTO}, warned: map[string]bool{}}, nil
}

func (c *routeCollector) path(name string) string { return filepath.Join(c.stateDir, name) }

func (c *routeCollector) baseURL() string {
	if v := strings.TrimSpace(os.Getenv("POLAR_AGENT_ROUTING_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	if b, err := os.ReadFile(c.path("base_url")); err == nil {
		return strings.TrimRight(strings.TrimSpace(string(b)), "/")
	}
	return ""
}

// acquireLock: one collector per box (several agent processes may run).
func (c *routeCollector) acquireLock() bool {
	f, err := os.OpenFile(c.path("collector.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true // can't lock → run anyway rather than silently vanish
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return false
	}
	c.lockFile = f
	return true
}

func (c *routeCollector) warnOnce(key, format string, a ...any) {
	c.warnMu.Lock()
	defer c.warnMu.Unlock()
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	log.Printf(format, a...)
}

func routeCollectorLoop(ctx context.Context, cfg AgentConfig) {
	c, err := newRouteCollector(cfg)
	if err != nil {
		log.Printf("[route] collector disabled: %v", err)
		return
	}
	if !c.acquireLock() {
		return
	}
	for {
		if base := c.baseURL(); base != "" {
			log.Printf("[route] collector active (base=%s)", base)
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(routeCollectorInterval):
		}
	}
	c.reportState(ctx)
	t := time.NewTicker(routeCollectorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.reportState(ctx)
		}
	}
}

type routeIface struct {
	Name  string   `json:"name"`
	Addrs []string `json:"addrs"`
}

func routeLocalIfaces() []routeIface {
	out := []routeIface{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		ri := routeIface{Name: i.Name}
		if addrs, err := i.Addrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.IsLinkLocalUnicast() {
					continue
				}
				ri.Addrs = append(ri.Addrs, a.String())
			}
		}
		if len(ri.Addrs) > 0 {
			out = append(out, ri)
		}
	}
	return out
}

func (c *routeCollector) snapshotTable(ctx context.Context) []routecmd.Route {
	var all []routecmd.Route
	for _, fam := range []int{4, 6} {
		argv, err := routecmd.ListArgv(c.goos, fam)
		if err != nil {
			return all
		}
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
		if err != nil {
			c.warnOnce(fmt.Sprintf("list%d", fam), "[route] list v%d table: %v", fam, err)
			continue
		}
		all = append(all, routecmd.ParseTable(c.goos, fam, string(out))...)
	}
	if all == nil {
		all = []routecmd.Route{}
	}
	return all
}

func (c *routeCollector) reportState(ctx context.Context) {
	table := c.snapshotTable(ctx)
	var managed []routecmd.Route
	if b, err := os.ReadFile(c.path("current.json")); err == nil {
		_ = json.Unmarshal(b, &managed)
	}
	if managed == nil {
		managed = []routecmd.Route{}
	}
	hash := ""
	if b, err := os.ReadFile(c.path("current.hash")); err == nil {
		hash = strings.TrimSpace(string(b))
	}
	gw := ""
	for _, r := range table {
		if r.Family == 4 && r.IsDefault() && r.Gateway != "" {
			gw = r.Gateway
			break
		}
	}
	body := map[string]any{
		"agent_id":     c.cfg.AgentID,
		"os":           c.goos,
		"table":        table,
		"managed":      managed,
		"managed_hash": hash,
		"ifaces":       routeLocalIfaces(),
		"default_gw":   gw,
	}
	if err := c.post(ctx, "/agent/state", body); err != nil {
		c.warnOnce("state:"+err.Error(), "[route] state report: %v", err)
	}
}

func (c *routeCollector) post(ctx context.Context, path string, body any) error {
	base := c.baseURL()
	if base == "" {
		return fmt.Errorf("route: no base url")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: http %d", path, resp.StatusCode)
	}
	return nil
}
