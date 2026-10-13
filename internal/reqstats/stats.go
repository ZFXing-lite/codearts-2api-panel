package reqstats

import (
	"sync"
	"time"
)

type Counters struct {
	Requests             int64   `json:"requests"`
	Errors               int64   `json:"errors"`
	Streams              int64   `json:"streams"`
	ClientAborts         int64   `json:"client_aborts"`    // client disconnected mid-response (client_gone)
	Rotations            int64   `json:"rotations"`        // account rotations within a single request
	SessionCapHits       int64   `json:"session_cap_hits"` // Huawei TM.00001041 concurrent-session rejects
	CacheHits            int64   `json:"cache_hits"`       // local identical-response cache
	UpstreamCachedTokens float64 `json:"upstream_cached_tokens"`
	PromptTokens         float64 `json:"prompt_tokens"`
	CompletionTokens     float64 `json:"completion_tokens"`
	LatencyMSTotal       int64   `json:"-"`
	LatencyCount         int64   `json:"-"`
	LatencyP95Approx     int64   `json:"latency_p95_ms"`
	HeaderWaitMSTotal    int64   `json:"-"`
	HeaderWaitCount      int64   `json:"-"`
	HeaderWaitP95Approx  int64   `json:"header_wait_p95_ms"` // upstream first-byte (response header) wait
	// Protocol breakdown: chat / messages (Claude) / responses.
	ProtoChat      int64 `json:"proto_chat"`
	ProtoMessages  int64 `json:"proto_messages"`
	ProtoResponses int64 `json:"proto_responses"`
	// Slow-request sampling (latency > slowRequestThresholdMS).
	SlowCount int64 `json:"slow_count"`
}

type TimelinePoint struct {
	TS                   int64   `json:"ts"`
	Label                string  `json:"label"`
	Requests             int64   `json:"requests"`
	Errors               int64   `json:"errors"`
	CacheHits            int64   `json:"cache_hits"`
	UpstreamCachedTokens float64 `json:"upstream_cached_tokens"`
	PromptTokens         float64 `json:"prompt_tokens"`
	CompletionTokens     float64 `json:"completion_tokens"`
}

type Snapshot struct {
	Today    Counters        `json:"today"`
	Total    Counters        `json:"total"`
	Timeline []TimelinePoint `json:"timeline"`
}

type Tracker struct {
	mu       sync.Mutex
	today    Counters
	total    Counters
	dayKey   string
	timeline map[int64]*TimelinePoint
	keep     int
}

func New() *Tracker {
	return &Tracker{
		timeline: make(map[int64]*TimelinePoint),
		keep:     26,
		dayKey:   dayKey(time.Now()),
	}
}

var statsLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

func dayKey(t time.Time) string {
	return t.In(statsLoc).Format("2006-01-02")
}

func hourBucket(t time.Time) int64 {
	lt := t.In(statsLoc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), 0, 0, 0, statsLoc).UnixMilli()
}

func hourLabel(ms int64) string {
	return time.UnixMilli(ms).In(statsLoc).Format("15:04")
}

// rollDayLocked resets today's counters when the local day changes. Caller holds t.mu.
func (t *Tracker) rollDayLocked(now time.Time) {
	if dayKey(now) != t.dayKey {
		t.today = Counters{}
		t.dayKey = dayKey(now)
	}
}

// NoteEvent records a lightweight reliability event that is not a full request:
// "client_abort" (client disconnected mid-response), "rotation" (account switch),
// "session_cap" (Huawei TM.00001041 concurrent-session reject).
func (t *Tracker) NoteEvent(kind string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollDayLocked(time.Now())
	bump := func(c *Counters) {
		switch kind {
		case "client_abort":
			c.ClientAborts++
		case "rotation":
			c.Rotations++
		case "session_cap":
			c.SessionCapHits++
		}
	}
	bump(&t.today)
	bump(&t.total)
}

// RecordHeaderWait records upstream first-byte (response header) latency in ms.
// Kept separate from Record so rotations/retries each contribute a first-byte sample.
func (t *Tracker) RecordHeaderWait(ms int64) {
	if t == nil || ms <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollDayLocked(time.Now())
	apply := func(c *Counters) {
		c.HeaderWaitMSTotal += ms
		c.HeaderWaitCount++
		if ms > c.HeaderWaitP95Approx {
			c.HeaderWaitP95Approx = ms
		} else if c.HeaderWaitCount > 20 {
			c.HeaderWaitP95Approx = (c.HeaderWaitP95Approx*9 + ms) / 10
		}
	}
	apply(&t.today)
	apply(&t.total)
}

// slowRequestThresholdMS marks requests slower than this as "slow" for the
// slow-request counter. 3s catches the 2-3 minute cases and normal slow turns.
const slowRequestThresholdMS = 3000

// Record logs one chat attempt.
// localCacheHit = identical-body response cache; upstreamCached = provider prompt-cache tokens.
// proto is "chat" / "messages" / "responses" — empty defaults to "chat".
func (t *Tracker) Record(stream, localCacheHit, ok bool, promptTok, completionTok, upstreamCached float64, latencyMS int64, proto string) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollDayLocked(now)
	apply := func(c *Counters) {
		c.Requests++
		if !ok {
			c.Errors++
		}
		if stream {
			c.Streams++
		}
		if localCacheHit {
			c.CacheHits++
		}
		if upstreamCached > 0 {
			c.UpstreamCachedTokens += upstreamCached
		}
		c.PromptTokens += promptTok
		c.CompletionTokens += completionTok
		switch proto {
		case "messages":
			c.ProtoMessages++
		case "responses":
			c.ProtoResponses++
		default:
			c.ProtoChat++
		}
		if latencyMS > 0 {
			c.LatencyMSTotal += latencyMS
			c.LatencyCount++
			if latencyMS > c.LatencyP95Approx {
				c.LatencyP95Approx = latencyMS
			} else if c.LatencyCount > 20 {
				c.LatencyP95Approx = (c.LatencyP95Approx*9 + latencyMS) / 10
			}
			if latencyMS > slowRequestThresholdMS {
				c.SlowCount++
			}
		}
	}
	apply(&t.today)
	apply(&t.total)

	b := hourBucket(now)
	pt := t.timeline[b]
	if pt == nil {
		pt = &TimelinePoint{TS: b, Label: hourLabel(b)}
		t.timeline[b] = pt
	}
	pt.Requests++
	if !ok {
		pt.Errors++
	}
	if localCacheHit {
		pt.CacheHits++
	}
	if upstreamCached > 0 {
		pt.UpstreamCachedTokens += upstreamCached
	}
	pt.PromptTokens += promptTok
	pt.CompletionTokens += completionTok
	t.trimLocked()
}

func (t *Tracker) trimLocked() {
	if len(t.timeline) <= t.keep {
		return
	}
	keys := make([]int64, 0, len(t.timeline))
	for k := range t.timeline {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for i := 0; i < len(keys)-t.keep; i++ {
		delete(t.timeline, keys[i])
	}
}

func (t *Tracker) Snapshot() Snapshot {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollDayLocked(now)
	out := Snapshot{
		Today: t.today,
		Total: t.total,
	}
	cur := hourBucket(now)
	for i := t.keep - 1; i >= 0; i-- {
		ts := cur - int64(i)*3600000
		pt := t.timeline[ts]
		if pt == nil {
			out.Timeline = append(out.Timeline, TimelinePoint{TS: ts, Label: hourLabel(ts)})
		} else {
			cp := *pt
			out.Timeline = append(out.Timeline, cp)
		}
	}
	return out
}
