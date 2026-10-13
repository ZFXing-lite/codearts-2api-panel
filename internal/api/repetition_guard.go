package api

import (
	"os"
	"strings"
)

const (
	repMinPeriod  = 50
	repMaxPeriod  = 2000
	repMinReps    = 3
	repMinSpan    = 400
	repCheckEvery = 200
	repWindowSize = 8192
	repCharRunMin = 200

	// Near-duplicate block detection. Catches the GLM-5.2 pattern of
	// generating 3-4 "similar but not identical" analysis blocks in
	// reasoning — same topic, slightly different wording each time.
	ndShingleN   = 8    // n-gram size for shingling
	ndMinOverlap = 0.50 // 50% shared n-grams = near-dup
	ndMinDups    = 3    // need 3+ similar blocks to trigger
)

type repetitionGuard struct {
	lastContentLen   int
	lastReasoningLen int
	disabled         bool
	triggered        bool
	TriggerKind      string
	TriggerPeriod    int
	TriggerReps      int
}

// newRepetitionGuard: OFF by default per the pure-forwarding rule. Cutting a
// stream mid-generation is response mutation; operators opt in via
// REPETITION_GUARD=1 (or true/on/yes) when they'd rather serve a truncated
// stream than let a known-degenerate loop burn tokens.
func newRepetitionGuard() *repetitionGuard {
	return &repetitionGuard{
		disabled: !repetitionGuardEnabled(),
	}
}

func repetitionGuardEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("REPETITION_GUARD"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (g *repetitionGuard) check(asm *sseAssembly) bool {
	if g == nil || g.disabled || g.triggered || asm == nil {
		return false
	}
	if cl := asm.content.Len(); cl-g.lastContentLen >= repCheckEvery {
		g.lastContentLen = cl
		text := asm.content.String()
		if p, r, ok := detectRepetitionLoop(text); ok {
			g.triggered = true
			g.TriggerKind = "content"
			g.TriggerPeriod = p
			g.TriggerReps = r
			return true
		}
		if p, r, ok := detectNearDuplicate(text); ok {
			g.triggered = true
			g.TriggerKind = "content_neardup"
			g.TriggerPeriod = p
			g.TriggerReps = r
			return true
		}
	}
	if rl := asm.reasoning.Len(); rl-g.lastReasoningLen >= repCheckEvery {
		g.lastReasoningLen = rl
		text := asm.reasoning.String()
		if p, r, ok := detectRepetitionLoop(text); ok {
			g.triggered = true
			g.TriggerKind = "reasoning"
			g.TriggerPeriod = p
			g.TriggerReps = r
			return true
		}
		if p, r, ok := detectNearDuplicate(text); ok {
			g.triggered = true
			g.TriggerKind = "reasoning_neardup"
			g.TriggerPeriod = p
			g.TriggerReps = r
			return true
		}
	}
	return false
}

func detectRepetitionLoop(text string) (period, reps int, found bool) {
	n := len(text)
	if n < repCharRunMin {
		return 0, 0, false
	}
	start := 0
	if n > repWindowSize {
		start = n - repWindowSize
	}
	win := text[start:]
	wn := len(win)

	if wn >= repMinSpan {
		maxP := repMaxPeriod
		if wn/repMinReps < maxP {
			maxP = wn / repMinReps
		}
		for p := repMinPeriod; p <= maxP; p++ {
			pat := win[wn-p:]
			count := 1
			pos := wn - p
			for pos >= p {
				if win[pos-p:pos] == pat {
					count++
					pos -= p
				} else {
					break
				}
			}
			if count >= repMinReps && count*p >= repMinSpan {
				// Markdown tables produce legitimate row-level repetition — a
				// 3-column table with 5 rows repeats "| ... | ... | ... |\n"
				// with identical spacing and can look like a degeneration loop
				// even though it's expected structure. Skip when the matched
				// window looks like a table.
				if looksLikeMarkdownTable(win[wn-count*p:]) {
					continue
				}
				return p, count, true
			}
		}
	}

	if wn >= repCharRunMin {
		tail := win[wn-repCharRunMin:]
		if allSameByte(tail) {
			ch := tail[0]
			runLen := 0
			for i := wn - 1; i >= 0; i-- {
				if win[i] == ch {
					runLen++
				} else {
					break
				}
			}
			if runLen >= repCharRunMin {
				return 1, runLen, true
			}
		}
	}
	return 0, 0, false
}

// looksLikeMarkdownTable reports whether the given span looks like several
// consecutive markdown table rows (or a table separator). Cheap heuristic:
// every non-empty line starts with `|`, and at least three lines are present.
func looksLikeMarkdownTable(s string) bool {
	lines := strings.Split(s, "\n")
	tableLines := 0
	nonEmpty := 0
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		nonEmpty++
		if strings.HasPrefix(t, "|") {
			tableLines++
		}
	}
	return nonEmpty >= 3 && tableLines == nonEmpty
}

func allSameByte(s string) bool {
	if len(s) == 0 {
		return false
	}
	ch := s[0]
	for i := 1; i < len(s); i++ {
		if s[i] != ch {
			return false
		}
	}
	return true
}

// detectNearDuplicate catches near-duplicate blocks — the GLM-5.2 pattern
// of repeating similar (but not identical) analysis 3-4 times. Uses n-gram
// overlap: if consecutive blocks of size P share ≥50% of their 8-byte
// substrings, they are near-duplicates. Tries multiple block sizes to
// handle varying repetition granularity.
func detectNearDuplicate(text string) (period, dups int, found bool) {
	n := len(text)
	if n < 600 {
		return 0, 0, false
	}
	start := 0
	if n > repWindowSize {
		start = n - repWindowSize
	}
	win := text[start:]
	wn := len(win)

	for _, p := range []int{200, 400, 800, 1500} {
		if p*ndMinDups > wn {
			continue
		}
		ref := win[wn-p:]
		refGrams := buildNgramSet(ref, ndShingleN)
		count := 1
		pos := wn - p
		for pos >= p {
			candidate := win[pos-p : pos]
			if ngramOverlapScore(refGrams, candidate, ndShingleN) >= ndMinOverlap {
				count++
				pos -= p
			} else {
				break
			}
		}
		if count >= ndMinDups {
			return p, count, true
		}
	}
	return 0, 0, false
}

func buildNgramSet(s string, n int) map[string]struct{} {
	if len(s) < n {
		return nil
	}
	set := make(map[string]struct{}, len(s)-n+1)
	for i := 0; i <= len(s)-n; i++ {
		set[s[i:i+n]] = struct{}{}
	}
	return set
}

func ngramOverlapScore(refGrams map[string]struct{}, candidate string, n int) float64 {
	if len(candidate) < n || len(refGrams) == 0 {
		return 0
	}
	shared := 0
	total := 0
	for i := 0; i <= len(candidate)-n; i++ {
		total++
		if _, ok := refGrams[candidate[i:i+n]]; ok {
			shared++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(shared) / float64(total)
}
