package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// opencodeBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args.
var opencodeBlockedArgs = map[string]blockedArgMode{
	"--format": blockedWithValue, // json output format for daemon communication
}

const opencodeToolOutputFallbackBytes = 64 * 1024

// opencodeBackend implements Backend by spawning `opencode run --format json`
// and reading streaming JSON events from stdout — the same pattern as Claude.
type opencodeBackend struct {
	cfg Config
}

func (b *opencodeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "opencode"
	}
	resolved, err := exec.LookPath(execPath)
	if err != nil {
		return nil, fmt.Errorf("opencode executable not found at %q: %w", execPath, err)
	}
	if runtime.GOOS == "windows" {
		if native := resolveOpenCodeNativeFromShim(resolved, os.Stat); native != "" {
			b.cfg.Logger.Info("opencode resolved to native binary to avoid .cmd shim argv truncation", "shim", resolved, "native", native)
			resolved = native
		}
	}
	execPath = resolved

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 20 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)

	args := []string{"run", "--format", "json"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--prompt", opts.SystemPrompt)
	}
	if opts.MaxTurns > 0 {
		b.cfg.Logger.Warn("opencode does not support --max-turns; ignoring", "maxTurns", opts.MaxTurns)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--session", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.CustomArgs, opencodeBlockedArgs, b.cfg.Logger)...)
	args = append(args, prompt)

	b.writeOpenCodeMcpConfig(opts.Cwd, opts.McpConfig)

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", args)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}

	env := buildEnv(b.cfg.Env)
	// Auto-approve all tool use in daemon mode.
	env = append(env, `OPENCODE_PERMISSION={"*":"allow"}`)
	// Inject MCP config via env var so even resumed sessions discover
	// platform tools like knowledge_search.
	if len(opts.McpConfig) > 0 {
		mcpRaw, _ := json.Marshal(map[string]interface{}{"mcp": extractMcpServers(opts.McpConfig)})
		if len(mcpRaw) > 10 {
			env = append(env, "OPENCODE_CONFIG_CONTENT="+string(mcpRaw))
			b.cfg.Logger.Debug("opencode: set OPENCODE_CONFIG_CONTENT", "len", len(mcpRaw))
		} else {
			b.cfg.Logger.Debug("opencode: OPENCODE_CONFIG_CONTENT not set (mcpRaw too short)", "len", len(mcpRaw))
		}
	} else {
		b.cfg.Logger.Debug("opencode: McpConfig is empty, skipping OPENCODE_CONFIG_CONTENT")
	}
	env = normalizeOpenCodePWD(env, opts.Cwd)
	cmd.Env = env

	// Blocking exec path: use CombinedOutput instead of StdoutPipe.
	// Avoids pipe buffering issues that can cause tool_use events to be
	// invisible to the daemon's streaming parser. Controlled by
	// MULTICA_OPENCODE_BLOCKING_EXEC env var or ExecOptions.UseBlockingExec.
	if opts.UseBlockingExec {
		result, err := b.runBlocking(cmd, opts)
		if err != nil {
			cancel()
			return nil, err
		}
		msgCh := make(chan Message, 1)
		resCh := make(chan Result, 1)
		resCh <- *result
		close(resCh)
		close(msgCh)
		return &Session{Messages: msgCh, Result: resCh}, nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("opencode stdout pipe: %w", err)
	}
	cmd.Stderr = newLogWriter(b.cfg.Logger, "[opencode:stderr] ")

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start opencode: %w", err)
	}

	b.cfg.Logger.Info("opencode started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// Close stdout when the context is cancelled so the scanner unblocks.
	go func() {
		<-runCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		scanResult := b.processEvents(stdout, msgCh)

		// Wait for process exit.
		exitErr := cmd.Wait()
		duration := time.Since(startTime)

		if runCtx.Err() == context.DeadlineExceeded {
			scanResult.status = "timeout"
			scanResult.errMsg = fmt.Sprintf("opencode timed out after %s", timeout)
		} else if runCtx.Err() == context.Canceled {
			scanResult.status = "aborted"
			scanResult.errMsg = "execution cancelled"
		} else if exitErr != nil && scanResult.status == "completed" {
			scanResult.status = "failed"
			scanResult.errMsg = fmt.Sprintf("opencode exited with error: %v", exitErr)
		}

		b.cfg.Logger.Info("opencode finished", "pid", cmd.Process.Pid, "status", scanResult.status, "duration", duration.Round(time.Millisecond).String())

		// Build usage map. OpenCode doesn't report model per-step, so we
		// attribute all usage to the configured model (or "unknown").
		var usage map[string]TokenUsage
		u := scanResult.usage
		if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
			model := opts.Model
			if model == "" {
				model = "unknown"
			}
			usage = map[string]TokenUsage{model: u}
		}

		resCh <- Result{
			Status:     scanResult.status,
			Output:     scanResult.output,
			Error:      scanResult.errMsg,
			DurationMs: duration.Milliseconds(),
			SessionID:  scanResult.sessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// ── Event handlers ──

// eventResult holds the accumulated state from processing the event stream.
type eventResult struct {
	status    string
	errMsg    string
	output    string
	sessionID string
	usage     TokenUsage // accumulated token usage across all steps
}

// processEvents reads JSON lines from r, dispatches events to ch, and returns
// the accumulated result. This is the core scanner loop, extracted for testability.
func (b *opencodeBackend) processEvents(r io.Reader, ch chan<- Message) eventResult {
	var output strings.Builder
	var toolOutputFallback strings.Builder
	var sessionID string
	var usage TokenUsage
	finalStatus := "completed"
	var finalError string

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event opencodeEvent
		if err := json.Unmarshal(line, &event); err != nil {
			// First 200 chars of the offending line for debug
			preview := string(line)
			if len(preview) > 200 {
				preview = preview[:200]
			}
			b.cfg.Logger.Debug("opencode: skip non-JSON line", "preview", preview)
			continue
		}
		if event.SessionID != "" {
			sessionID = event.SessionID
		}
		switch event.Type {
		case "text":
			b.handleTextEvent(event, ch, &output)
		case "tool_use":
			b.cfg.Logger.Debug("opencode: received tool_use event", "tool", event.Part.Tool)
			b.captureToolOutputFallback(event, &toolOutputFallback)
			b.handleToolUseEvent(event, ch)
		case "error":
			b.handleErrorEvent(event, ch, &finalStatus, &finalError)
		case "step_start":
			trySend(ch, Message{Type: MessageStatus, Status: "running"})
		case "step_finish":
			// Accumulate token usage from step_finish events.
			if t := event.Part.Tokens; t != nil {
				usage.InputTokens += t.Input
				usage.OutputTokens += t.Output
				if t.Cache != nil {
					usage.CacheReadTokens += t.Cache.Read
					usage.CacheWriteTokens += t.Cache.Write
				}
			}
		}
	}

	// Check for scanner errors (e.g. broken pipe, read errors).
	if scanErr := scanner.Err(); scanErr != nil {
		b.cfg.Logger.Warn("opencode stdout scanner error", "error", scanErr)
		if finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("stdout read error: %v", scanErr)
		}
	}

	finalOutput := output.String()
	if finalOutput == "" {
		finalOutput = toolOutputFallback.String()
		if finalOutput != "" {
			trySend(ch, Message{Type: MessageText, Content: finalOutput})
		}
	}

	return eventResult{
		status:    finalStatus,
		errMsg:    finalError,
		output:    finalOutput,
		sessionID: sessionID,
		usage:     usage,
	}
}

func (b *opencodeBackend) handleTextEvent(event opencodeEvent, ch chan<- Message, output *strings.Builder) {
	text := event.Part.Text
	if text != "" {
		output.WriteString(text)
		trySend(ch, Message{Type: MessageText, Content: text})
	}
}

// handleToolUseEvent processes "tool_use" events from opencode. A single
// tool_use event contains both the call and result in part.state when the
// tool has completed (state.status == "completed").
func (b *opencodeBackend) handleToolUseEvent(event opencodeEvent, ch chan<- Message) {
	// Extract input from state.input (the tool invocation parameters).
	var input map[string]any
	if event.Part.State != nil && event.Part.State.Input != nil {
		_ = json.Unmarshal(event.Part.State.Input, &input)
	}

	// Emit the tool-use message.
	trySend(ch, Message{
		Type:   MessageToolUse,
		Tool:   event.Part.Tool,
		CallID: event.Part.CallID,
		Input:  input,
	})

	// If the tool has completed, also emit a tool-result message.
	if event.Part.State != nil && event.Part.State.Status == "completed" {
		outputStr := extractToolOutput(event.Part.State.Output)
		trySend(ch, Message{
			Type:   MessageToolResult,
			Tool:   event.Part.Tool,
			CallID: event.Part.CallID,
			Output: outputStr,
		})
	}
}

func (b *opencodeBackend) captureToolOutputFallback(event opencodeEvent, fallback *strings.Builder) {
	if event.Part.State == nil || event.Part.State.Status != "completed" {
		return
	}
	outputStr := strings.TrimSpace(extractToolOutput(event.Part.State.Output))
	if outputStr == "" || fallback.Len() >= opencodeToolOutputFallbackBytes {
		return
	}
	if fallback.Len() > 0 {
		fallback.WriteString("\n\n")
	}
	remaining := opencodeToolOutputFallbackBytes - fallback.Len()
	if len(outputStr) > remaining {
		fallback.WriteString(outputStr[:remaining])
		return
	}
	fallback.WriteString(outputStr)
}

// handleErrorEvent processes "error" events from opencode. OpenCode can exit
// with RC=0 even on errors (e.g. invalid model), so error events are the
// reliable signal for failures.
func (b *opencodeBackend) handleErrorEvent(event opencodeEvent, ch chan<- Message, finalStatus, finalError *string) {
	errMsg := ""
	if event.Error != nil {
		errMsg = event.Error.Message()
	}
	if errMsg == "" {
		errMsg = "unknown opencode error"
	}

	b.cfg.Logger.Warn("opencode error event", "error", errMsg)
	trySend(ch, Message{Type: MessageError, Content: errMsg})

	*finalStatus = "failed"
	*finalError = errMsg
}

// resolveOpenCodeNativeFromShim returns the path to the native OpenCode
// executable bundled inside the npm package, given the path to the npm
// `opencode.cmd` shim that PATH lookup found on Windows. Returns "" if shim
// doesn't end in `.cmd` or no candidate npm platform package has a bundled
// native binary present.
//
// Windows batch argument forwarding via `%*` does not preserve newlines, so
// multi-line positional argv is truncated at the first newline before the
// shim hands off to the JS entrypoint. Daemon prompts can include literal
// newlines (system prompt + user message), which makes the agent see only
// the first line. Native binary spawn skips the cmd.exe layer entirely.
//
// Layout when installed via `npm install -g opencode-ai`:
//
//	<prefix>\opencode.cmd                                                                       (shim)
//	<prefix>\node_modules\opencode-ai\node_modules\opencode-windows-{x64,x64-baseline,arm64}\bin\opencode.exe (native)
//
// `opencode-windows-x64-baseline` ships for older CPUs without AVX2;
// `opencode-windows-arm64` ships for Surface / Copilot+ PC hosts.
// Candidates are tried in GOARCH-preferred order so the most likely match
// for the current host comes first.
//
// statFn is injected so this is testable on non-Windows hosts.
func resolveOpenCodeNativeFromShim(shimPath string, statFn func(string) (os.FileInfo, error)) string {
	if !strings.EqualFold(filepath.Ext(shimPath), ".cmd") {
		return ""
	}
	prefix := filepath.Dir(shimPath)
	for _, pkg := range opencodeWindowsPackageCandidates(runtime.GOARCH) {
		candidate := filepath.Join(prefix, "node_modules", "opencode-ai", "node_modules", pkg, "bin", "opencode.exe")
		if _, err := statFn(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// opencodeWindowsPackageCandidates returns the npm platform package names
// that may host the bundled `opencode.exe` on Windows, ordered so the most
// likely match for the given GOARCH comes first. ARM64 hosts try the arm64
// build first; everything else tries x64, then the baseline x64 build for
// older CPUs without AVX2, then arm64 as a final fallback. Cost is one
// extra statFn call per miss when the GOARCH-preferred package isn't
// installed.
func opencodeWindowsPackageCandidates(goarch string) []string {
	switch goarch {
	case "arm64":
		return []string{"opencode-windows-arm64", "opencode-windows-x64", "opencode-windows-x64-baseline"}
	default:
		return []string{"opencode-windows-x64", "opencode-windows-x64-baseline", "opencode-windows-arm64"}
	}
}

// extractToolOutput converts the tool state output (which may be a string or
// structured object) into a string.
func extractToolOutput(output any) string {
	if output == nil {
		return ""
	}
	if s, ok := output.(string); ok {
		return s
	}
	data, _ := json.Marshal(output)
	return string(data)
}

// ── JSON types for `opencode run --format json` stdout events ──

// opencodeEvent represents a single JSON line from `opencode run --format json`.
//
// Event types observed in real output:
//
//	"step_start"  — agent step begins
//	"text"        — text output from agent (part.text)
//	"tool_use"    — tool invocation with call and result (part.tool, part.callID, part.state)
//	"error"       — error from opencode (error.name, error.data.message)
//	"step_finish" — agent step completes (includes token usage)
type opencodeEvent struct {
	Type      string            `json:"type"`
	Timestamp int64             `json:"timestamp,omitempty"`
	SessionID string            `json:"sessionID,omitempty"`
	Part      opencodeEventPart `json:"part"`
	Error     *opencodeError    `json:"error,omitempty"`
}

// opencodeEventPart represents the part field in an opencode event.
type opencodeEventPart struct {
	ID        string `json:"id,omitempty"`
	MessageID string `json:"messageID,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	Type      string `json:"type,omitempty"`

	// Text events
	Text string `json:"text,omitempty"`

	// Tool use events
	Tool   string             `json:"tool,omitempty"`
	CallID string             `json:"callID,omitempty"`
	State  *opencodeToolState `json:"state,omitempty"`

	// step_finish token usage
	Tokens *opencodeTokens `json:"tokens,omitempty"`
}

// opencodeTokens represents token usage in a step_finish event.
type opencodeTokens struct {
	Input  int64                `json:"input"`
	Output int64                `json:"output"`
	Cache  *opencodeCacheTokens `json:"cache,omitempty"`
}

type opencodeCacheTokens struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

// opencodeToolState represents the state of a tool invocation.
type opencodeToolState struct {
	Status string          `json:"status,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output any             `json:"output,omitempty"`
}

// opencodeError represents an error event from opencode.
type opencodeError struct {
	Name string           `json:"name,omitempty"`
	Data *opencodeErrData `json:"data,omitempty"`
}

// Message returns the human-readable error message.
func (e *opencodeError) Message() string {
	if e.Data != nil && e.Data.Message != "" {
		return e.Data.Message
	}
	if e.Name != "" {
		return e.Name
	}
	return ""
}

type opencodeErrData struct {
	Message string `json:"message,omitempty"`
}

// extractMcpServers extracts the MCP servers map from a json.RawMessage
// containing {"mcpServers": {...}} and converts to OpenCode format:
// {"name": {"type": "local", "command": [...], "enabled": true, ...}}.
func extractMcpServers(raw json.RawMessage) map[string]interface{} {
	var wrapper map[string]interface{}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil
	}
	mcpServers, ok := wrapper["mcpServers"]
	if !ok {
		return nil
	}
	servers, ok := mcpServers.(map[string]interface{})
	if !ok {
		return nil
	}
	result := make(map[string]interface{})
	for name, srv := range servers {
		srvMap, ok := srv.(map[string]interface{})
		if !ok {
			continue
		}
		entry := map[string]interface{}{"type": "local", "enabled": true}
		if cmd, ok := srvMap["command"].(string); ok {
			var args []string
			if a, ok := srvMap["args"].([]interface{}); ok {
				for _, arg := range a {
					if s, ok := arg.(string); ok {
						args = append(args, s)
					}
				}
			}
			entry["command"] = append([]string{cmd}, args...)
		}
		if env, ok := srvMap["env"]; ok {
			entry["environment"] = env
		}
		result[name] = entry
	}
	return result
}

func (b *opencodeBackend) writeOpenCodeMcpConfig(cwd string, mcpConfig json.RawMessage) {
	if len(mcpConfig) == 0 {
		return
	}
	opencodeMcp := extractMcpServers(mcpConfig)
	if len(opencodeMcp) == 0 {
		return
	}
	opencodeDir := filepath.Join(cwd, ".opencode")
	if err := os.MkdirAll(opencodeDir, 0755); err != nil {
		b.cfg.Logger.Warn("opencode: failed to create .opencode dir for MCP config", "error", err)
		return
	}
	cfg := map[string]interface{}{"mcp": opencodeMcp}
	raw, err := json.Marshal(cfg)
	if err != nil {
		b.cfg.Logger.Warn("opencode: failed to marshal MCP config", "error", err)
		return
	}
	configPath := filepath.Join(opencodeDir, "opencode.json")
	if err := os.WriteFile(configPath, raw, 0644); err != nil {
		b.cfg.Logger.Warn("opencode: failed to write MCP config", "error", err)
		return
	}
	b.cfg.Logger.Debug("opencode: wrote per-task MCP config", "path", configPath)
}

func normalizeOpenCodePWD(env []string, cwd string) []string {
	if cwd == "" {
		return env
	}
	// OpenCode trusts PWD over getcwd(). If Go's cmd.Dir changes cwd while PWD
	// still points at the daemon's launch dir, `opencode run` exits 0 with no
	// stdout/stderr. Keep the logical and actual cwd in sync for the child.
	out := make([]string, 0, len(env)+1)
	found := false
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key == "PWD" {
			if !found {
				out = append(out, "PWD="+cwd)
				found = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, "PWD="+cwd)
	}
	return out
}

// runBlocking runs opencode via CombinedOutput — no pipes, no goroutines,
// no streaming. Parses all output after the process exits. Slower but
// avoids the StdoutPipe buffering / event-loss issues seen with MCP tools.
func (b *opencodeBackend) runBlocking(cmd *exec.Cmd, opts ExecOptions) (*Result, error) {
	start := time.Now()
	raw, err := cmd.CombinedOutput()
	dur := time.Since(start)

	var stderr string
	var exitErr *exec.ExitError
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitErr = ee
			stderr = string(ee.Stderr)
		} else {
			return nil, fmt.Errorf("opencode: %w", err)
		}
	}

	b.cfg.Logger.Info("opencode [blocking] finished", "duration", dur.Round(time.Millisecond).String(), "bytes", len(raw), "exitError", exitErr)
	if len(raw) == 0 {
		// Diagnostic: run a simple test to verify CombinedOutput works from this process
		testCmd := exec.Command("echo", "hello_from_daemon")
		testOut, _ := testCmd.CombinedOutput()
		b.cfg.Logger.Warn("opencode [blocking] produced zero output",
			"dir", cmd.Dir,
			"envCount", len(cmd.Env),
			"canExec", len(testOut) > 0,
		)
	}

	// Parse events from raw output (same format as streaming parser)
	lines := strings.Split(string(raw), "\n")
	var output strings.Builder
	var toolCount int
	var usage TokenUsage
	var sessionID string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event opencodeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		switch event.Type {
		case "text":
			output.WriteString(event.Part.Text)
			if event.SessionID != "" {
				sessionID = event.SessionID
			}
		case "tool_use":
			toolCount++
		case "step_finish":
			if event.Part.Tokens != nil {
				usage.InputTokens += event.Part.Tokens.Input
				usage.OutputTokens += event.Part.Tokens.Output
				usage.CacheReadTokens += event.Part.Tokens.Cache.Read
				usage.CacheWriteTokens += event.Part.Tokens.Cache.Write
			}
		}
	}

	status := "completed"
	errMsg := ""
	if exitErr != nil {
		status = "failed"
		errMsg = fmt.Sprintf("opencode exited with error: %v", exitErr)
		if stderr != "" {
			errMsg += "; stderr: " + stderr
		}
	}

	var u map[string]TokenUsage
	if usage.InputTokens > 0 || usage.OutputTokens > 0 {
		model := opts.Model
		if model == "" {
			model = "unknown"
		}
		u = map[string]TokenUsage{model: usage}
	}

	return &Result{
		Status:     status,
		Output:     output.String(),
		Error:      errMsg,
		ToolCount:  toolCount,
		DurationMs: dur.Milliseconds(),
		SessionID:  sessionID,
		Usage:      u,
	}, nil
}
