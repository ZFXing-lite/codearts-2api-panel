package api

import (
	"os"
	"testing"
)

// TestMain hermetically clears env vars that behavior-gate api-package code so
// tests can't be perturbed by whatever REPETITION_GUARD / SSE_DEDUP / etc.
// values are set on the developer machine or CI runner. Any test that needs a
// specific value opts in via t.Setenv, which restores the cleared baseline.
//
// This closes M5 (regurgitation_test env leakage under parallel testing): the
// regurgitation and repetition-guard suites depend on newRepetitionGuard() /
// assembledResponseFingerprint() reading a clean environment; without this,
// a preset REPETITION_GUARD=1 on the host would make TestRepetitionGuard_Disabled
// spuriously see !g.disabled if the explicit t.Setenv("0") were ever removed,
// and would flip regurgitation-loop wiring in future integration tests.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"REPETITION_GUARD",
		"SSE_DEDUP",
		"SSE_ASSEMBLY_MAX",
		"FORCE_THINKING",
		"STRIP_HISTORICAL_REASONING",
		"KEEP_REASONING_TURNS",
		"STREAMING_WALL_LIMIT",
		"THINK_LOOP_TIMEOUT",
		"SSE_IDLE_TIMEOUT",
	} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
