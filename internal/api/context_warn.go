package api

import (
	"log"
	"net/http"
	"strconv"
)

// contextWarnTokens is a conservative early-warning line, set well below
// Huawei's 307200-token hard cap. It is not meant to line up with any
// client-side trigger (e.g. Claude Code's own local auto-compact) — that
// counts tokens with a different tokenizer against a different context
// window, and there's no fixed conversion between the two, so the margin
// here has to absorb tokenizer disagreement, not just estimator error.
const contextWarnTokens = 250_000

// warnIfContextLarge never touches the request body. This gateway is
// stateless per request — the client owns and resends the full history every
// turn, so only the client can actually shrink what it sends next. This just
// surfaces, via response header and log, that a request is getting close to
// Huawei's hard limit, so the client's own compaction (automatic or a manual
// /compact) can act before a later turn is rejected outright. See
// protoStream.emitError in protocol.go for what the client sees if a request
// is already over the limit by the time it reaches Huawei.
func warnIfContextLarge(w http.ResponseWriter, sessionID string, tokens int64) {
	if tokens < contextWarnTokens {
		return
	}
	w.Header().Set("X-Context-Tokens-Estimate", strconv.FormatInt(tokens, 10))
	w.Header().Set("X-Context-Warning", "approaching_limit")
	log.Printf("chat context_warning session=%s tokens_est=%d threshold=%d hard_limit=307200",
		sessionID, tokens, contextWarnTokens)
}
