package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// jcodeBackend runs Jcode as an ACP v1 agent through `jcode acp`, the
// upstream stdio adapter that is backed by the persistent `jcode serve`
// daemon. Unlike the other ACP backends, the per-task process is only a shim:
// the session — transcript, provider state, the running turn — lives in the
// shared daemon, survives the shim's death, and is reattached with
// session/resume. The full transport decision and the live-probe evidence
// behind every claim in this file are recorded in
// docs/adr/ADR-JCODE-TRANSPORT.md.
//
// Consequences of the shim/daemon split that shape this backend:
//
//   - The daemon's identity is its runtime dir + socket. Task execution is
//     pinned to a Multica-scoped pair (JCODE_RUNTIME_DIR / JCODE_SOCKET,
//     defaults under ~/.multica/jcode) so tasks never collide with the user's
//     interactive jcode servers: the daemon holds a single-instance lock per
//     runtime dir, and reusing the user's would race their TUI. `jcode acp`
//     autostarts the daemon on a dead socket under a `<socket>.spawning`
//     lock, so concurrent task starts cannot double-spawn.
//   - Killing the shim does NOT stop the running turn — sessions are
//     detachable by design. Cancellation therefore sends `session/cancel`
//     (verified to interrupt in ~60ms) and waits a bounded interrupt window
//     for the prompt response before tearing the shim down; an unacknowledged
//     cancel logs the session id as potentially orphaned. The shared server
//     is never killed here.
//   - `session/resume` restores a session without replaying its history
//     (session/load replays every retained message as session/update
//     notifications). Resuming an UNKNOWN id silently attaches a fresh,
//     empty-context session under that id — verified live, no error, and the
//     next prompt runs — so this backend cannot detect a refused resume and
//     is listed in resumeRejectionUndetectable.
//   - `mcpServers` on session/new is validated as an array and then ignored
//     (upstream keeps session-scoped MCP as a TODO). MCP for jcode is
//     operator-side (~/.jcode/mcp.json), so providerSupportsMcpConfig hides
//     the MCP tab and a stale mcp_config only warns.
//   - Model selection works per session (`session/set_model`, verified), and
//     the reasoning-effort dial is the `reasoning_effort` config option that
//     applyACPEffortOption already drives — jcode genuinely threads it into
//     the provider request (GH #6720; see acpCatalogThinkingProviders).
//   - Tool calls run unattended: `jcode acp` never issues
//     session/request_permission; tool policy belongs to the daemon config.
type jcodeBackend struct {
	cfg Config
}

var (
	jcodeReaderDrainGrace      = 2 * time.Second
	jcodeNotificationQuietTime = 250 * time.Millisecond
)

// jcodeDefaultInterruptTimeout bounds the graceful session/cancel round-trip
// after Multica cancels a task, before the shim is torn down. Interrupts
// answered in ~60ms in live probes; the window is generous because an
// in-flight tool may finish its current step before the daemon acknowledges.
var jcodeDefaultInterruptTimeout = 10 * time.Second

// jcodeDefaultMaxConcurrent is the default per-server session slot count.
// Each active Multica task holds one slot for its whole execution.
const jcodeDefaultMaxConcurrent = 4

// jcodeMaxSocketPathLen keeps configured socket paths under the kernel's
// sockaddr_un limit (~104 bytes on macOS, 108 on Linux) with headroom for the
// `<socket>.spawning` lock suffix jcode appends. Verified failure mode: the
// spawned server exits with "path must be shorter than SUN_LEN".
const jcodeMaxSocketPathLen = 90

// jcodeBlockedArgs protects the ACP transport and the Multica-scoped server
// pinning from custom_args. Subcommand tokens would abort `jcode acp` at
// argument parsing; `--socket` / `--remote-working-dir` would re-route the
// session away from the socket the concurrency slots and isolation are keyed
// on; `--resume` would hijack session selection from the daemon's resume
// pointer. Provider/model flags (`-p`, `--model`, `--provider-profile`) stay
// allowed on purpose — they are jcode's supported per-launch selection knobs.
var jcodeBlockedArgs = map[string]blockedArgMode{
	"acp":                  blockedStandalone,
	"serve":                blockedStandalone,
	"server":               blockedStandalone,
	"login":                blockedStandalone,
	"repl":                 blockedStandalone,
	"run":                  blockedStandalone,
	"debug":                blockedStandalone,
	"api-bridge":           blockedStandalone,
	"--help":               blockedStandalone,
	"-h":                   blockedStandalone,
	"--socket":             blockedWithValue,
	"--remote-working-dir": blockedWithValue,
	"--resume":             blockedWithValue,
}

// jcodeRuntimeDir resolves the Multica-scoped jcode runtime directory: the
// directory whose single-instance lock defines WHICH persistent server task
// execution belongs to. MULTICA_JCODE_RUNTIME_DIR overrides; the default
// lives under the user's home so it is stable across reboots (macOS TMPDIR—
// jcode's own default—is per-boot, which would strand resume pointers).
func jcodeRuntimeDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("MULTICA_JCODE_RUNTIME_DIR")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for jcode runtime dir: %w", err)
	}
	return filepath.Join(home, ".multica", "jcode"), nil
}

// jcodeSocketPath resolves the server socket the shim (and its autostarted
// server) must use. MULTICA_JCODE_SOCKET overrides; otherwise it derives from
// the runtime dir the way jcode itself does (<runtime-dir>/jcode.sock).
func jcodeSocketPath() (socket, runtimeDir string, err error) {
	runtimeDir, err = jcodeRuntimeDir()
	if err != nil {
		return "", "", err
	}
	if s := strings.TrimSpace(os.Getenv("MULTICA_JCODE_SOCKET")); s != "" {
		return s, runtimeDir, nil
	}
	return filepath.Join(runtimeDir, "jcode.sock"), runtimeDir, nil
}

func jcodeAutostartEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MULTICA_JCODE_AUTOSTART"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func jcodeMaxConcurrent(logger interface{ Warn(string, ...any) }) int {
	raw := strings.TrimSpace(os.Getenv("MULTICA_JCODE_MAX_CONCURRENT"))
	if raw == "" {
		return jcodeDefaultMaxConcurrent
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		if logger != nil {
			logger.Warn("invalid MULTICA_JCODE_MAX_CONCURRENT; using default",
				"value", raw, "default", jcodeDefaultMaxConcurrent)
		}
		return jcodeDefaultMaxConcurrent
	}
	return n
}

func jcodeInterruptTimeout(logger interface{ Warn(string, ...any) }) time.Duration {
	raw := strings.TrimSpace(os.Getenv("MULTICA_JCODE_INTERRUPT_TIMEOUT"))
	if raw == "" {
		return jcodeDefaultInterruptTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		if logger != nil {
			logger.Warn("invalid MULTICA_JCODE_INTERRUPT_TIMEOUT; using default",
				"value", raw, "default", jcodeDefaultInterruptTimeout.String())
		}
		return jcodeDefaultInterruptTimeout
	}
	return d
}

// jcodeSlotTable is the provider-level concurrency limiter: one semaphore per
// server socket, shared by every task the daemon runs against that server.
// The size is fixed when a socket's semaphore is first created — resizing a
// live semaphore cannot be done without losing count of held slots, so a
// changed MULTICA_JCODE_MAX_CONCURRENT takes effect on daemon restart.
type jcodeSlotTable struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

var jcodeSlots = &jcodeSlotTable{
	slots: map[string]chan struct{}{},
}

// acquire blocks until a session slot for socket is free or ctx is done.
// The returned release function is idempotent-unsafe by design (call once);
// callers hold the slot for the whole task execution.
func (t *jcodeSlotTable) acquire(ctx context.Context, socket string, size int) (release func(), err error) {
	t.mu.Lock()
	sem, ok := t.slots[socket]
	if !ok {
		sem = make(chan struct{}, size)
		t.slots[socket] = sem
	}
	t.mu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// jcodeProbeSocket reports whether a jcode server is accepting connections on
// socket. Used only when autostart is disabled: with autostart on, the shim's
// own spawn-lock path is the authority and a pre-flight dial would just race
// it.
var jcodeProbeSocket = func(socket string, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// jcodeNotify writes a JSON-RPC notification (no id, so no response is
// expected or tracked) on the shim's stdin. session/cancel is the one
// notification this backend sends.
func jcodeNotify(c *hermesClient, method string, params any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.writeLine(append(data, '\n'))
}

// jcodeResumeSupported mirrors zeroclawResumeSupported: session/resume is
// gated on the capability the runtime actually advertises rather than a
// version guess.
func jcodeResumeSupported(result json.RawMessage) bool {
	var r struct {
		AgentCapabilities struct {
			SessionCapabilities struct {
				Resume *struct{} `json:"resume"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	return json.Unmarshal(result, &r) == nil && r.AgentCapabilities.SessionCapabilities.Resume != nil
}

func (b *jcodeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "jcode"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("jcode executable not found at %q: %w", execPath, err)
	}

	socket, runtimeDir, err := jcodeSocketPath()
	if err != nil {
		return nil, fmt.Errorf("jcode: %w", err)
	}
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("jcode: socket path must be absolute: %q", socket)
	}
	if len(socket) > jcodeMaxSocketPathLen {
		return nil, fmt.Errorf("jcode: socket path %q is %d bytes; unix sockets are limited to ~104 — set MULTICA_JCODE_SOCKET (or MULTICA_JCODE_RUNTIME_DIR) to a shorter path", socket, len(socket))
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("jcode: create runtime dir %q: %w", runtimeDir, err)
	}
	if !jcodeAutostartEnabled() {
		if err := jcodeProbeSocket(socket, 2*time.Second); err != nil {
			return nil, fmt.Errorf("jcode server unavailable at %q and autostart is disabled (MULTICA_JCODE_AUTOSTART): %w", socket, err)
		}
	}

	if len(opts.McpConfig) > 0 {
		// Verified against jcode v0.80: session/new validates mcpServers as an
		// array and then ignores it (session-scoped MCP is an upstream TODO).
		// MCP for jcode lives in the operator's own jcode config.
		// providerSupportsMcpConfig hides the MCP tab for this runtime, so
		// reaching here means a value saved before that — warn and continue
		// rather than bricking the task over config we cannot honour.
		b.cfg.Logger.Warn("jcode ignores MCP servers supplied by Multica; configure MCP in jcode's own config instead (~/.jcode/mcp.json)",
			"backend", "jcode",
		)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	// One slot per active task against this server, held for the whole
	// execution. Waiting respects cancellation/timeout.
	release, err := jcodeSlots.acquire(runCtx, socket, jcodeMaxConcurrent(b.cfg.Logger))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("jcode: waiting for a session slot: %w", err)
	}

	args := []string{"acp", "--no-update", "--quiet", "--no-selfdev"}
	args = append(args, filterCustomArgs(opts.ExtraArgs, jcodeBlockedArgs, b.cfg.Logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, jcodeBlockedArgs, b.cfg.Logger)...)
	cmd := b.cfg.commandAt(execPath).exec(runCtx, args...)
	hideAgentWindow(cmd)
	// runCtx expiry must NOT kill the shim outright: the goroutine below owns
	// escalation so a graceful session/cancel can round-trip first. WaitDelay
	// is the final backstop that unblocks cmd.Wait if everything else fails.
	cmd.Cancel = func() error { return nil }
	interruptTimeout := jcodeInterruptTimeout(b.cfg.Logger)
	cmd.WaitDelay = interruptTimeout + jcodeReaderDrainGrace
	b.cfg.logAgentCommand(cmd, newAgentCommandLogArgs(args, trustAgentCommandPositional(0, "acp")))
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	env := buildEnv(b.cfg.Env)
	// Pin the shim (and any server it autostarts) to the Multica-scoped
	// persistent server. Appended last so nothing in cfg.Env or the caller's
	// environment can re-route task execution to another server.
	env = append(env,
		"JCODE_RUNTIME_DIR="+runtimeDir,
		"JCODE_SOCKET="+socket,
	)
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		release()
		cancel()
		return nil, fmt.Errorf("jcode stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		release()
		cancel()
		return nil, fmt.Errorf("jcode stdin pipe: %w", err)
	}
	// StderrPipe + explicit copier for a join point before the
	// failure-promotion decision — see hermes.go for the race the
	// io.MultiWriter form has with stopReason=end_turn under load.
	providerErr := newACPProviderErrorSniffer("jcode")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		release()
		cancel()
		return nil, fmt.Errorf("jcode stderr pipe: %w", err)
	}
	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		release()
		cancel()
		return nil, fmt.Errorf("start jcode: %w", err)
	}

	stderrSink := io.MultiWriter(newLogWriter(b.cfg.Logger, "[jcode:stderr] "), providerErr)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrSink, stderr)
	}()

	b.cfg.Logger.Info("jcode acp started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "socket", socket)

	msgStream := newJcodeMessageStream(256)
	resCh := make(chan Result, 1)
	var deliverable acpDeliverableTracker
	// Gate every session update on the current turn. It must default to
	// false so anything flushed during initialize / session setup — including
	// a history replay, should resume semantics ever change — is dropped
	// rather than landing in this turn's deliverable.
	var streamingCurrentTurn atomic.Bool
	promptDone := make(chan hermesPromptResult, 1)
	activity := make(chan struct{}, 1)

	c := &hermesClient{
		cfg:          b.cfg,
		stdin:        stdin,
		pending:      make(map[int]*pendingRPC),
		pendingTools: make(map[string]*pendingToolCall),
		acceptNotification: func(string) bool {
			return streamingCurrentTurn.Load()
		},
		onActivity: func() {
			select {
			case activity <- struct{}{}:
			default:
			}
		},
		onMessage: func(msg Message) {
			if !streamingCurrentTurn.Load() {
				return
			}
			if msg.Type == MessageToolUse {
				// Same snake_case re-normalisation as kimi/traecli/grok/dim so
				// the UI sees consistent tool names.
				msg.Tool = kimiToolNameFromTitle(msg.Tool)
			}
			deliverable.observe(msg)
			msgStream.send(msg)
		},
		onPromptDone: func(result hermesPromptResult) {
			if !streamingCurrentTurn.Load() {
				return
			}
			select {
			case promptDone <- result:
			default:
			}
		},
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := newAgentStreamScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			c.handleLine(line)
		}
		c.closeAllPending(fmt.Errorf("jcode process exited"))
	}()

	go func() {
		defer cancel()
		defer msgStream.close()
		defer close(resCh)
		defer release()
		defer func() {
			_ = stdin.Close()
			// The shim is stateless — every session lives in the shared
			// server — so a group SIGKILL is always safe here and never
			// affects other tasks' sessions. Any needed graceful work
			// (session/cancel) already happened before this point.
			signalProcessGroup(cmd, syscall.SIGKILL)
			_ = cmd.Wait()
			releaseProcessGroup(cmd)
		}()

		startTime := time.Now()
		finalStatus := "completed"
		var finalError string
		var sessionID string
		var resumeRejected bool
		var effectiveModel string

		initResult, err := c.request(runCtx, "initialize", map[string]any{
			"protocolVersion": 1,
			"clientInfo": map[string]any{
				"name":    "multica-agent-sdk",
				"version": "0.2.0",
			},
			"clientCapabilities": map[string]any{},
		})
		if err != nil {
			finalStatus, finalError = jcodeRequestFailure(runCtx, timeout, "initialize", err)
			resCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
			return
		}

		cwd := opts.Cwd
		if cwd == "" {
			cwd = "."
		}

		var sessionResult json.RawMessage
		if opts.ResumeSessionID != "" {
			if !jcodeResumeSupported(initResult) {
				resCh <- Result{
					Status:         "failed",
					Error:          "jcode session/resume unavailable: initialize did not advertise sessionCapabilities.resume; retry with a fresh session",
					DurationMs:     time.Since(startTime).Milliseconds(),
					ResumeRejected: true,
				}
				return
			}
			// session/resume, not session/load: both reattach the daemon
			// session, but load also replays the retained transcript back as
			// session/update notifications. NOTE: resuming an id the server no
			// longer knows silently attaches a fresh session (verified live),
			// so no rejection detection is possible on this path — see
			// resumeRejectionUndetectable.
			result, err := c.request(runCtx, "session/resume", map[string]any{
				"sessionId":  opts.ResumeSessionID,
				"cwd":        cwd,
				"mcpServers": []any{},
			})
			if err != nil {
				finalStatus, finalError = jcodeRequestFailure(runCtx, timeout, "session/resume", err)
				if finalStatus == "failed" && isACPSessionNotFound(err) {
					resumeRejected = true
				}
				resCh <- Result{
					Status:         finalStatus,
					Error:          finalError,
					DurationMs:     time.Since(startTime).Milliseconds(),
					ResumeRejected: resumeRejected,
				}
				return
			}
			sessionResult = result
			sessionID, _ = resolveResumedSessionID(opts.ResumeSessionID, result)
		} else {
			result, err := c.request(runCtx, "session/new", map[string]any{
				"cwd":        cwd,
				"mcpServers": []any{},
			})
			if err != nil {
				finalStatus, finalError = jcodeRequestFailure(runCtx, timeout, "session/new", err)
				resCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
				return
			}
			sessionResult = result
			sessionID = extractACPSessionID(result)
			if sessionID == "" {
				resCh <- Result{
					Status:     "failed",
					Error:      "jcode session/new returned no session ID",
					DurationMs: time.Since(startTime).Milliseconds(),
				}
				return
			}
		}

		c.sessionID = sessionID
		// Early pin so a cancelled run still preserves the resume pointer.
		msgStream.send(Message{Type: MessageStatus, Status: "running", SessionID: sessionID})
		b.cfg.Logger.Info("jcode session ready", "session_id", sessionID, "socket", socket)

		sessionCurrentModel := extractACPCurrentModelID(sessionResult)
		effectiveModel = sessionCurrentModel
		if opts.Model != "" && opts.Model != sessionCurrentModel {
			// Model choice MUST fail the task on error: silently running on
			// the session's default would let the user believe their pick was
			// honoured.
			if _, err := c.request(runCtx, "session/set_model", map[string]any{
				"sessionId": sessionID,
				"modelId":   opts.Model,
			}); err != nil {
				finalStatus, finalError = jcodeRequestFailure(runCtx, timeout, "session/set_model", err)
				if finalStatus == "failed" {
					finalError = fmt.Sprintf("jcode could not switch to model %q: %v", opts.Model, err)
				}
				resCh <- Result{
					Status:         finalStatus,
					Error:          finalError,
					DurationMs:     time.Since(startTime).Milliseconds(),
					SessionID:      sessionID,
					ResumeRejected: resumeRejected,
				}
				return
			}
			b.cfg.Logger.Info("jcode session model set", "model", opts.Model)
		}
		if opts.Model != "" {
			effectiveModel = opts.Model
		}

		// Effort is advertised per session as the `reasoning_effort` config
		// option and genuinely applied by jcode (GH #6720). Unlike set_model
		// this must not fail the task; the helper no-ops when the session
		// advertises no effort option. The session state stops being current
		// once set_model runs, because the effort vocabulary can be per model.
		applyACPEffortOption(runCtx, c.request, "jcode", b.cfg.Logger,
			sessionID, sessionResult, opts.ThinkingLevel, opts.Model == "" || opts.Model == sessionCurrentModel)

		userText := prompt
		if opts.SystemPrompt != "" {
			// The runtime brief is already on disk as AGENTS.md — jcode loads
			// it as session bootstrap input — so only log when a caller sends
			// an inline system prompt anyway.
			b.cfg.Logger.Debug("jcode ignoring ExecOptions.SystemPrompt; using cwd-scoped AGENTS.md", "cwd", opts.Cwd)
		}

		// A running turn survives shim death by design, so cancellation must
		// be an explicit session/cancel with a bounded wait for the prompt
		// response — the prompt request itself therefore waits on a context
		// that outlives runCtx by the interrupt window.
		promptCtx, promptCancel := context.WithCancel(context.WithoutCancel(runCtx))
		promptReturned := make(chan struct{})
		interruptDone := make(chan struct{})
		go func() {
			defer close(interruptDone)
			defer promptCancel()
			select {
			case <-runCtx.Done():
				if err := jcodeNotify(c, "session/cancel", map[string]any{"sessionId": sessionID}); err != nil {
					b.cfg.Logger.Warn("jcode session/cancel write failed", "error", err, "session_id", sessionID)
					return
				}
				select {
				case <-promptReturned:
					b.cfg.Logger.Info("jcode session interrupted", "session_id", sessionID)
				case <-time.After(interruptTimeout):
					// The spec's orphaned-session case: the task ends now, the
					// shared server stays up, and the session id is preserved
					// in the result for later inspection or cleanup.
					b.cfg.Logger.Warn("jcode session did not acknowledge interrupt within the timeout; it may still be running on the shared server",
						"session_id", sessionID,
						"interrupt_timeout", interruptTimeout.String(),
					)
				}
			case <-promptReturned:
			}
		}()

		streamingCurrentTurn.Store(true)
		_, err = c.request(promptCtx, "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "text", "text": userText},
			},
		})
		close(promptReturned)
		<-interruptDone
		if err != nil {
			finalStatus, finalError = jcodeRequestFailure(runCtx, timeout, "session/prompt", err)
			if finalStatus == "failed" {
				if opts.ResumeSessionID != "" && isACPSessionNotFound(err) {
					sessionID = ""
					resumeRejected = true
				}
			}
		} else {
			select {
			case pr := <-promptDone:
				if pr.stopReason == "cancelled" {
					// Either our interrupt above or a jcode-side cancel; the
					// runCtx error decides which status the daemon records.
					if runCtx.Err() == context.DeadlineExceeded {
						finalStatus = "timeout"
						finalError = fmt.Sprintf("jcode timed out after %s", timeout)
					} else {
						finalStatus = "aborted"
						finalError = "execution cancelled"
					}
				}
				if pr.modelID != "" {
					effectiveModel = pr.modelID
				}
				c.mergeUsage(pr.usage)
			default:
			}
			waitForACPNotificationQuiescence(runCtx, activity, readerDone, jcodeNotificationQuietTime, jcodeReaderDrainGrace)
		}

		duration := time.Since(startTime)
		b.cfg.Logger.Info("jcode finished", "pid", cmd.Process.Pid, "status", finalStatus, "session_id", sessionID, "duration", duration.Round(time.Millisecond).String())

		_ = stdin.Close()
		cancel()

		drainCtx, drainCancel := context.WithTimeout(context.Background(), jcodeReaderDrainGrace)
		select {
		case <-readerDone:
		case <-drainCtx.Done():
		}
		select {
		case <-stderrDone:
		case <-drainCtx.Done():
		}
		drainCancel()
		streamingCurrentTurn.Store(false)

		finalOutput, providerErrorOutput := deliverable.result()
		finalStatus, finalError = promoteACPResultOnProviderError(finalStatus, finalError, providerErrorOutput, providerErr)

		var usageMap map[string]TokenUsage
		if u := c.accumulatedUsage(); acpUsagePresent(u) {
			model := effectiveModel
			if model == "" {
				model = "unknown"
			}
			usageMap = map[string]TokenUsage{model: u}
		}

		resCh <- Result{
			Status:         finalStatus,
			Output:         finalOutput,
			Error:          finalError,
			DurationMs:     duration.Milliseconds(),
			SessionID:      sessionID,
			ResumeRejected: resumeRejected,
			Usage:          usageMap,
		}
	}()

	return &Session{Messages: msgStream.ch, Result: resCh}, nil
}

func jcodeRequestFailure(ctx context.Context, timeout time.Duration, operation string, err error) (string, string) {
	if ctx.Err() == context.DeadlineExceeded {
		return "timeout", fmt.Sprintf("jcode timed out during %s after %s", operation, timeout)
	}
	if ctx.Err() == context.Canceled {
		return "aborted", "execution cancelled"
	}
	return "failed", fmt.Sprintf("jcode %s failed: %v", operation, err)
}

// jcodeMessageStream serializes sends and the final close so a late stdout
// reader cannot send on a closed channel. Mirrors dim/grok/mcode/zeroclaw.
type jcodeMessageStream struct {
	ch     chan Message
	mu     sync.Mutex
	closed bool
}

func newJcodeMessageStream(size int) *jcodeMessageStream {
	return &jcodeMessageStream{ch: make(chan Message, size)}
}

func (s *jcodeMessageStream) send(msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	trySend(s.ch, msg)
}

func (s *jcodeMessageStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}
