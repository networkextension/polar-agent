//go:build darwin || freebsd || linux

package main

// fw_collector.go — polar-firewall 的 agent 侧采集器(FW-P8)。两条线:
//
//   - state reporter(30s):running_hash(~/.polar/fw/current.hash)+ conntrack
//     快照(pf: pfctl -ss / linux: conntrack -L)+ 本机接口 → POST
//     {fw_base}/agent/state。fw-svc 用 running_hash 做 drift 对账
//     (fw_task.go 回滚后删 hash,正是靠这条上报收敛)。
//   - event tailer(常驻):pf 平台 tail `tcpdump -i pflog0`(pflog0 不跨重启
//     持久,每次 supervise 迭代都 ensure create);linux tail 内核日志过滤
//     nft log prefix "polarfw:…"。解析后批量(≤100/批,5s flush,cap 1000
//     drop-on-full)POST {fw_base}/agent/events。
//
// 生命周期抄 recovery_watcher:进程级 context.Background()(不挂 WS session
// ctx,断线不重启 tcpdump),sync.Once,POLAR_AGENT_FW_COLLECTOR_DISABLED=true
// 关闭。门控:等状态目录出现 current.conf + base_url(= 本机受管且知道
// fw-svc 地址;base_url 由 fw_task 在每次 apply/rollback 时落盘,env
// POLAR_AGENT_FW_BASE 可覆盖)。认证:Bearer cfg.Token(agent token,fw-svc
// requireAgent → dock AuthVerifyAgent)。
//
// 采集全程只读(pfctl -ss / tcpdump / journalctl),不取 fwMu。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	fwStatePeriod     = 30 * time.Second
	fwEventFlush      = 5 * time.Second
	fwEventBatchMax   = 100
	fwEventBufferCap  = 1000
	fwConnCap         = 500
	fwCollectorHTTPTO = 10 * time.Second
)

var fwCollectorOnce sync.Once

// startFirewallCollector 起采集器(main.go 在 registerFirewallHandlers 旁调用)。
func startFirewallCollector(cfg AgentConfig) {
	fwCollectorOnce.Do(func() {
		go fwCollectorLoop(context.Background(), cfg)
	})
}

// fwCollectorLoop:门控等待受管 → 起两条线。
func fwCollectorLoop(ctx context.Context, cfg AgentConfig) {
	c, err := newFWCollector(cfg)
	if err != nil {
		log.Printf("[fw] collector disabled: %v", err)
		return
	}
	// 每机单例:一台机器常跑多个 polar-agent 实例(zen 4 个 launchd bot),
	// 每个进程都会走到这里 —— flock 抢锁,抢不到的进程静默退出,否则
	// events/state 会按实例数翻倍上报。锁随进程存亡自动释放。
	if !c.acquireLock() {
		return
	}
	for {
		if base := c.baseURL(); base != "" {
			log.Printf("[fw] collector active (base=%s)", base)
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
	}
	go c.stateLoop(ctx)
	go c.eventLoop(ctx)
}

// fwCollector 把外部世界收进可注入的一层(单测全 stub)。
type fwCollector struct {
	cfg      AgentConfig
	stateDir string
	goos     string
	run      func(ctx context.Context, argv ...string) (string, error)
	client   *http.Client
	events   chan fwEventRow

	nowarnMu sync.Mutex
	warned   map[string]bool
}

func newFWCollector(cfg AgentConfig) (*fwCollector, error) {
	dir := strings.TrimSpace(os.Getenv("POLAR_AGENT_FW_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = home + "/.polar/fw"
	}
	return &fwCollector{
		cfg:      cfg,
		stateDir: dir,
		goos:     runtime.GOOS,
		run:      fwRunCmd,
		client:   &http.Client{Timeout: fwCollectorHTTPTO},
		events:   make(chan fwEventRow, fwEventBufferCap),
		warned:   map[string]bool{},
	}, nil
}

// acquireLock:LOCK_EX|LOCK_NB 抢 stateDir/collector.lock;fd 故意不关,
// 锁生命周期 = 进程生命周期。
func (c *fwCollector) acquireLock() bool {
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		log.Printf("[fw] collector lock dir: %v", err)
		return false
	}
	f, err := os.OpenFile(c.path("collector.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		log.Printf("[fw] collector lock: %v", err)
		return false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close() // 别的实例在跑,本进程静默让位
		return false
	}
	return true
}

func (c *fwCollector) warnOnce(key, format string, a ...any) {
	c.nowarnMu.Lock()
	defer c.nowarnMu.Unlock()
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	log.Printf(format, a...)
}

func (c *fwCollector) path(name string) string { return c.stateDir + "/" + name }

// baseURL:受管(current.conf 存在)且知道 fw-svc 地址才非空。
func (c *fwCollector) baseURL() string {
	if _, err := os.Stat(c.path("current.conf")); err != nil {
		return ""
	}
	if v := strings.TrimSpace(os.Getenv("POLAR_AGENT_FW_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	b, err := os.ReadFile(c.path("base_url"))
	if err != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(string(b)), "/")
}

func (c *fwCollector) post(ctx context.Context, path string, body any) error {
	base := c.baseURL()
	if base == "" {
		return fmt.Errorf("fw: no base url")
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

// ── state reporter ──────────────────────────────────────────────────

type fwConn struct {
	Proto string `json:"protocol,omitempty"`
	Src   string `json:"src,omitempty"`
	Dst   string `json:"dst,omitempty"`
	State string `json:"state,omitempty"`
	Iface string `json:"iface,omitempty"`
}

type fwIface struct {
	Name  string   `json:"name"`
	Addrs []string `json:"addrs,omitempty"`
}

func (c *fwCollector) stateLoop(ctx context.Context) {
	t := time.NewTicker(fwStatePeriod)
	defer t.Stop()
	for {
		c.reportState(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *fwCollector) reportState(ctx context.Context) {
	hash := ""
	if b, err := os.ReadFile(c.path("current.hash")); err == nil {
		hash = strings.TrimSpace(string(b))
	}
	conns := c.snapshotConns(ctx)
	body := map[string]any{
		"agent_id":     c.cfg.AgentID,
		"running_hash": hash,
		"connections":  conns,
		"ifaces":       localIfaces(),
	}
	if err := c.post(ctx, "/agent/state", body); err != nil {
		c.warnOnce("state:"+err.Error(), "[fw] state report: %v", err)
	}
}

func (c *fwCollector) snapshotConns(ctx context.Context) []fwConn {
	switch c.goos {
	case "darwin", "freebsd":
		out, err := c.run(ctx, "/sbin/pfctl", "-ss")
		if err != nil {
			c.warnOnce("pfctl-ss", "[fw] pfctl -ss: %v", err)
			return []fwConn{}
		}
		return parsePFStates(out, fwConnCap)
	case "linux":
		bin, err := exec.LookPath("conntrack")
		if err != nil {
			c.warnOnce("conntrack", "[fw] conntrack not installed; connections empty")
			return []fwConn{}
		}
		out, err := c.run(ctx, bin, "-L")
		if err != nil {
			c.warnOnce("conntrack-l", "[fw] conntrack -L: %v", err)
			return []fwConn{}
		}
		return parseConntrack(out, fwConnCap)
	}
	return []fwConn{}
}

// parsePFStates 解析 `pfctl -ss`:
//
//	IFACE proto src -> dst [-> dst2] STATE:STATE
//
// NAT 三段(src -> nat -> dst)取首尾。IFACE 可为 ALL。
func parsePFStates(out string, cap_ int) []fwConn {
	conns := []fwConn{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 5 {
			continue
		}
		conn := fwConn{Iface: f[0], Proto: strings.ToLower(f[1]), Src: f[2], State: f[len(f)-1]}
		// f[3] 起是 "->"/"<-" 与地址交替;最后一个地址在 state 前。
		conn.Dst = f[len(f)-2]
		if conn.Iface == "ALL" {
			conn.Iface = ""
		}
		conns = append(conns, conn)
		if len(conns) >= cap_ {
			break
		}
	}
	return conns
}

var conntrackStateRe = regexp.MustCompile(`^[A-Z_]{4,}$`)

// parseConntrack 解析 `conntrack -L` 常见行:
//
//	tcp 6 431999 ESTABLISHED src=10.0.0.2 dst=1.2.3.4 sport=5 dport=443 …
func parseConntrack(out string, cap_ int) []fwConn {
	conns := []fwConn{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		conn := fwConn{Proto: f[0]}
		kv := map[string]string{}
		for _, tok := range f {
			if k, v, ok := strings.Cut(tok, "="); ok {
				if _, seen := kv[k]; !seen { // 首次出现(orig 方向)为准
					kv[k] = v
				}
			} else if conn.State == "" && conntrackStateRe.MatchString(tok) {
				conn.State = tok // 纯大写字母段 = 状态(ESTABLISHED 等);[ASSURED] 带括号不算
			}
		}
		if kv["src"] == "" {
			continue
		}
		conn.Src = kv["src"]
		conn.Dst = kv["dst"]
		if kv["sport"] != "" {
			conn.Src += ":" + kv["sport"]
		}
		if kv["dport"] != "" {
			conn.Dst += ":" + kv["dport"]
		}
		conns = append(conns, conn)
		if len(conns) >= cap_ {
			break
		}
	}
	return conns
}

func localIfaces() []fwIface {
	out := []fwIface{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		fi := fwIface{Name: i.Name}
		if addrs, err := i.Addrs(); err == nil {
			for _, a := range addrs {
				fi.Addrs = append(fi.Addrs, a.String())
			}
		}
		if len(fi.Addrs) > 0 {
			out = append(out, fi)
		}
	}
	return out
}

// ── event tailer ────────────────────────────────────────────────────

type fwEventRow struct {
	TS        time.Time `json:"ts"`
	Iface     string    `json:"iface,omitempty"`
	Direction string    `json:"direction,omitempty"`
	Proto     string    `json:"proto,omitempty"`
	Src       string    `json:"src,omitempty"`
	SrcPort   int       `json:"src_port,omitempty"`
	Dst       string    `json:"dst,omitempty"`
	DstPort   int       `json:"dst_port,omitempty"`
	Action    string    `json:"action,omitempty"`
	RuleRef   string    `json:"rule_ref,omitempty"`
	Raw       any       `json:"raw,omitempty"`
}

func (c *fwCollector) eventLoop(ctx context.Context) {
	go c.flushLoop(ctx)
	backoff := 5 * time.Second
	for {
		start := time.Now()
		err := c.tailOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.warnOnce("tail:"+err.Error(), "[fw] event tail: %v", err)
		}
		// 跑得久说明之前正常,重置退避。
		if time.Since(start) > time.Minute {
			backoff = 5 * time.Second
		} else if backoff < time.Minute {
			backoff *= 2
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// tailOnce 起一次 tail 子进程并逐行喂解析;进程退出即返回(supervise 重启)。
func (c *fwCollector) tailOnce(ctx context.Context) error {
	var argv []string
	var parse func(string) (fwEventRow, bool)
	switch c.goos {
	case "darwin", "freebsd":
		// pflog0 不跨重启持久;create 幂等(exists 报错忽略)。
		if out, err := c.run(ctx, "/sbin/ifconfig", "pflog0", "create"); err != nil &&
			!strings.Contains(out, "exists") && !strings.Contains(err.Error(), "exists") {
			return fmt.Errorf("pflog0 create: %w", err)
		}
		// -q:抑制应用层解码(否则 5353 显示 "domain" 而非 "UDP",proto 判不出)。
		argv = []string{"/usr/sbin/tcpdump", "-i", "pflog0", "-n", "-e", "-l", "-ttt", "-q"}
		parse = parsePFLogLine
	case "linux":
		if p, err := exec.LookPath("journalctl"); err == nil {
			argv = []string{p, "-kf", "-o", "cat", "--no-pager"}
		} else if p, err := exec.LookPath("dmesg"); err == nil {
			argv = []string{p, "-w"}
		} else {
			return fmt.Errorf("no journalctl/dmesg for nft log tail")
		}
		parse = parseNFTLogLine
	default:
		return fmt.Errorf("unsupported goos %s", c.goos)
	}
	if os.Geteuid() != 0 {
		argv = append([]string{"sudo", "-n"}, argv...)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		row, ok := parse(scanner.Text())
		if !ok {
			continue
		}
		select {
		case c.events <- row:
		default: // 满了丢弃(洪泛保护)
		}
	}
	return cmd.Wait()
}

// flushLoop:5s 或攒满 100 条冲一批。
func (c *fwCollector) flushLoop(ctx context.Context) {
	t := time.NewTicker(fwEventFlush)
	defer t.Stop()
	batch := make([]fwEventRow, 0, fwEventBatchMax)
	flush := func(pctx context.Context) {
		if len(batch) == 0 {
			return
		}
		body := map[string]any{"agent_id": c.cfg.AgentID, "events": batch}
		if err := c.post(pctx, "/agent/events", body); err != nil {
			c.warnOnce("events:"+err.Error(), "[fw] event post: %v", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			// 退出前冲残余:原 ctx 已取消,换短时独立 ctx。
			fctx, cancel := context.WithTimeout(context.Background(), fwCollectorHTTPTO)
			flush(fctx)
			cancel()
			return
		case row := <-c.events:
			batch = append(batch, row)
			if len(batch) >= fwEventBatchMax {
				flush(ctx)
			}
		case <-t.C:
			flush(ctx)
		}
	}
}

// ── 行解析 ──────────────────────────────────────────────────────────

// parsePFLogLine 解析 tcpdump -i pflog0 -n -e -ttt 输出,如:
//
//	00:00:00.000000 rule 2/0(match): block in on en0: 10.0.0.22.53122 > 1.2.3.4.22: Flags [S], …
//	00:00:01.000000 rule 0/0(match): pass out on en0: 10.0.0.5.5353 > 224.0.0.251.5353: UDP, length 45
var pfLogRe = regexp.MustCompile(`rule ([\d.]+)/\d+\([^)]*\): (pass|block|rdr|nat) (in|out) on (\S+): (\S+) > (\S+?): (.*)`)

func parsePFLogLine(line string) (fwEventRow, bool) {
	m := pfLogRe.FindStringSubmatch(line)
	if m == nil {
		return fwEventRow{}, false
	}
	row := fwEventRow{
		TS:      time.Now().UTC(),
		RuleRef: "rule " + m[1],
		Action:  map[string]string{"pass": "allow", "block": "drop"}[m[2]],
		Iface:   m[4],
	}
	if row.Action == "" {
		row.Action = m[2]
	}
	if m[3] == "in" {
		row.Direction = "in"
	} else {
		row.Direction = "out"
	}
	row.Src, row.SrcPort = splitHostPortDot(m[5])
	row.Dst, row.DstPort = splitHostPortDot(m[6])
	tail := m[7]
	switch {
	case strings.Contains(tail, "Flags [") || strings.HasPrefix(tail, "tcp"):
		row.Proto = "tcp"
	case strings.HasPrefix(tail, "UDP") || strings.HasPrefix(tail, "udp") || strings.Contains(tail, " UDP"):
		row.Proto = "udp"
	case strings.Contains(tail, "ICMP") || strings.Contains(tail, "icmp"):
		row.Proto = "icmp"
	}
	row.Raw = map[string]string{"line": truncateForErr(line)}
	return row, true
}

// splitHostPortDot:tcpdump 的 "1.2.3.4.443" / "fe80::1.5353" → (host, port)。
// 裸地址(ICMP 等无端口)原样返回:整串本身是合法 IP 就绝不拆
// ("9.9.9.9" 不能拆成 9.9.9 + 9)。
func splitHostPortDot(s string) (string, int) {
	if net.ParseIP(s) != nil {
		return s, 0
	}
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return s, 0
	}
	if p, err := strconv.Atoi(s[i+1:]); err == nil && p > 0 && p <= 65535 {
		if host := s[:i]; net.ParseIP(host) != nil {
			return host, p
		}
	}
	return s, 0
}

// parseNFTLogLine 解析内核 netfilter 日志(我们的 nft 产物 log prefix
// "polarfw:<chain>:<ref>: "),如:
//
//	polarfw:input:ssh: IN=eth0 OUT= MAC=… SRC=1.2.3.4 DST=5.6.7.8 … PROTO=TCP SPT=1024 DPT=22 …
func parseNFTLogLine(line string) (fwEventRow, bool) {
	i := strings.Index(line, "polarfw:")
	if i < 0 {
		return fwEventRow{}, false
	}
	rest := line[i+len("polarfw:"):]
	chain, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return fwEventRow{}, false
	}
	ref, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return fwEventRow{}, false
	}
	row := fwEventRow{TS: time.Now().UTC(), RuleRef: chain + ":" + ref}
	kv := map[string]string{}
	for _, tok := range strings.Fields(rest) {
		if k, v, ok := strings.Cut(tok, "="); ok {
			kv[k] = v
		}
	}
	if kv["IN"] != "" {
		row.Iface, row.Direction = kv["IN"], "in"
	} else if kv["OUT"] != "" {
		row.Iface, row.Direction = kv["OUT"], "out"
	}
	row.Src = kv["SRC"]
	row.Dst = kv["DST"]
	row.Proto = strings.ToLower(kv["PROTO"])
	row.SrcPort, _ = strconv.Atoi(kv["SPT"])
	row.DstPort, _ = strconv.Atoi(kv["DPT"])
	if row.Src == "" {
		return fwEventRow{}, false
	}
	row.Raw = map[string]string{"line": truncateForErr(line)}
	return row, true
}
