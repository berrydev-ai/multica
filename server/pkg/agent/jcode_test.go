package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setJcodeTestRuntimeDir pins the Multica-scoped jcode server identity to a
// short throwaway directory. Short matters: the backend validates the derived
// socket path against jcodeMaxSocketPathLen, and t.TempDir on macOS routinely
// exceeds it. No fake in this file ever binds the socket — the path only has
// to validate and the directory only has to be creatable.
func setJcodeTestRuntimeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mjc")
	if err != nil {
		t.Fatalf("mkdir short runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("MULTICA_JCODE_RUNTIME_DIR", dir)
	return dir
}

// writeFakeJcodeACP writes a POSIX-sh fake of `jcode acp` speaking the ACP
// dialect captured from jcode 0.80.x (see docs/adr/ADR-JCODE-TRANSPORT.md):
// initialize advertises sessionCapabilities.resume, session/new returns a
// sessionId plus a models block, session/resume replays nothing but answers
// only configOptions/models (no sessionId), and session/prompt carries usage
// on its result.
//
// Behavior toggles (all optional):
//   - resumeCapability=false drops sessionCapabilities from initialize.
//   - failSetModel makes session/set_model answer a JSON-RPC error.
//   - hangPrompt makes session/prompt stay silent until a session/cancel
//     notification arrives (answering it with stopReason=cancelled), which is
//     exactly the graceful-interrupt contract the backend relies on.
//   - ignoreCancel additionally ignores session/cancel, for the
//     orphaned-session timeout path.
type fakeJcodeOptions struct {
	resumeCapability bool
	failSetModel     bool
	hangPrompt       bool
	ignoreCancel     bool
}

func writeFakeJcodeACP(t *testing.T, opts fakeJcodeOptions) (bin, requests, envDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "jcode")
	requests = filepath.Join(dir, "requests.jsonl")
	envDump = filepath.Join(dir, "env.txt")

	caps := `"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"close":{},"resume":{}},"mcpCapabilities":{"http":false,"sse":false}}`
	if !opts.resumeCapability {
		caps = `"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":false,"sse":false}}`
	}
	setModel := `printf '{"jsonrpc":"2.0","id":%s,"result":{"configOptions":[],"models":{"currentModelId":"gpt-fake","availableModels":[]}}}\n' "$id"`
	if opts.failSetModel {
		setModel = `printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"model not available"}}\n' "$id"`
	}
	prompt := `printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session_fake_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pong"}}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"inputTokens":305,"outputTokens":4,"cachedReadTokens":100,"cachedWriteTokens":200,"totalTokens":609}}}\n' "$id"`
	if opts.hangPrompt {
		prompt = `PROMPT_ID=$id`
	}
	cancel := `printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"cancelled"}}\n' "$PROMPT_ID"`
	if opts.ignoreCancel {
		cancel = `:`
	}

	script := fmt.Sprintf(`#!/bin/sh
printf 'JCODE_RUNTIME_DIR=%%s\nJCODE_SOCKET=%%s\n' "$JCODE_RUNTIME_DIR" "$JCODE_SOCKET" > %q
PROMPT_ID=""
while IFS= read -r line; do
  printf '%%s\n' "$line" >> %q
  id=$(printf '%%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":1,"agentInfo":{"name":"jcode","title":"Jcode","version":"0.80.1"},"authMethods":[],%s}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"sessionId":"session_fake_1","models":{"currentModelId":"claude-fake-1","availableModels":[{"modelId":"claude-fake-1","name":"claude-fake-1"}]},"configOptions":[]}}\n' "$id"
      ;;
    *'"method":"session/resume"'*)
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session_fake_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"STALE-REPLAY"}}}}\n'
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"configOptions":[],"models":{"currentModelId":"claude-fake-1","availableModels":[]}}}\n' "$id"
      ;;
    *'"method":"session/set_model"'*)
      %s
      ;;
    *'"method":"session/cancel"'*)
      %s
      ;;
    *'"method":"session/prompt"'*)
      %s
      ;;
    *'"method":"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`, envDump, requests, caps, setModel, cancel, prompt)
	writeTestExecutable(t, bin, []byte(script))
	return bin, requests, envDump
}

func runJcodeSession(t *testing.T, bin string, ctx context.Context, prompt string, opts ExecOptions) (Result, []Message) {
	t.Helper()
	backend, err := New("jcode", Config{ExecutablePath: bin, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new jcode backend: %v", err)
	}
	session, err := backend.Execute(ctx, prompt, opts)
	if err != nil {
		t.Fatalf("execute jcode: %v", err)
	}
	var streamed []Message
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range session.Messages {
			streamed = append(streamed, msg)
		}
	}()
	select {
	case res := <-session.Result:
		<-done
		return res, streamed
	case <-time.After(30 * time.Second):
		t.Fatalf("jcode session did not finish")
		return Result{}, nil
	}
}

func readRequests(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read requests: %v", err)
	}
	return string(data)
}

func TestNewReturnsJcodeBackend(t *testing.T) {
	backend, err := New("jcode", Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(jcode): %v", err)
	}
	if _, ok := backend.(*jcodeBackend); !ok {
		t.Fatalf("New(jcode) = %T, want *jcodeBackend", backend)
	}
	if LaunchHeader("jcode") == "" {
		t.Fatal("LaunchHeader(jcode) is empty")
	}
}

func TestJcodeFreshSessionExecutesPromptAndPinsServerEnv(t *testing.T) {
	runtimeDir := setJcodeTestRuntimeDir(t)
	bin, requests, envDump := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true})
	workDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, streamed := runJcodeSession(t, bin, ctx, "do the thing", ExecOptions{Cwd: workDir, Timeout: 15 * time.Second})

	if res.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed", res.Status, res.Error)
	}
	if res.SessionID != "session_fake_1" {
		t.Errorf("session id = %q, want session_fake_1", res.SessionID)
	}
	if res.Output != "pong" {
		t.Errorf("output = %q, want pong", res.Output)
	}
	usage, ok := res.Usage["claude-fake-1"]
	if !ok {
		t.Fatalf("usage keys = %v, want the session's current model claude-fake-1", res.Usage)
	}
	if usage.InputTokens != 305 || usage.OutputTokens != 4 || usage.CacheReadTokens != 100 || usage.CacheWriteTokens != 200 {
		t.Errorf("usage = %+v, want 305/4/100/200", usage)
	}

	req := readRequests(t, requests)
	if !strings.Contains(req, `"method":"session/new"`) || !strings.Contains(req, fmt.Sprintf("%q", workDir)) {
		t.Errorf("session/new did not carry the task cwd:\n%s", req)
	}
	if strings.Contains(req, "session/set_model") {
		t.Errorf("set_model sent without a model override:\n%s", req)
	}

	// The early resume-pointer pin must arrive before any turn content.
	foundPin := false
	for _, msg := range streamed {
		if msg.Type == MessageStatus && msg.SessionID == "session_fake_1" {
			foundPin = true
			break
		}
	}
	if !foundPin {
		t.Errorf("no early MessageStatus session pin in stream: %+v", streamed)
	}

	// The shim (and anything it autostarts) must be pinned to the
	// Multica-scoped server, not the user's interactive one.
	env, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	wantSocket := filepath.Join(runtimeDir, "jcode.sock")
	if !strings.Contains(string(env), "JCODE_RUNTIME_DIR="+runtimeDir) || !strings.Contains(string(env), "JCODE_SOCKET="+wantSocket) {
		t.Errorf("shim env not pinned to the Multica-scoped server:\n%s", env)
	}
	if fi, err := os.Stat(runtimeDir); err != nil || !fi.IsDir() {
		t.Errorf("runtime dir was not created: %v", err)
	}
}

func TestJcodeModelSelectionSendsSetModel(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	bin, requests, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, _ := runJcodeSession(t, bin, ctx, "p", ExecOptions{Cwd: t.TempDir(), Timeout: 15 * time.Second, Model: "gpt-fake"})
	if res.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed", res.Status, res.Error)
	}
	if _, ok := res.Usage["gpt-fake"]; !ok {
		t.Errorf("usage keys = %v, want requested model gpt-fake", res.Usage)
	}
	req := readRequests(t, requests)
	if !strings.Contains(req, `"method":"session/set_model"`) || !strings.Contains(req, `"modelId":"gpt-fake"`) {
		t.Errorf("session/set_model with modelId not sent:\n%s", req)
	}
}

func TestJcodeSetModelFailureFailsTask(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	bin, _, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true, failSetModel: true})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, _ := runJcodeSession(t, bin, ctx, "p", ExecOptions{Cwd: t.TempDir(), Timeout: 15 * time.Second, Model: "gpt-fake"})
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed when set_model errors", res.Status)
	}
	if !strings.Contains(res.Error, `could not switch to model "gpt-fake"`) {
		t.Errorf("error = %q, want a could-not-switch message", res.Error)
	}
	if res.SessionID != "session_fake_1" {
		t.Errorf("session id = %q; a set_model failure must still preserve the resume pointer", res.SessionID)
	}
}

func TestJcodeResumeUsesSessionResumeWithoutReplayLeak(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	bin, requests, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, streamed := runJcodeSession(t, bin, ctx, "continue", ExecOptions{Cwd: t.TempDir(), Timeout: 15 * time.Second, ResumeSessionID: "session_fake_1"})

	if res.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed", res.Status, res.Error)
	}
	if res.SessionID != "session_fake_1" {
		t.Errorf("session id = %q, want the resumed id (resume answers no sessionId, so the requested one stands)", res.SessionID)
	}
	req := readRequests(t, requests)
	if !strings.Contains(req, `"method":"session/resume"`) {
		t.Errorf("resume must go through session/resume:\n%s", req)
	}
	if strings.Contains(req, `"method":"session/load"`) {
		t.Errorf("session/load must not be used (it replays history):\n%s", req)
	}
	if !strings.Contains(req, `"sessionId":"session_fake_1"`) {
		t.Errorf("session/resume did not carry the session id:\n%s", req)
	}
	if strings.Contains(res.Output, "STALE-REPLAY") {
		t.Errorf("replayed history leaked into Result.Output: %q", res.Output)
	}
	for _, msg := range streamed {
		if strings.Contains(msg.Content, "STALE-REPLAY") {
			t.Errorf("replayed history leaked into the message stream: %+v", msg)
		}
	}
}

func TestJcodeResumeUnsupportedRequestsFreshRetry(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	bin, _, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: false})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, _ := runJcodeSession(t, bin, ctx, "continue", ExecOptions{Cwd: t.TempDir(), Timeout: 15 * time.Second, ResumeSessionID: "session_fake_1"})
	if res.Status != "failed" || !res.ResumeRejected {
		t.Fatalf("status=%q resumeRejected=%v, want failed + rejected when resume capability is absent", res.Status, res.ResumeRejected)
	}
}

func TestJcodeCancellationInterruptsSessionGracefully(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	bin, requests, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true, hangPrompt: true})

	ctx, cancel := context.WithCancel(context.Background())
	backend, err := New("jcode", Config{ExecutablePath: bin, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new jcode backend: %v", err)
	}
	session, err := backend.Execute(ctx, "long task", ExecOptions{Cwd: t.TempDir(), Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("execute jcode: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	// Give the handshake time to reach the in-flight prompt, then cancel the
	// task the way the daemon does.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(requests); err == nil && strings.Contains(string(data), `"method":"session/prompt"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	select {
	case res := <-session.Result:
		if res.Status != "aborted" {
			t.Fatalf("status = %q (%s), want aborted", res.Status, res.Error)
		}
		if res.SessionID != "session_fake_1" {
			t.Errorf("session id = %q; a cancelled run must preserve the session pointer", res.SessionID)
		}
		req := readRequests(t, requests)
		if !strings.Contains(req, `"method":"session/cancel"`) {
			t.Errorf("no session/cancel sent on task cancellation:\n%s", req)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled session did not finish")
	}
}

func TestJcodeCancellationOrphanTimeoutStillAborts(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	t.Setenv("MULTICA_JCODE_INTERRUPT_TIMEOUT", "200ms")
	bin, requests, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true, hangPrompt: true, ignoreCancel: true})

	ctx, cancel := context.WithCancel(context.Background())
	backend, err := New("jcode", Config{ExecutablePath: bin, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new jcode backend: %v", err)
	}
	session, err := backend.Execute(ctx, "long task", ExecOptions{Cwd: t.TempDir(), Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("execute jcode: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(requests); err == nil && strings.Contains(string(data), `"method":"session/prompt"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case res := <-session.Result:
		if res.Status != "aborted" {
			t.Fatalf("status = %q (%s), want aborted after an unacknowledged interrupt", res.Status, res.Error)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("teardown after ignored interrupt took %s; the 200ms window should bound it", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("session with ignored interrupt did not finish")
	}
}

func TestJcodeTimeoutClassification(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	t.Setenv("MULTICA_JCODE_INTERRUPT_TIMEOUT", "100ms")
	bin, _, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true, hangPrompt: true, ignoreCancel: true})

	ctx := context.Background()
	res, _ := runJcodeSession(t, bin, ctx, "slow", ExecOptions{Cwd: t.TempDir(), Timeout: 400 * time.Millisecond})
	if res.Status != "timeout" {
		t.Fatalf("status = %q (%s), want timeout", res.Status, res.Error)
	}
}

func TestJcodeConcurrencySlotWaitRespectsCancellation(t *testing.T) {
	table := &jcodeSlotTable{slots: map[string]chan struct{}{}}
	release, err := table.acquire(context.Background(), "/tmp/s.sock", 1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := table.acquire(ctx, "/tmp/s.sock", 1); err == nil {
		t.Fatal("second acquire succeeded with the only slot held")
	}
	release()
	release2, err := table.acquire(context.Background(), "/tmp/s.sock", 1)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()

	// Distinct sockets are independent pools.
	r1, err := table.acquire(context.Background(), "/tmp/a.sock", 1)
	if err != nil {
		t.Fatalf("socket a: %v", err)
	}
	defer r1()
	r2, err := table.acquire(context.Background(), "/tmp/b.sock", 1)
	if err != nil {
		t.Fatalf("socket b: %v", err)
	}
	defer r2()
}

func TestJcodeAutostartDisabledFailsFastWhenSocketDead(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	t.Setenv("MULTICA_JCODE_AUTOSTART", "0")
	bin, _, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true})

	backend, err := New("jcode", Config{ExecutablePath: bin, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new jcode backend: %v", err)
	}
	_, err = backend.Execute(context.Background(), "p", ExecOptions{Cwd: t.TempDir(), Timeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "autostart is disabled") {
		t.Fatalf("err = %v, want an autostart-disabled connection failure", err)
	}
}

func TestJcodeSocketPathValidation(t *testing.T) {
	bin, _, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true})
	backend, err := New("jcode", Config{ExecutablePath: bin, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new jcode backend: %v", err)
	}

	t.Setenv("MULTICA_JCODE_SOCKET", "/tmp/"+strings.Repeat("x", 120)+".sock")
	if _, err := backend.Execute(context.Background(), "p", ExecOptions{Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "MULTICA_JCODE_SOCKET") {
		t.Fatalf("err = %v, want the socket-length guidance", err)
	}

	t.Setenv("MULTICA_JCODE_SOCKET", "relative/path.sock")
	if _, err := backend.Execute(context.Background(), "p", ExecOptions{Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want an absolute-path rejection", err)
	}
}

func TestJcodeBlockedArgsKeepACPTransportStable(t *testing.T) {
	setJcodeTestRuntimeDir(t)
	bin, requests, _ := writeFakeJcodeACP(t, fakeJcodeOptions{resumeCapability: true})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, _ := runJcodeSession(t, bin, ctx, "p", ExecOptions{
		Cwd:     t.TempDir(),
		Timeout: 15 * time.Second,
		CustomArgs: []string{
			"serve", "login", "--socket", "/tmp/evil.sock", "--resume", "session_x",
			"--remote-working-dir", "/elsewhere",
			"--trace",
		},
	})
	if res.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed", res.Status, res.Error)
	}
	// A hijacked argv would never answer initialize — the transport coming up
	// at all proves the blocked tokens were filtered before exec.
	req := readRequests(t, requests)
	if !strings.Contains(req, `"method":"initialize"`) {
		t.Errorf("ACP transport did not come up under filtered args:\n%s", req)
	}
	for _, blocked := range []string{"acp", "serve", "server", "login", "--socket", "--resume", "--remote-working-dir"} {
		if _, ok := jcodeBlockedArgs[blocked]; !ok {
			t.Errorf("expected %q in jcodeBlockedArgs", blocked)
		}
	}
}

func TestJcodeEnvKnobParsing(t *testing.T) {
	logger := slog.Default()

	t.Setenv("MULTICA_JCODE_MAX_CONCURRENT", "")
	if got := jcodeMaxConcurrent(logger); got != jcodeDefaultMaxConcurrent {
		t.Errorf("default max concurrent = %d, want %d", got, jcodeDefaultMaxConcurrent)
	}
	t.Setenv("MULTICA_JCODE_MAX_CONCURRENT", "12")
	if got := jcodeMaxConcurrent(logger); got != 12 {
		t.Errorf("max concurrent = %d, want 12", got)
	}
	for _, invalid := range []string{"0", "-3", "lots"} {
		t.Setenv("MULTICA_JCODE_MAX_CONCURRENT", invalid)
		if got := jcodeMaxConcurrent(logger); got != jcodeDefaultMaxConcurrent {
			t.Errorf("max concurrent(%q) = %d, want default %d", invalid, got, jcodeDefaultMaxConcurrent)
		}
	}

	t.Setenv("MULTICA_JCODE_INTERRUPT_TIMEOUT", "")
	if got := jcodeInterruptTimeout(logger); got != jcodeDefaultInterruptTimeout {
		t.Errorf("default interrupt timeout = %s, want %s", got, jcodeDefaultInterruptTimeout)
	}
	t.Setenv("MULTICA_JCODE_INTERRUPT_TIMEOUT", "3s")
	if got := jcodeInterruptTimeout(logger); got != 3*time.Second {
		t.Errorf("interrupt timeout = %s, want 3s", got)
	}
	for _, invalid := range []string{"-1s", "soon", "0"} {
		t.Setenv("MULTICA_JCODE_INTERRUPT_TIMEOUT", invalid)
		if got := jcodeInterruptTimeout(logger); got != jcodeDefaultInterruptTimeout {
			t.Errorf("interrupt timeout(%q) = %s, want default", invalid, got)
		}
	}

	t.Setenv("MULTICA_JCODE_AUTOSTART", "")
	if !jcodeAutostartEnabled() {
		t.Error("autostart default = false, want true")
	}
	for _, off := range []string{"0", "false", "NO", "off"} {
		t.Setenv("MULTICA_JCODE_AUTOSTART", off)
		if jcodeAutostartEnabled() {
			t.Errorf("autostart(%q) = true, want false", off)
		}
	}
}
