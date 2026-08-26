//go:build agentintegration

package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// jcodeSmokeEnv pins the smoke run to a throwaway Multica-scoped jcode server
// (short /tmp runtime dir — socket paths are SUN_LEN-bound) and stops that
// server when the test ends, so repeated runs cannot collide with the user's
// interactive jcode servers or leak daemons.
func jcodeSmokeEnv(t *testing.T, jcodePath string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mjcs")
	if err != nil {
		t.Fatalf("mkdir smoke runtime dir: %v", err)
	}
	t.Setenv("MULTICA_JCODE_RUNTIME_DIR", dir)
	socket := filepath.Join(dir, "jcode.sock")
	t.Cleanup(func() {
		stop := exec.Command(jcodePath, "--quiet", "server", "stop", "--force")
		stop.Env = append(os.Environ(), "JCODE_RUNTIME_DIR="+dir, "JCODE_SOCKET="+socket)
		if out, err := stop.CombinedOutput(); err != nil {
			t.Logf("jcode server stop (best-effort): %v (%s)", err, strings.TrimSpace(string(out)))
		}
		_ = os.RemoveAll(dir)
	})
}

// jcodeSmokeModel returns the model the smoke turn should run on.
// MULTICA_JCODE_SMOKE_MODEL overrides; empty keeps the jcode configuration's
// own default model. A machine whose default model is not usable (for example
// a custom provider profile that rejects it) needs the override.
func jcodeSmokeModel() string {
	return strings.TrimSpace(os.Getenv("MULTICA_JCODE_SMOKE_MODEL"))
}

// TestJcodeRealPersistentServerSmoke drives an authenticated Jcode turn
// end-to-end through the real persistent server: AGENTS.md bootstrap context,
// tool execution inside the bound cwd, session-id persistence, usage
// reporting — then a second Execute that RESUMES the same session, proving
// reattachment through the shared server after the first shim exited.
func TestJcodeRealPersistentServerSmoke(t *testing.T) {
	requireRealAgentSmoke(t)
	if testing.Short() {
		t.Skip("skipping real-binary smoke test in -short mode")
	}
	path, err := exec.LookPath("jcode")
	if err != nil {
		t.Skip("jcode not on PATH; skipping real-binary smoke test")
	}
	if version, err := exec.Command(path, "--version").CombinedOutput(); err == nil {
		t.Logf("jcode --version: %s", strings.TrimSpace(string(version)))
	}
	jcodeSmokeEnv(t, path)

	workDir := t.TempDir()
	writeMcodeSmokeFile(t, filepath.Join(workDir, "AGENTS.md"), `# Multica smoke context

For every response in this workspace, include the exact marker AGENTS-JCODE-OK.
`)
	writeMcodeSmokeFile(t, filepath.Join(workDir, "tool-canary.txt"), "TOOL-JCODE-OK\n")

	backend, err := New("jcode", Config{ExecutablePath: path, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new jcode backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx,
		"Read tool-canary.txt with a file-reading tool and include its exact contents in your final response. Follow the workspace instructions.",
		ExecOptions{Cwd: workDir, Timeout: 210 * time.Second, Model: jcodeSmokeModel()},
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	var toolUses, toolResults int
	messagesDone := make(chan struct{})
	go func() {
		defer close(messagesDone)
		for message := range session.Messages {
			switch message.Type {
			case MessageToolUse:
				toolUses++
			case MessageToolResult:
				toolResults++
			}
		}
	}()

	var result Result
	select {
	case result = <-session.Result:
	case <-time.After(240 * time.Second):
		t.Fatal("timeout waiting for real jcode result")
	}
	<-messagesDone

	if result.Status != "completed" {
		t.Fatalf("real jcode run did not complete: status=%q error=%q output=%q", result.Status, result.Error, result.Output)
	}
	for _, marker := range []string{"AGENTS-JCODE-OK", "TOOL-JCODE-OK"} {
		if !strings.Contains(result.Output, marker) {
			t.Fatalf("real jcode output missing %s: %q", marker, result.Output)
		}
	}
	if toolUses == 0 || toolResults == 0 {
		t.Fatalf("real jcode stream exposed no tool activity: uses=%d results=%d", toolUses, toolResults)
	}
	if result.SessionID == "" {
		t.Fatal("real jcode run returned an empty session id")
	}
	if len(result.Usage) == 0 {
		t.Fatal("real jcode run reported no token usage")
	}

	// Second run resumes the SAME session on the persistent server — the
	// first shim is gone, so a successful contextual answer proves the
	// session state lives server-side.
	resumed, err := backend.Execute(ctx,
		"Without using any tools: repeat the exact canary marker you read from tool-canary.txt earlier in this conversation.",
		ExecOptions{Cwd: workDir, Timeout: 120 * time.Second, ResumeSessionID: result.SessionID},
	)
	if err != nil {
		t.Fatalf("resume execute: %v", err)
	}
	go func() {
		for range resumed.Messages {
		}
	}()
	var resumedResult Result
	select {
	case resumedResult = <-resumed.Result:
	case <-time.After(180 * time.Second):
		t.Fatal("timeout waiting for resumed jcode result")
	}
	if resumedResult.Status != "completed" {
		t.Fatalf("resumed jcode run did not complete: status=%q error=%q", resumedResult.Status, resumedResult.Error)
	}
	if resumedResult.SessionID != result.SessionID {
		t.Fatalf("resumed session id %q != original %q", resumedResult.SessionID, result.SessionID)
	}
	if !strings.Contains(resumedResult.Output, "TOOL-JCODE-OK") {
		t.Fatalf("resumed turn lost conversation context (no canary recall): %q", resumedResult.Output)
	}
	t.Logf("real jcode smoke OK: session=%s tools=%d/%d resumed_output=%q", result.SessionID, toolUses, toolResults, resumedResult.Output)
}

// TestJcodeRealConcurrentSessionsAndCancellation is the spec's demonstration
// scenario against the real server: two isolated sessions run concurrently on
// one persistent server, cancelling one interrupts only that session, and the
// other completes untouched.
func TestJcodeRealConcurrentSessionsAndCancellation(t *testing.T) {
	requireRealAgentSmoke(t)
	if testing.Short() {
		t.Skip("skipping real-binary smoke test in -short mode")
	}
	path, err := exec.LookPath("jcode")
	if err != nil {
		t.Skip("jcode not on PATH; skipping real-binary smoke test")
	}
	jcodeSmokeEnv(t, path)

	backend, err := New("jcode", Config{ExecutablePath: path, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new jcode backend: %v", err)
	}

	dirA, dirB := t.TempDir(), t.TempDir()
	writeMcodeSmokeFile(t, filepath.Join(dirA, "canary-a.txt"), "CANARY-A\n")

	ctxA, cancelA := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	var wg sync.WaitGroup
	var resultA, resultB Result

	wg.Add(1)
	go func() {
		defer wg.Done()
		session, err := backend.Execute(ctxA,
			"Read canary-a.txt with a file-reading tool and include its exact contents in your final response.",
			ExecOptions{Cwd: dirA, Timeout: 210 * time.Second, Model: jcodeSmokeModel()},
		)
		if err != nil {
			t.Errorf("execute A: %v", err)
			return
		}
		go func() {
			for range session.Messages {
			}
		}()
		resultA = <-session.Result
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		session, err := backend.Execute(ctxB,
			"Count from 1 to 2000, one number per line, double-checking each number carefully before writing it.",
			ExecOptions{Cwd: dirB, Timeout: 210 * time.Second, Model: jcodeSmokeModel()},
		)
		if err != nil {
			t.Errorf("execute B: %v", err)
			return
		}
		sawSession := make(chan struct{})
		go func() {
			var once sync.Once
			for msg := range session.Messages {
				if msg.SessionID != "" {
					once.Do(func() { close(sawSession) })
				}
			}
		}()
		// Cancel B once its session exists and has had a moment to run.
		select {
		case <-sawSession:
		case <-time.After(120 * time.Second):
		}
		time.Sleep(3 * time.Second)
		cancelB()
		resultB = <-session.Result
	}()

	wg.Wait()

	if resultA.Status != "completed" || !strings.Contains(resultA.Output, "CANARY-A") {
		t.Fatalf("task A should complete despite B's cancellation: status=%q error=%q output=%q", resultA.Status, resultA.Error, resultA.Output)
	}
	if resultB.Status != "aborted" {
		t.Fatalf("task B should be aborted by cancellation: status=%q error=%q", resultB.Status, resultB.Error)
	}
	if resultB.SessionID == "" {
		t.Fatal("cancelled task B lost its session id; diagnostics require it")
	}
	if resultA.SessionID == resultB.SessionID {
		t.Fatalf("tasks shared one session: %q", resultA.SessionID)
	}
	t.Logf("real jcode concurrency smoke OK: A=%s completed, B=%s aborted", resultA.SessionID, resultB.SessionID)
}
