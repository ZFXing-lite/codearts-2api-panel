package api

import (
	"strings"
	"testing"
)

func TestDetectRepetitionLoop_NormalText(t *testing.T) {
	text := "This is a perfectly normal response about Go programming. " +
		"It discusses interfaces, goroutines, channels, and error handling. " +
		"No repetition here, just regular technical content that varies from sentence to sentence."
	if _, _, found := detectRepetitionLoop(text); found {
		t.Fatal("normal text must not trigger")
	}
}

func TestDetectRepetitionLoop_ParagraphRepeat(t *testing.T) {
	para := "由于内容量巨大，我需要高效地写。让我把关键文档都写出来。" +
		"实际上，让我重新考虑。YG的核心诉求是：文档没写清楚SO是干什么的。"
	text := strings.Repeat(para, 5)
	period, reps, found := detectRepetitionLoop(text)
	if !found {
		t.Fatalf("5x paragraph repeat must trigger, textLen=%d", len(text))
	}
	if reps < 3 {
		t.Fatalf("reps=%d want >=3", reps)
	}
	if period != len(para) {
		t.Fatalf("period=%d want %d", period, len(para))
	}
}

func TestDetectRepetitionLoop_SingleCharRun(t *testing.T) {
	text := strings.Repeat("!", 300)
	_, reps, found := detectRepetitionLoop(text)
	if !found {
		t.Fatal("300x '!' must trigger single-char run detection")
	}
	if reps < 200 {
		t.Fatalf("reps=%d want >=200", reps)
	}
}

func TestDetectRepetitionLoop_ShortRepeatBelowSpan(t *testing.T) {
	short := "abc"
	text := strings.Repeat(short, 20)
	if _, _, found := detectRepetitionLoop(text); found {
		t.Fatal("short repeat (period=3, below minPeriod=50) must not trigger")
	}
}

func TestDetectRepetitionLoop_TwoRepsNotEnough(t *testing.T) {
	para := "This is a unique paragraph with enough variety and length to exceed minPeriod. " +
		"It contains multiple sentences with different words in each one, preventing sub-period matches. " +
		"The content here is intentionally varied to make only the full paragraph a repeating unit. END."
	if len(para) < repMinPeriod {
		t.Fatalf("para too short: %d", len(para))
	}
	text := para + para
	if _, _, found := detectRepetitionLoop(text); found {
		t.Fatal("only 2 reps must not trigger (minReps=3)")
	}
}

func TestDetectRepetitionLoop_CodeBlock(t *testing.T) {
	var b strings.Builder
	b.WriteString("```go\n")
	for i := 0; i < 10; i++ {
		b.WriteString("    fmt.Println(\"hello world\")\n")
		b.WriteString("    fmt.Println(\"another line\")\n")
		b.WriteString("    x := doSomething(i)\n")
	}
	b.WriteString("```\n")
	if _, _, found := detectRepetitionLoop(b.String()); found {
		t.Fatal("code block with similar but varying lines must not trigger")
	}
}

func TestDetectRepetitionLoop_ReasoningRepeat(t *testing.T) {
	para := strings.Repeat("A", 60) + strings.Repeat("B", 60) + strings.Repeat("C", 60)
	text := strings.Repeat(para, 4)
	_, _, found := detectRepetitionLoop(text)
	if !found {
		t.Fatalf("4x 180-byte paragraph repeat must trigger, textLen=%d", len(text))
	}
}

func TestDetectRepetitionLoop_JustBelowMinSpan(t *testing.T) {
	para := strings.Repeat("Z", 60)
	text := strings.Repeat(para, 3)
	if len(text) >= repMinSpan {
		if _, _, found := detectRepetitionLoop(text); !found {
			t.Fatalf("3x60=180, span=%d >= minSpan=%d should trigger if met", len(text), repMinSpan)
		}
	}
}

func TestDetectRepetitionLoop_ExactMinSpan(t *testing.T) {
	pLen := repMinSpan / repMinReps
	if pLen < repMinPeriod {
		pLen = repMinPeriod
	}
	para := strings.Repeat("Q", pLen)
	text := strings.Repeat(para, repMinReps+1)
	if len(text) < repMinSpan {
		t.Skipf("text too short: %d < %d", len(text), repMinSpan)
	}
	_, _, found := detectRepetitionLoop(text)
	if !found {
		t.Fatalf("repeating at minSpan boundary must trigger: period=%d reps=%d span=%d",
			pLen, repMinReps+1, len(text))
	}
}

func TestRepetitionGuard_Disabled(t *testing.T) {
	t.Setenv("REPETITION_GUARD", "0")
	g := newRepetitionGuard()
	if !g.disabled {
		t.Fatal("REPETITION_GUARD=0 must disable")
	}
	asm := &sseAssembly{}
	para := strings.Repeat("X", 200)
	for i := 0; i < 5; i++ {
		asm.content.WriteString(para)
	}
	if g.check(asm) {
		t.Fatal("disabled guard must never trigger")
	}
}

func TestRepetitionGuard_IntegrationContent(t *testing.T) {
	t.Setenv("REPETITION_GUARD", "1")
	g := newRepetitionGuard()
	asm := &sseAssembly{}
	para := "这是一段会重复的中文段落，包含足够的字符来超过最小周期阈值。" +
		"Adding some English too to make the period larger than 50 bytes."
	for i := 0; i < 10; i++ {
		asm.content.WriteString(para)
		if g.check(asm) {
			if g.TriggerKind != "content" {
				t.Fatalf("kind=%s want content", g.TriggerKind)
			}
			return
		}
	}
	t.Fatal("10x repeat must trigger guard")
}

func TestRepetitionGuard_IntegrationReasoning(t *testing.T) {
	t.Setenv("REPETITION_GUARD", "1")
	g := newRepetitionGuard()
	asm := &sseAssembly{}
	para := strings.Repeat("thinking about the same thing over and over ", 3)
	for i := 0; i < 10; i++ {
		asm.reasoning.WriteString(para)
		if g.check(asm) {
			if g.TriggerKind != "reasoning" {
				t.Fatalf("kind=%s want reasoning", g.TriggerKind)
			}
			return
		}
	}
	t.Fatal("10x reasoning repeat must trigger guard")
}

func TestDetectNearDuplicate_SimilarBlocks(t *testing.T) {
	// Simulate GLM-5.2's pattern: same topic, slightly different wording.
	// Each block shares 50%+ of its 8-grams with the reference block.
	base := "详细分析如下：验证码通过后进入二次审核环节，但是第二轮验证失败了"
	block1 := "captcha验证过了，但是挂在第二轮审核流程。" + base + "需要重新检查"
	block2 := "captcha验证通过了，但在第二轮审核中失败。" + base + "需要再次排查"
	block3 := "captcha已过验证，但是挂在第二轮审核环节。" + base + "重新分析一遍"
	block4 := "captcha已经通过了，卡在第二轮审核过程。" + base + "需要再看一下"
	text := block1 + block2 + block3 + block4
	period, dups, found := detectNearDuplicate(text)
	if !found {
		t.Fatalf("4 near-dup blocks must trigger, textLen=%d", len(text))
	}
	if dups < 3 {
		t.Fatalf("dups=%d want >=3", dups)
	}
	t.Logf("detected: period=%d dups=%d", period, dups)
}

func TestDetectNearDuplicate_DifferentBlocks(t *testing.T) {
	// Truly different content — no shared vocabulary at all
	block1 := strings.Repeat("AAAA1234BBBB5678CCCC9012DDDD3456", 10)
	block2 := strings.Repeat("xxxx!@#$yyyy%^&*zzzz()_+wwww-=[]", 10)
	block3 := strings.Repeat("MMMM~~~~NNNN````OOOO{{}}PPPP<>??", 10)
	text := block1 + block2 + block3
	if _, _, found := detectNearDuplicate(text); found {
		t.Fatal("different-content blocks must not trigger near-dup")
	}
}

func TestDetectNearDuplicate_TooShort(t *testing.T) {
	if _, _, found := detectNearDuplicate("short"); found {
		t.Fatal("short text must not trigger")
	}
}

func TestNgramOverlapScore(t *testing.T) {
	a := "abcdefghijklmnop"
	// Identical strings should have overlap = 1.0
	set := buildNgramSet(a, 4)
	score := ngramOverlapScore(set, a, 4)
	if score < 0.99 {
		t.Fatalf("identical strings overlap=%.2f want ~1.0", score)
	}
	// Completely different strings should have low overlap
	b := "zyxwvutsrqponmlk"
	score2 := ngramOverlapScore(set, b, 4)
	if score2 > 0.3 {
		t.Fatalf("different strings overlap=%.2f want <0.3", score2)
	}
}

func TestRepetitionGuard_NearDupReasoning(t *testing.T) {
	t.Setenv("REPETITION_GUARD", "1")
	g := newRepetitionGuard()
	asm := &sseAssembly{}
	// Write 4 SIMILAR but NOT IDENTICAL blocks — exact match won't catch this,
	// only near-dup detection should fire.
	shared := "需要检查验证码状态和审核链路的完整性。分析中。重新审视流程。"
	blocks := []string{
		"让我分析这个captcha验证流程的问题。第一步通过了，但是第二步挂了。" + shared,
		"让我来分析captcha验证过程的故障。第一步OK了，但第二步失败了。" + shared,
		"让我重新分析这个captcha验证流程问题。第一步过了，但第二步出错了。" + shared,
		"让我再看看captcha验证流程的问题所在。第一步成功了，但第二步卡住了。" + shared,
	}
	for _, b := range blocks {
		asm.reasoning.WriteString(b)
	}
	if !g.check(asm) {
		t.Fatal("4x near-dup reasoning blocks must trigger guard")
	}
	if g.TriggerKind != "reasoning" && g.TriggerKind != "reasoning_neardup" {
		t.Fatalf("kind=%s want reasoning or reasoning_neardup", g.TriggerKind)
	}
}

func TestRepetitionGuard_TriggersOnlyOnce(t *testing.T) {
	t.Setenv("REPETITION_GUARD", "1")
	g := newRepetitionGuard()
	asm := &sseAssembly{}
	para := strings.Repeat("W", 200)
	for i := 0; i < 5; i++ {
		asm.content.WriteString(para)
	}
	if !g.check(asm) {
		t.Fatal("first check must trigger")
	}
	asm.content.WriteString(strings.Repeat(para, 5))
	if g.check(asm) {
		t.Fatal("second check must not trigger (already triggered)")
	}
}
