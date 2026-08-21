//go:build darwin || freebsd || linux

package main

// fw_task.go — polar-firewall 的 fw executor(FW-P4)。claim compute-task skill
// `fw_apply` / `fw_rollback`,把 fw-svc 编译好的产物(pf.conf / nft ruleset)
// staged 应用到本机防火墙。合约:polar-firewall doc/api.md §4/§5。
//
// 事务安全模型(design.md §5/§6):
//
//	备份当前状态 → 语法检查 → staged apply → confirm 握手 → commit / 本地回滚
//
// confirm 握手(POST fw_base_url/agent/transactions/:id/confirm,per-txn
// confirm_token 认证)这次 HTTP 往返本身就是 controller-reachable 证明:
// 拿到 {commit:true} 才落 commit;拿到 {commit:false}、或 rollback 窗口内
// 一直调不通 → 本地回滚(备份与回滚全在本机完成,不依赖网络)。
//
// 状态目录(POLAR_AGENT_FW_DIR,默认 ~/.polar/fw):
//
//	current.conf  — 当前 committed 的产物(fw_rollback 之后 = 回滚到的状态)
//	previous.conf — 上一次 apply 前的状态(fw_rollback 的还原目标)
//	current.hash  — current.conf 的 compiled_hash(drift 对账)
//
// 提权:agent 以 root 跑则直接执行,否则命令前缀 `sudo -n`(fleet 部署需给
// pfctl / nft 配 NOPASSWD sudoers)。防火墙操作全局串行(fwMu):compute 循环
// 最多 4 并发,但一台机器同一时刻只允许一个防火墙事务在动。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	fwSkillApply    = "fw_apply"
	fwSkillRollback = "fw_rollback"

	fwBackendPF  = "pf"
	fwBackendNFT = "nftables"

	fwConfirmRetryInterval = 3 * time.Second
	fwConfirmAttemptTO     = 10 * time.Second
)

var fwMu sync.Mutex

func registerFirewallHandlers() {
	registerComputeHandler(fwSkillApply, runFWApplyTask)
	registerComputeHandler(fwSkillRollback, runFWRollbackTask)
}

// fwTaskInput 镜像 fw-svc 的 task input(键名合约,两边都有测试锁)。
type fwTaskInput struct {
	TxnID              int64  `json:"txn_id"`
	FWBaseURL          string `json:"fw_base_url"`
	Backend            string `json:"backend"`
	Compiled           string `json:"compiled"`
	CompiledHash       string `json:"compiled_hash"`
	PolicyVersion      int    `json:"policy_version"`
	RollbackTimeoutSec int    `json:"rollback_timeout_sec"`
	Mode               string `json:"mode"`
	ConfirmToken       string `json:"confirm_token"`
}

// fwExec 把外部世界(命令执行/HTTP/状态目录)收进可注入的一层,单测全走 stub。
type fwExec struct {
	backend  string
	stateDir string
	goos     string
	run      func(ctx context.Context, argv ...string) (string, error)
	client   *http.Client
}

func newFWExec(backend string) (*fwExec, error) {
	dir := strings.TrimSpace(os.Getenv("POLAR_AGENT_FW_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".polar", "fw")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &fwExec{
		backend:  backend,
		stateDir: dir,
		goos:     runtime.GOOS,
		run:      fwRunCmd,
		client:   &http.Client{Timeout: fwConfirmAttemptTO},
	}, nil
}

// fwRunCmd 执行一条命令(必要时 sudo -n 前缀),返回合并输出。
func fwRunCmd(ctx context.Context, argv ...string) (string, error) {
	if os.Geteuid() != 0 {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil {
		return out, fmt.Errorf("%s: %w (%s)", strings.Join(argv, " "), err, truncateForErr(out))
	}
	return out, nil
}

func (e *fwExec) path(name string) string { return filepath.Join(e.stateDir, name) }

// backendSupported:pf ↔ darwin/freebsd,nftables ↔ linux。
func (e *fwExec) backendSupported() error {
	switch e.backend {
	case fwBackendPF:
		if e.goos == "darwin" || e.goos == "freebsd" {
			return nil
		}
	case fwBackendNFT:
		if e.goos == "linux" {
			return nil
		}
	}
	return fmt.Errorf("fw: backend %q unsupported on %s", e.backend, e.goos)
}

func (e *fwExec) nftBin() string {
	if p, err := exec.LookPath("nft"); err == nil {
		return p
	}
	return "/usr/sbin/nft"
}

// captureBackup 取"apply 前状态"的可回放文本。
// pf:上一次 committed 的 current.conf;从未管理过则回退 /etc/pf.conf(OS 默认)。
// nft:只备我们独占的 table inet polar_fw(declare→delete→dump 可回放脚本);
// 表不存在时回放脚本 = 纯删表(还原到"未管理"状态),不碰机器上其它表。
func (e *fwExec) captureBackup(ctx context.Context) (string, error) {
	switch e.backend {
	case fwBackendPF:
		if b, err := os.ReadFile(e.path("current.conf")); err == nil {
			return string(b), nil
		}
		b, err := os.ReadFile("/etc/pf.conf")
		if err != nil {
			return "", fmt.Errorf("fw: no prior state and no /etc/pf.conf: %w", err)
		}
		return string(b), nil
	case fwBackendNFT:
		script := "table inet polar_fw\ndelete table inet polar_fw\n"
		dump, err := e.run(ctx, e.nftBin(), "list", "table", "inet", "polar_fw")
		if err == nil && strings.TrimSpace(dump) != "" {
			script += dump + "\n"
		}
		return script, nil
	}
	return "", fmt.Errorf("fw: unknown backend %q", e.backend)
}

// checkAndApply:staged 文件语法检查 + 加载。pf 加载后确保 pf 已启用。
func (e *fwExec) checkAndApply(ctx context.Context, file string) error {
	switch e.backend {
	case fwBackendPF:
		if _, err := e.run(ctx, "/sbin/pfctl", "-nf", file); err != nil {
			return fmt.Errorf("fw: pf syntax check: %w", err)
		}
		if _, err := e.run(ctx, "/sbin/pfctl", "-f", file); err != nil {
			return fmt.Errorf("fw: pf load: %w", err)
		}
		return e.pfEnable(ctx)
	case fwBackendNFT:
		if _, err := e.run(ctx, e.nftBin(), "-c", "-f", file); err != nil {
			return fmt.Errorf("fw: nft check: %w", err)
		}
		if _, err := e.run(ctx, e.nftBin(), "-f", file); err != nil {
			return fmt.Errorf("fw: nft load: %w", err)
		}
		return nil
	}
	return fmt.Errorf("fw: unknown backend %q", e.backend)
}

// pfEnable:darwin 用 -E(引用计数,幂等);freebsd 用 -e,"already enabled"
// 不算错。规则装了但 pf 没开等于没装,启用失败必须冒泡。
func (e *fwExec) pfEnable(ctx context.Context) error {
	if e.goos == "darwin" {
		if _, err := e.run(ctx, "/sbin/pfctl", "-E"); err != nil {
			return fmt.Errorf("fw: pf enable: %w", err)
		}
		return nil
	}
	if out, err := e.run(ctx, "/sbin/pfctl", "-e"); err != nil && !strings.Contains(out, "already enabled") {
		return fmt.Errorf("fw: pf enable: %w", err)
	}
	return nil
}

// restore 回放一份备份文本。
func (e *fwExec) restore(ctx context.Context, backup string) error {
	file := e.path("rollback.conf")
	if err := os.WriteFile(file, []byte(backup), 0o600); err != nil {
		return err
	}
	switch e.backend {
	case fwBackendPF:
		if _, err := e.run(ctx, "/sbin/pfctl", "-f", file); err != nil {
			return fmt.Errorf("fw: pf restore: %w", err)
		}
		return nil
	case fwBackendNFT:
		if _, err := e.run(ctx, e.nftBin(), "-f", file); err != nil {
			return fmt.Errorf("fw: nft restore: %w", err)
		}
		return nil
	}
	return fmt.Errorf("fw: unknown backend %q", e.backend)
}

// promote:commit 后落状态文件(previous ← 备份,current ← 产物)。
func (e *fwExec) promote(backup, compiled, hash string) error {
	if err := os.WriteFile(e.path("previous.conf"), []byte(backup), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(e.path("current.conf"), []byte(compiled), 0o600); err != nil {
		return err
	}
	return os.WriteFile(e.path("current.hash"), []byte(hash), 0o600)
}

// ── fw-svc 回连 ─────────────────────────────────────────────────────

type fwConfirmResp struct {
	Commit bool   `json:"commit"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// postConfirm 一次 confirm 尝试。definitive=true 表示拿到了 fw-svc 的明确
// 结论(2xx 的 commit 布尔,或 4xx 这类重试也不会好的拒绝);false = 传输层/
// 5xx,窗口内可重试。
func (e *fwExec) postConfirm(ctx context.Context, in fwTaskInput, appliedHash string) (commit bool, definitive bool, err error) {
	body, _ := json.Marshal(map[string]any{
		"confirm_token": in.ConfirmToken,
		"applied_hash":  appliedHash,
		"health":        map[string]any{"goos": e.goos},
	})
	url := fmt.Sprintf("%s/agent/transactions/%d/confirm", strings.TrimRight(in.FWBaseURL, "/"), in.TxnID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return false, false, fmt.Errorf("confirm: http %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return false, true, fmt.Errorf("confirm rejected: http %d", resp.StatusCode)
	}
	var r fwConfirmResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return false, false, fmt.Errorf("confirm: decode: %w", err)
	}
	return r.Commit, true, nil
}

// confirmLoop 在 rollback 窗口内反复尝试 confirm。返回是否 commit 与说明。
func (e *fwExec) confirmLoop(ctx context.Context, in fwTaskInput, appliedHash string, deadline time.Time) (bool, string) {
	for {
		commit, definitive, err := e.postConfirm(ctx, in, appliedHash)
		if definitive {
			if err != nil {
				return false, err.Error()
			}
			if !commit {
				return false, "controller refused commit"
			}
			return true, ""
		}
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("controller unreachable within %ds window: %v", in.RollbackTimeoutSec, err)
		}
		log.Printf("[fw] confirm txn=%d retry: %v", in.TxnID, err)
		select {
		case <-ctx.Done():
			return false, "cancelled: " + ctx.Err().Error()
		case <-time.After(fwConfirmRetryInterval):
		}
	}
}

// reportStatus 中间态心跳(best effort,失败只打日志)。
func (e *fwExec) reportStatus(ctx context.Context, in fwTaskInput, status string) {
	body, _ := json.Marshal(map[string]any{"confirm_token": in.ConfirmToken, "status": status})
	url := fmt.Sprintf("%s/agent/transactions/%d/status", strings.TrimRight(in.FWBaseURL, "/"), in.TxnID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := e.client.Do(req); err == nil {
		resp.Body.Close()
	} else {
		log.Printf("[fw] status txn=%d: %v", in.TxnID, err)
	}
}

// ── handlers ────────────────────────────────────────────────────────

func parseFWInput(t computeTask, wantMode string) (fwTaskInput, error) {
	var in fwTaskInput
	if err := json.Unmarshal(t.Input, &in); err != nil {
		return in, fmt.Errorf("fw: bad input: %w", err)
	}
	if in.TxnID <= 0 || in.FWBaseURL == "" || in.ConfirmToken == "" {
		return in, fmt.Errorf("fw: txn_id/fw_base_url/confirm_token required")
	}
	if in.Mode != wantMode {
		return in, fmt.Errorf("fw: mode %q on skill for %q", in.Mode, wantMode)
	}
	if in.RollbackTimeoutSec <= 0 {
		in.RollbackTimeoutSec = 60
	}
	return in, nil
}

func fwHashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func runFWApplyTask(ctx context.Context, cfg AgentConfig, t computeTask) (any, error) {
	in, err := parseFWInput(t, "apply")
	if err != nil {
		return nil, err
	}
	if in.Compiled == "" {
		return nil, fmt.Errorf("fw: empty compiled artifact")
	}
	if got := fwHashOf(in.Compiled); got != in.CompiledHash {
		return nil, fmt.Errorf("fw: compiled hash mismatch (input %s, computed %s)", in.CompiledHash, got)
	}
	e, err := newFWExec(in.Backend)
	if err != nil {
		return nil, err
	}
	return fwApply(e, in)
}

// fwApply 是 apply 事务主体(fwExec 可注入,单测覆盖三条路径:commit、
// controller 拒绝→回滚、不可达→回滚)。刻意脱离 claim 循环的 session ctx:
// 规则一旦上机,WS 掉线不能把 confirm/回滚逻辑一起带走 —— confirm 本来就是
// 网络健康探针,必须走完。
func fwApply(e *fwExec, in fwTaskInput) (any, error) {
	if err := e.backendSupported(); err != nil {
		return nil, err
	}
	fwMu.Lock()
	defer fwMu.Unlock()

	window := time.Duration(in.RollbackTimeoutSec) * time.Second
	opCtx, cancel := context.WithTimeout(context.Background(), window+2*time.Minute)
	defer cancel()
	deadline := time.Now().Add(window)

	e.reportStatus(opCtx, in, "applying")

	backup, err := e.captureBackup(opCtx)
	if err != nil {
		return nil, err
	}
	staged := e.path("staged.conf")
	if err := os.WriteFile(staged, []byte(in.Compiled), 0o600); err != nil {
		return nil, err
	}
	if err := e.checkAndApply(opCtx, staged); err != nil {
		// staged 失败:pf/nft 的 -f 是原子加载,失败即未生效,无需回滚。
		return nil, err
	}
	log.Printf("[fw] txn=%d %s applied (staged), confirming within %s", in.TxnID, e.backend, window)

	commit, reason := e.confirmLoop(opCtx, in, in.CompiledHash, deadline)
	if !commit {
		if rerr := e.restore(opCtx, backup); rerr != nil {
			// 回滚也失败:机器可能带着未确认规则 —— 这是最坏情况,把两个错都上报。
			return map[string]any{"rolled_back": false},
				fmt.Errorf("fw: %s; ROLLBACK FAILED: %v", reason, rerr)
		}
		log.Printf("[fw] txn=%d rolled back: %s", in.TxnID, reason)
		return map[string]any{"rolled_back": true}, fmt.Errorf("fw: rolled back: %s", reason)
	}
	if err := e.promote(backup, in.Compiled, in.CompiledHash); err != nil {
		log.Printf("[fw] txn=%d promote state files: %v", in.TxnID, err)
	}
	log.Printf("[fw] txn=%d committed hash=%s", in.TxnID, in.CompiledHash)
	return map[string]any{"committed": true, "applied_hash": in.CompiledHash, "policy_version": in.PolicyVersion}, nil
}

func runFWRollbackTask(ctx context.Context, cfg AgentConfig, t computeTask) (any, error) {
	in, err := parseFWInput(t, "rollback")
	if err != nil {
		return nil, err
	}
	e, err := newFWExec(in.Backend)
	if err != nil {
		return nil, err
	}
	return fwRollback(e, in)
}

// fwRollback 主动回滚:回放 previous.conf(上一次 apply 前的状态),然后
// current/previous 互换。结果经 task 完成 → dock 回调收口(fw-svc 侧
// rollback 事务不走 confirm 握手)。
func fwRollback(e *fwExec, in fwTaskInput) (any, error) {
	if err := e.backendSupported(); err != nil {
		return nil, err
	}
	fwMu.Lock()
	defer fwMu.Unlock()

	opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prev, err := os.ReadFile(e.path("previous.conf"))
	if err != nil {
		return nil, fmt.Errorf("fw: nothing to roll back (no previous state): %w", err)
	}
	e.reportStatus(opCtx, in, "applying")
	if err := e.restore(opCtx, string(prev)); err != nil {
		return nil, err
	}
	cur, _ := os.ReadFile(e.path("current.conf"))
	if err := os.WriteFile(e.path("current.conf"), prev, 0o600); err != nil {
		log.Printf("[fw] rollback txn=%d state swap: %v", in.TxnID, err)
	}
	if len(cur) > 0 {
		if err := os.WriteFile(e.path("previous.conf"), cur, 0o600); err != nil {
			log.Printf("[fw] rollback txn=%d state swap: %v", in.TxnID, err)
		}
	}
	_ = os.Remove(e.path("current.hash")) // 回滚后 hash 未知,下次 state 上报对账
	log.Printf("[fw] txn=%d rolled back to previous state", in.TxnID)
	return map[string]any{"rolled_back": true}, nil
}
