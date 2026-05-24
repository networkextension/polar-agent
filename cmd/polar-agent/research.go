package main

// Research-mode runner: receives a research_task envelope from dock
// over WS, runs a local LLM tool loop (Aider-style), and reports
// progress back via HTTP callbacks.
//
// Wire shape (matches internal/app/dock/agent_hub.go agentResearchTask):
//
//   {
//     "kind":"research_task",
//     "research_run_id":7,
//     "project_id":"proj_xyz",
//     "task_id":"task_abc",
//     "task_title":"...",
//     "chat_thread_id":42,
//     "llm_thread_id":99,
//     "workdir":"<subpath>",
//     "git_remote_url":"git@...",
//     "task_content":"<rendered pickup body>",
//     "llm":{"name":"...","model":"...","base_url":"...","api_key":"..."}
//   }
//
// Lifecycle:
//   1. POST /research/runs/<id>/start         (ack to dock)
//   2. mkdir -p workdir; gitPrepare if remote
//   3. tool loop with [read_file, write_file, list_dir, run_cmd]
//   4. git add/commit/push with LLM-trailer commit message
//   5. POST /research/runs/<id>/result       (terminal)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	researchMaxIterations = 16
	researchToolResultCap = 8000 // chars per tool_result fed back to LLM
)

type researchTaskEnvelope struct {
	Kind          string                 `json:"kind"`
	ResearchRunID int64                  `json:"research_run_id"`
	ProjectID     string                 `json:"project_id"`
	TaskID        string                 `json:"task_id"`
	TaskTitle     string                 `json:"task_title"`
	ChatThreadID  int64                  `json:"chat_thread_id"`
	LLMThreadID   int64                  `json:"llm_thread_id"`
	Workdir       string                 `json:"workdir"`
	GitRemoteURL  string                 `json:"git_remote_url"`
	TaskContent   string                 `json:"task_content"`
	LLM           researchLLMSnapshot    `json:"llm"`
}

type researchLLMSnapshot struct {
	ConfigID int64  `json:"config_id"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	// ProxyURL: optional HTTP(S) proxy for the upstream LLM call.
	// Empty = direct. Dock plumbs this from llm_configs.proxy_url
	// so operators don't have to touch agent-side env vars.
	ProxyURL string `json:"proxy_url"`
}

type researchResult struct {
	OK           bool     `json:"ok"`
	Iterations   int      `json:"iterations"`
	FilesWritten []string `json:"files_written"`
	CommitSHA    string   `json:"commit_sha"`
	Summary      string   `json:"summary"`
	ErrorMessage string   `json:"error_message,omitempty"`
	LogText      string   `json:"log_text"`
}

// runResearchTask is the entry point invoked from the WS message
// dispatcher. Returns nil on a successful POST-result (whether the
// underlying research succeeded or failed); only returns non-nil if
// even reporting back to dock failed.
func runResearchTask(ctx context.Context, cfg AgentConfig, baseWorkdir string, env researchTaskEnvelope, verbose bool) error {
	logger := newResearchLogger()
	defer logger.flushTo(os.Stderr)

	logger.printf("[research] run=%d project=%s task=%s llm=%s/%s",
		env.ResearchRunID, env.ProjectID, env.TaskID, env.LLM.Name, env.LLM.Model)

	// Resolve workdir: same convention as chat_message — subpath
	// joined under the agent's pinned workdir, mkdir -p so the loop
	// finds an empty dir on first use.
	workdir := baseWorkdir
	if sp := strings.TrimSpace(env.Workdir); sp != "" {
		if !isSafeSubpath(sp) {
			return reportFailure(ctx, cfg, env.ResearchRunID, "invalid workdir subpath: "+sp, logger)
		}
		workdir = filepath.Join(baseWorkdir, sp)
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			return reportFailure(ctx, cfg, env.ResearchRunID, "mkdir workdir: "+err.Error(), logger)
		}
	}

	// Ack — flips dock-side run row queued→running and posts the
	// transparency message into the chat thread.
	if err := postResearchStart(ctx, cfg, env.ResearchRunID); err != nil {
		logger.printf("[research] /start callback failed: %v", err)
		// Continue anyway — losing the ack is annoying but not fatal,
		// and /result will still finalize the run.
	}

	// Optional pre-task git plumbing. If the project has a git remote
	// we clone/init + ensure remote is wired so the LLM can write
	// straight into a checked-out repo.
	var preHEAD string
	if remote := strings.TrimSpace(env.GitRemoteURL); remote != "" {
		logger.printf("[git] prepare workdir=%s remote=%s", workdir, remote)
		head, err := gitPrepare(ctx, workdir, remote)
		if err != nil {
			logger.printf("[git] prepare failed: %v", err)
			// Don't bail — let the LLM still write files locally; the
			// final git push will simply fail and we report it.
		} else {
			preHEAD = head
			logger.printf("[git] prepare ok preHEAD=%s", shortSHA(preHEAD))
		}
	}

	llm := newLLMClient(env.LLM.BaseURL, env.LLM.APIKey, env.LLM.ProxyURL, cfg.Server)
	exec := newExecutor(workdir, verbose)

	systemPrompt := buildResearchSystemPrompt(env, workdir)
	messages := []llmChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: env.TaskContent},
	}
	tools := researchToolSpecs()

	iterations, _, loopErr := runResearchToolLoop(ctx, llm, exec, env.LLM.Model, &messages, tools, logger)

	// Walk back through messages to find the last assistant text — use
	// it as the LLM's "final summary" for both the commit message body
	// and the result callback.
	finalContent := lastAssistantContent(messages)

	// Git commit + push with LLM trailer (transparency #4).
	commitSHA, filesWritten, gitErr := commitAndPushResearch(ctx, workdir, preHEAD, env, finalContent, logger)

	ok := loopErr == nil && gitErr == nil
	errMsg := ""
	if loopErr != nil {
		errMsg = loopErr.Error()
	} else if gitErr != nil {
		errMsg = "git: " + gitErr.Error()
	}

	res := researchResult{
		OK:           ok,
		Iterations:   iterations,
		FilesWritten: filesWritten,
		CommitSHA:    commitSHA,
		Summary:      truncateUTF8(finalContent, 4000),
		ErrorMessage: errMsg,
		LogText:      logger.tail(8000),
	}
	if err := postResearchResult(ctx, cfg, env.ResearchRunID, res); err != nil {
		return fmt.Errorf("post result: %w", err)
	}
	return nil
}

func reportFailure(ctx context.Context, cfg AgentConfig, runID int64, msg string, logger *researchLogger) error {
	logger.printf("[research] FAIL run=%d: %s", runID, msg)
	return postResearchResult(ctx, cfg, runID, researchResult{
		OK:           false,
		ErrorMessage: msg,
		LogText:      logger.tail(8000),
	})
}

func researchToolSpecs() []llmToolSpec {
	return []llmToolSpec{
		{
			Type: "function",
			Function: llmToolFunction{
				Name:        "read_file",
				Description: "Read a file relative to the working dir. Returns its contents.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: llmToolFunction{
				Name:        "write_file",
				Description: "Create or overwrite a file inside the working dir. Use this to save research outputs (markdown reports, design docs, plans, opinions, etc.) — DO NOT dump them in chat.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			Type: "function",
			Function: llmToolFunction{
				Name:        "list_dir",
				Description: "List entries under a directory inside the working dir. Returns name/is_dir/size for each.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Directory path. Defaults to '.'"},
					},
				},
			},
		},
		{
			Type: "function",
			Function: llmToolFunction{
				Name:        "run_cmd",
				Description: "Run a whitelisted command in the working dir. Allowed: git, swift, xcodebuild, xcrun, ls, cat, find, grep, swiftformat, swiftlint, open. 5min cap. Returns stdout/stderr/exit_code.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cmd":   map[string]any{"type": "string"},
						"args":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"stdin": map[string]any{"type": "string"},
					},
					"required": []string{"cmd"},
				},
			},
		},
	}
}

func buildResearchSystemPrompt(env researchTaskEnvelope, workdir string) string {
	return fmt.Sprintf(`你是研究助理，本次任务由 Polar 平台派发。请按以下规则工作：

- 当前 LLM: %s (%s)。这是平台告诉操作员的，不要伪装成别的模型。
- 工作目录: %s（你只能在这个目录里操作；read_file/write_file 路径都是相对该目录）。
- 任务产物形态不固定 — 文本报告、开发方案、设计稿、投研观点、时间线 …… 由任务内容决定。把产物写成文件（建议 markdown），不要把正文塞进 chat 回复里。
- 优先 plan 后执行；先用 list_dir 看一眼当前目录有没有东西、再决定写在哪个文件。
- 你没有联网工具（没有 web_fetch / web_search）。基于自身知识 + 任务给出的资料完成；事实存疑一定要在产物里标 "[需核实]"。
- 任务结束时 chat 回复留一句简短总结（1~3 句），具体内容写到文件里。git 由 polar-agent 在你之后自动 commit + push，不要自己调 git push。
- 工具异常（路径越权 / 命令未授权 / 写盘失败）会以 error 字段返回，请读 stderr 自己处理 — 别盲目重试相同调用。`,
		env.LLM.Name, env.LLM.Model, workdir,
	)
}

// dispatchResearchToolCall translates an LLM tool_call into the
// existing executor's toolCall shape, runs it, and produces the
// "role: tool" message that goes back into the LLM context.
// runResearchToolLoop drives one user-prompt's worth of LLM calls
// + tool dispatches. Returns the iteration count, last finish_reason
// (or "" when truncated), and any error. messages is mutated in place
// — the caller can keep it across multiple turns (REPL) or read the
// last assistant message for a summary (one-shot).
//
// Shared between the dock-driven path (runResearchTask) and the
// stdin REPL path so both behave identically: same tool semantics,
// same iteration cap, same logger.
func runResearchToolLoop(
	ctx context.Context,
	llm *llmClient,
	exec *executor,
	model string,
	messages *[]llmChatMessage,
	tools []llmToolSpec,
	logger *researchLogger,
) (iterations int, finishReason string, err error) {
	for i := 0; i < researchMaxIterations; i++ {
		iterations = i + 1
		resp, callErr := llm.chatCompletions(ctx, llmChatRequest{
			Model:      model,
			Messages:   *messages,
			Tools:      tools,
			ToolChoice: "auto",
			MaxTokens:  16384,
		})
		if callErr != nil {
			err = fmt.Errorf("llm call iter=%d: %w", iterations, callErr)
			return
		}
		choice := resp.Choices[0]
		assistant := choice.Message
		*messages = append(*messages, assistant)
		finishReason = choice.FinishReason

		if len(assistant.ToolCalls) == 0 {
			logger.printf("[research] iter=%d done (no tool_calls)", iterations)
			return
		}
		logger.printf("[research] iter=%d dispatching %d tool_call(s)", iterations, len(assistant.ToolCalls))
		for _, tc := range assistant.ToolCalls {
			toolMsg := dispatchResearchToolCall(ctx, exec, tc)
			*messages = append(*messages, toolMsg)
		}
	}
	err = fmt.Errorf("tool loop exceeded %d iterations", researchMaxIterations)
	return
}

func dispatchResearchToolCall(ctx context.Context, exec *executor, tc llmToolCall) llmChatMessage {
	out := llmChatMessage{
		Role:       "tool",
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
	}
	var args map[string]any
	if raw := strings.TrimSpace(tc.Function.Arguments); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			out.Content = fmt.Sprintf(`{"ok":false,"error":"failed to parse arguments: %s"}`, err.Error())
			return out
		}
	}
	call := toolCall{
		Kind: "tool_call",
		ID:   tc.ID,
		Tool: tc.Function.Name,
		Args: args,
	}
	res := exec.dispatch(ctx, call)

	payload := map[string]any{
		"ok":     res.OK,
		"output": res.Output,
	}
	if res.Stdout != "" {
		payload["stdout"] = truncateUTF8(res.Stdout, researchToolResultCap)
	}
	if res.Stderr != "" {
		payload["stderr"] = truncateUTF8(res.Stderr, researchToolResultCap)
	}
	if res.Error != "" {
		payload["error"] = res.Error
	}
	b, err := json.Marshal(payload)
	if err != nil {
		out.Content = `{"ok":false,"error":"serialize tool result failed"}`
		return out
	}
	out.Content = string(b)
	return out
}

func lastAssistantContent(messages []llmChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			c := strings.TrimSpace(messages[i].Content)
			if c != "" {
				return c
			}
		}
	}
	return ""
}

// commitAndPushResearch stages everything in workdir, commits with a
// custom message that includes the LLM trailer (transparency #4),
// pushes, and returns the new commit SHA + list of files added/changed
// since preHEAD. If preHEAD is empty (fresh repo), files are taken
// from the post-commit tree's added entries.
func commitAndPushResearch(ctx context.Context, workdir, preHEAD string, env researchTaskEnvelope, finalContent string, logger *researchLogger) (commitSHA string, files []string, err error) {
	if !isGitRepo(workdir) {
		return "", nil, errors.New("workdir is not a git repo (no remote configured?)")
	}
	dirty, err := workingTreeDirty(ctx, workdir)
	if err != nil {
		return "", nil, fmt.Errorf("git status: %w", err)
	}
	if !dirty && preHEAD != "" {
		// Nothing changed — research was a no-op (the LLM didn't
		// write any files). That's not necessarily a failure (a
		// "summarize" task could be entirely in-chat) but we don't
		// have anything to commit.
		logger.printf("[git] no changes to commit")
		head, _ := headSHA(ctx, workdir)
		return head, nil, nil
	}
	if dirty {
		if _, err := runGit(ctx, workdir, gitOpTimeout, "add", "-A"); err != nil {
			return "", nil, fmt.Errorf("git add -A: %w", err)
		}
		title := strings.TrimSpace(env.TaskTitle)
		if title == "" {
			title = "research output"
		}
		summary := truncateUTF8(finalContent, 1500)
		body := buildResearchCommitMessage(title, summary, env)
		args := []string{
			"-c", "user.name=" + gitTailCommitUser,
			"-c", "user.email=" + gitTailCommitMail,
			"commit", "--allow-empty-message", "-q", "-m", body,
		}
		if _, err := runGit(ctx, workdir, gitOpTimeout, args...); err != nil {
			return "", nil, fmt.Errorf("git commit: %w", err)
		}
	}

	postHEAD, err := headSHA(ctx, workdir)
	if err != nil || postHEAD == "" {
		return "", nil, fmt.Errorf("git rev-parse HEAD: %w", err)
	}

	// Files changed in the most recent commit (or the whole tree on
	// the very first commit).
	files = listFilesChangedInCommit(ctx, workdir, postHEAD, preHEAD)

	branch, berr := currentBranch(ctx, workdir)
	if berr != nil || branch == "" {
		branch = "main"
		_, _ = runGit(ctx, workdir, gitOpTimeout, "branch", "-M", branch)
	}
	if _, err := runGit(ctx, workdir, gitPushTimeout, "push", "-u", "origin", branch); err != nil {
		return postHEAD, files, fmt.Errorf("git push: %w", err)
	}
	logger.printf("[git] pushed %s on %s (%d files)", shortSHA(postHEAD), branch, len(files))
	return postHEAD, files, nil
}

func buildResearchCommitMessage(title, summary string, env researchTaskEnvelope) string {
	var b strings.Builder
	b.WriteString("research: ")
	b.WriteString(title)
	if summary != "" {
		b.WriteString("\n\n")
		b.WriteString(summary)
	}
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Polar-Research-Run: %d\n", env.ResearchRunID))
	b.WriteString(fmt.Sprintf("Polar-Research-LLM: %s/%s\n", env.LLM.Name, env.LLM.Model))
	if strings.TrimSpace(env.ProjectID) != "" {
		b.WriteString(fmt.Sprintf("Polar-Project: %s\n", env.ProjectID))
	}
	return b.String()
}

func listFilesChangedInCommit(ctx context.Context, workdir, postHEAD, preHEAD string) []string {
	var args []string
	if strings.TrimSpace(preHEAD) != "" {
		args = []string{"diff", "--name-only", preHEAD + ".." + postHEAD}
	} else {
		args = []string{"show", "--name-only", "--pretty=format:", postHEAD}
	}
	out, err := runGit(ctx, workdir, gitOpTimeout, args...)
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		if s != "" {
			files = append(files, s)
		}
	}
	return files
}

// ---- HTTP callbacks back to dock --------------------------------

func postResearchStart(ctx context.Context, cfg AgentConfig, runID int64) error {
	endpoint, err := researchCallbackURL(cfg.Server, fmt.Sprintf("/api/projects/research/runs/%d/start", runID))
	if err != nil {
		return err
	}
	return postJSON(ctx, cfg.Token, endpoint, struct{}{}, nil)
}

func postResearchResult(ctx context.Context, cfg AgentConfig, runID int64, res researchResult) error {
	endpoint, err := researchCallbackURL(cfg.Server, fmt.Sprintf("/api/projects/research/runs/%d/result", runID))
	if err != nil {
		return err
	}
	return postJSON(ctx, cfg.Token, endpoint, res, nil)
}

func researchCallbackURL(server, path string) (string, error) {
	u, err := url.Parse(strings.TrimRight(server, "/") + path)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	return u.String(), nil
}

func postJSON(ctx context.Context, token, endpoint string, body any, into any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncateForErr(string(respBody)))
	}
	if into != nil && len(respBody) > 0 {
		_ = json.Unmarshal(respBody, into)
	}
	return nil
}

// ---- in-process logger -----------------------------------------

type researchLogger struct {
	buf bytes.Buffer
}

func newResearchLogger() *researchLogger { return &researchLogger{} }

func (l *researchLogger) printf(format string, args ...any) {
	line := fmt.Sprintf(time.Now().Format("15:04:05 ")+format+"\n", args...)
	log.Print(strings.TrimRight(line, "\n"))
	l.buf.WriteString(line)
}

func (l *researchLogger) tail(maxBytes int) string {
	s := l.buf.String()
	if len(s) <= maxBytes {
		return s
	}
	return "...[truncated]\n" + s[len(s)-maxBytes:]
}

func (l *researchLogger) flushTo(_ io.Writer) {} // no-op; we already log.Print as we go

// truncateUTF8 caps a string to roughly n bytes without slicing inside
// a multi-byte rune. Cheap and good enough for log + summary clipping.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "...[truncated]"
}
