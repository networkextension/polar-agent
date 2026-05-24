package main

// `polar-agent research` — local stdin REPL that drives the same
// LLM tool-loop the dock-pushed research_task path uses, except:
//
//   - LLM config comes from CLI flags / env (no dock involvement)
//   - prompts come from stdin (multi-line, blank line submits)
//   - no callbacks to dock; results print to stdout
//   - optional auto git commit + push per turn (when --git-remote set)
//
// Useful when an operator wants the same Aider-style agent on a
// laptop without dock in the loop — for one-off research, drafts,
// or when iterating on a prompt before dispatching the real run.
//
// LLM transparency: the model name + id is printed before the first
// turn (pre-action) AND included in any auto-commit trailer
// (artifact). Two of the four touchpoints from the dock-driven
// dispatch — chat-thread + DB columns aren't applicable here.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runResearchREPL(args []string) error {
	fs := flag.NewFlagSet("research", flag.ContinueOnError)
	workdir := fs.String("workdir", ".", "working directory for the LLM (where read_file/write_file/run_cmd operate)")
	llmBaseURL := fs.String("llm-base-url", os.Getenv("POLAR_LLM_BASE_URL"), "LLM base URL (OpenAI-compatible /chat/completions endpoint prefix). Falls back to $POLAR_LLM_BASE_URL")
	llmAPIKey := fs.String("llm-api-key", os.Getenv("POLAR_LLM_API_KEY"), "LLM API key. Falls back to $POLAR_LLM_API_KEY")
	llmModel := fs.String("llm-model", os.Getenv("POLAR_LLM_MODEL"), "LLM model id. Falls back to $POLAR_LLM_MODEL")
	llmName := fs.String("llm-name", os.Getenv("POLAR_LLM_NAME"), "Display name for the LLM (for the pre-action banner + commit trailer). Falls back to $POLAR_LLM_NAME or the model id")
	gitRemote := fs.String("git-remote", "", "git remote URL. When set, each turn auto-commits + pushes the LLM's file changes")
	initialPrompt := fs.String("prompt", "", "initial prompt; if empty, read the first prompt from stdin")
	verbose := fs.Bool("verbose", false, "log each tool call's args + truncated result")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// flag pkg already printed usage; treat -h as success.
			return nil
		}
		return err
	}

	if strings.TrimSpace(*llmBaseURL) == "" || strings.TrimSpace(*llmAPIKey) == "" || strings.TrimSpace(*llmModel) == "" {
		return errors.New("missing required LLM config — set --llm-base-url / --llm-api-key / --llm-model (or POLAR_LLM_BASE_URL / POLAR_LLM_API_KEY / POLAR_LLM_MODEL env vars)")
	}

	wd, err := filepath.Abs(*workdir)
	if err != nil {
		return fmt.Errorf("workdir: %w", err)
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return fmt.Errorf("mkdir workdir %s: %w", wd, err)
	}

	displayName := strings.TrimSpace(*llmName)
	if displayName == "" {
		displayName = *llmModel
	}

	// Pre-action transparency banner — operator sees exactly which LLM
	// is about to do work before they type the first prompt.
	fmt.Printf("polar-agent research · LLM: %s (%s) · workdir: %s\n",
		displayName, *llmModel, wd)
	if strings.TrimSpace(*gitRemote) != "" {
		fmt.Printf("git remote: %s (auto commit + push per turn)\n", *gitRemote)
	} else {
		fmt.Println("git: disabled (no --git-remote). Files written but not committed.")
	}
	fmt.Println("Tools: read_file / write_file / list_dir / run_cmd (git+swift+xcodebuild+xcrun+ls/cat/find/grep/swiftformat/swiftlint/open).")
	fmt.Println("Type your prompt and finish with an empty line. Type :exit (or Ctrl-D) to quit.")
	fmt.Println(strings.Repeat("─", 60))

	ctx := context.Background()
	// REPL is a manual standalone tool — operator can set HTTPS_PROXY
	// via env if they need a proxy; we don't take it as a flag here to
	// keep the CLI surface tight. dockBypassURL stays empty since this
	// path isn't dock-driven.
	llm := newLLMClient(*llmBaseURL, *llmAPIKey, "", "")
	exec := newExecutor(wd, *verbose)
	logger := newResearchLogger()

	// Persistent message history so multi-turn keeps the LLM aware of
	// prior tool output, file changes, etc. Init with a system prompt
	// tuned for the local-no-dock context — most importantly: tell the
	// LLM there's no chat fallback and operator is reading stdout.
	systemPrompt := buildResearchREPLSystemPrompt(displayName, *llmModel, wd)
	messages := []llmChatMessage{{Role: "system", Content: systemPrompt}}
	tools := researchToolSpecs()

	// Pre-task git plumbing — same as dock-driven path. preHEAD is
	// re-snapshotted before each turn's commit so the per-turn diff
	// catches just that turn's changes.
	if remote := strings.TrimSpace(*gitRemote); remote != "" {
		if _, err := gitPrepare(ctx, wd, remote); err != nil {
			fmt.Printf("⚠️ git prepare failed: %v (continuing — files write to disk; push will fail until repo is fixed)\n", err)
		}
	}

	reader := bufio.NewReader(os.Stdin)
	turn := 0
	for {
		var prompt string
		if turn == 0 && strings.TrimSpace(*initialPrompt) != "" {
			prompt = strings.TrimSpace(*initialPrompt)
			fmt.Printf("> (--prompt) %s\n", truncateUTF8(prompt, 200))
		} else {
			fmt.Print("\n> ")
			line, err := readMultiline(reader)
			if err != nil {
				if errors.Is(err, errREPLExit) {
					fmt.Println("bye.")
					return nil
				}
				return err
			}
			prompt = strings.TrimSpace(line)
			if prompt == "" {
				continue
			}
		}
		turn++

		preHEAD := ""
		if strings.TrimSpace(*gitRemote) != "" {
			preHEAD, _ = headSHA(ctx, wd)
		}

		messages = append(messages, llmChatMessage{Role: "user", Content: prompt})
		iterations, _, loopErr := runResearchToolLoop(ctx, llm, exec, *llmModel, &messages, tools, logger)
		if loopErr != nil {
			fmt.Printf("⚠️ LLM loop error (iter=%d): %v\n", iterations, loopErr)
			// Don't pop the user message — leave it in history so the
			// next turn carries forward and the LLM can see what
			// happened on retry.
			continue
		}
		final := lastAssistantContent(messages)
		fmt.Println()
		fmt.Println(strings.Repeat("─", 60))
		fmt.Println(final)
		fmt.Println(strings.Repeat("─", 60))
		fmt.Printf("(iterations: %d)\n", iterations)

		// Per-turn git commit + push when remote is configured. Skipped
		// silently if nothing changed.
		if remote := strings.TrimSpace(*gitRemote); remote != "" {
			localRunID := fmt.Sprintf("local-%d", time.Now().Unix())
			env := researchTaskEnvelope{
				ProjectID:    "(local-repl)",
				TaskTitle:    truncateUTF8(prompt, 60),
				ResearchRunID: 0,
				LLM: researchLLMSnapshot{
					Name:  displayName,
					Model: *llmModel,
				},
			}
			_ = localRunID
			commitSHA, files, gerr := commitAndPushResearch(ctx, wd, preHEAD, env, final, logger)
			if gerr != nil {
				fmt.Printf("⚠️ git: %v\n", gerr)
			} else if commitSHA != "" && len(files) > 0 {
				fmt.Printf("✅ committed %s · %d files: %s\n", shortSHA(commitSHA), len(files), strings.Join(files, ", "))
			} else {
				fmt.Println("(no file changes this turn)")
			}
		}
	}
}

func buildResearchREPLSystemPrompt(llmName, model, workdir string) string {
	return fmt.Sprintf(`你是研究助理，运行在 polar-agent 的本地 stdin REPL 模式下。规则：

- 当前 LLM: %s (%s)。这是平台告诉操作员的，不要伪装成别的模型。
- 工作目录: %s（read_file/write_file 路径都是相对该目录）。
- 你没有联网工具（没有 web_fetch / web_search）。基于自身知识 + 用户给出的资料完成；事实存疑一定要在产物里标 "[需核实]"。
- 操作员通过 stdout 看到你的最终回复 — 要点直接回到聊天里，但产物（markdown / 设计稿 / 报告）请用 write_file 落到工作目录，不要把正文塞进 chat 回复里。
- 多轮对话：每个用户输入是一个独立 turn，但你的工具调用历史在多 turn 间持续，可以引用之前写的文件。
- 不要主动调 git push — 如果 polar-agent 配了 --git-remote，它会在每个 turn 结束后自动 commit + push。
- 工具异常（路径越权 / 命令未授权 / 写盘失败）会以 error 字段返回，请读 stderr 自己处理 — 别盲目重试相同调用。`,
		llmName, model, workdir)
}

var errREPLExit = errors.New("repl exit requested")

// readMultiline reads lines until either an empty line (submit) or
// EOF / `:exit` (terminate). Returns the joined non-empty lines.
func readMultiline(r *bufio.Reader) (string, error) {
	var lines []string
	first := true
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			// EOF on the very first read = clean exit. EOF mid-prompt =
			// submit what we have.
			if first {
				return "", errREPLExit
			}
			return strings.Join(lines, "\n"), nil
		}
		first = false
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(trimmed) == ":exit" {
			return "", errREPLExit
		}
		if trimmed == "" {
			if len(lines) == 0 {
				// double-blank doesn't exit — let the caller loop and
				// re-prompt.
				return "", nil
			}
			return strings.Join(lines, "\n"), nil
		}
		lines = append(lines, trimmed)
	}
}
