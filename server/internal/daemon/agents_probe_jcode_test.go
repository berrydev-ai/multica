package daemon

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestProbeAgentCLIs_JcodeGatedByExperimentalFlag covers the Jcode prototype's
// rollout gate: with the binary on PATH the provider must stay invisible until
// MULTICA_EXPERIMENTAL_JCODE opts the daemon in, and must be discovered like
// any other provider once it does. "Invisible" is what guarantees the spec's
// disabled-state promises — no detection, no registration, and no
// `jcode serve` ever started — because everything downstream keys off this
// probe result.
func TestProbeAgentCLIs_JcodeGatedByExperimentalFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	fakeDir := t.TempDir()
	writeDaemonTestExecutable(t, filepath.Join(fakeDir, "jcode"), []byte("#!/bin/sh\nexit 0\n"))

	orig := resolveAgentsViaLoginShell
	t.Cleanup(func() { resolveAgentsViaLoginShell = orig })
	resolveAgentsViaLoginShell = func([]string) map[string]string {
		return map[string]string{}
	}
	resetShellResolveCacheForTest(t)

	t.Setenv("PATH", fakeDir)
	t.Setenv("MULTICA_JCODE_PATH", "")

	t.Setenv("MULTICA_EXPERIMENTAL_JCODE", "")
	if _, ok := probeAgentCLIs()["jcode"]; ok {
		t.Fatal("jcode was discovered without MULTICA_EXPERIMENTAL_JCODE; the flag must gate detection")
	}

	t.Setenv("MULTICA_EXPERIMENTAL_JCODE", "0")
	if _, ok := probeAgentCLIs()["jcode"]; ok {
		t.Fatal("jcode was discovered with MULTICA_EXPERIMENTAL_JCODE=0; falsy values must keep the gate closed")
	}

	t.Setenv("MULTICA_EXPERIMENTAL_JCODE", "1")
	entry, ok := probeAgentCLIs()["jcode"]
	if !ok {
		t.Fatal("jcode was not discovered with MULTICA_EXPERIMENTAL_JCODE=1")
	}
	if entry.Command != "jcode" {
		t.Errorf("jcode command = %q, want %q", entry.Command, "jcode")
	}

	t.Setenv("MULTICA_JCODE_MODEL", "claude-fable-5")
	entry, ok = probeAgentCLIs()["jcode"]
	if !ok || entry.Model != "claude-fable-5" {
		t.Errorf("jcode entry = %+v (ok=%v), want MULTICA_JCODE_MODEL to seed the default model", entry, ok)
	}
}
