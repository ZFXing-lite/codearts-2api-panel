package huawei

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go-codearts/internal/kit"
)

// SubscriptionInfo is the CodeArts CLI /snap-manager/v1/statistics/plugin payload.
type SubscriptionInfo struct {
	Raw          json.RawMessage   `json:"-"`
	IsTeAdmin    bool              `json:"is_teadmin"`
	IsWhiteList  bool              `json:"is_white_list"`
	StartDate    string            `json:"start_date"` // trial window start, e.g. 2026-09-11
	EndDate      string            `json:"end_date"`   // trial window end / reset date, e.g. 2026-10-12
	Package      *PackageInfo      `json:"package"`
	Metrics      []MetricInfo      `json:"metrics"`
	ModelMetrics []json.RawMessage `json:"model_metrics"`
	Show         map[string]any    `json:"show"`
}

type PackageInfo struct {
	Status        string `json:"status"`
	SpecCode      string `json:"spec_code"`
	PackageNameEn string `json:"package_name_en"`
	PackageName   string `json:"package_name"`
	PackageNameCN string `json:"package_name_cn"`
	ExpireTime    string `json:"expire_time"`
	EffectiveTime string `json:"effective_time"`
	IsTokenPkg    bool   `json:"is_token_package"`
}

type MetricInfo struct {
	Name               string  `json:"name"`
	UsageTokenNum      float64 `json:"usage_token_num"`
	PackageTokenAmount float64 `json:"package_token_amount"`
	RemainTokenNum     float64 `json:"remain_token_num"`
	UsageRatio         float64 `json:"usage_ratio"`
}

// QuotaSummary is a compact view for the admin UI.
type QuotaSummary struct {
	PackageStatus  string  `json:"package_status"`
	PackageName    string  `json:"package_name"`
	ExpireTime     string  `json:"expire_time"`
	EffectiveTime  string  `json:"effective_time"`
	UsedTokens     float64 `json:"used_tokens"`
	PackageTokens  float64 `json:"package_tokens"`
	RemainTokens   float64 `json:"remain_tokens"`
	UsagePercent   float64 `json:"usage_percent"`
	MetricName     string  `json:"metric_name"`
	Period         string  `json:"period"`       // daily | monthly | package | unknown
	PeriodLabel    string  `json:"period_label"` // 每天 / 每月 / 套餐周期
	PeriodEvidence string  `json:"period_evidence,omitempty"`
	// Real upstream validity window (payload start_date / end_date).
	StartDate string `json:"start_date,omitempty"`
	EndDate   string `json:"end_date,omitempty"`
	// Reset schedule is upstream-owned. NextResetAt is either an explicit
	// upstream field (source=upstream) or a labelled fallback (estimated/expire).
	NextResetAt time.Time     `json:"next_reset_at,omitzero"`
	ResetSource string        `json:"reset_source,omitempty"`
	ResetNote   string        `json:"reset_note,omitempty"`
	Metrics     []MetricBrief `json:"metrics,omitempty"`
	FetchedAt   time.Time     `json:"fetched_at"`
	Raw         any           `json:"raw,omitempty"`
}

type MetricBrief struct {
	Name          string  `json:"name"`
	UsedTokens    float64 `json:"used_tokens"`
	PackageTokens float64 `json:"package_tokens"`
	RemainTokens  float64 `json:"remain_tokens"`
	UsagePercent  float64 `json:"usage_percent"`
}

// FetchSubscription pulls quota/usage from InferHub snap-manager.
// client may be nil (falls back to a 30s direct client).
func FetchSubscription(ctx context.Context, baseURL string, creds *OAuthTokenResult, client *http.Client) (*SubscriptionInfo, error) {
	if creds == nil || creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("oauth credentials required")
	}
	if baseURL == "" {
		baseURL = DefaultModelBase
	}
	u := strings.TrimRight(baseURL, "/") + PathQuotaPlugin
	headers, _, err := AKSKSign(creds.AccessKeyID, creds.SecretAccessKey, creds.SecurityToken, http.MethodGet, u, nil, map[string]string{
		"Accept": "application/json",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		// 先脱敏再按字符边界截断：错误信息会进面板与日志。
		return nil, fmt.Errorf("subscription %d: %s", resp.StatusCode, kit.Snippet(string(body), 300))
	}
	info := &SubscriptionInfo{Raw: append(json.RawMessage(nil), body...)}
	if err := json.Unmarshal(body, info); err != nil {
		return nil, err
	}
	return info, nil
}

func (info *SubscriptionInfo) Summary() QuotaSummary {
	s := QuotaSummary{FetchedAt: time.Now(), Raw: json.RawMessage(info.Raw)}
	if info.Package != nil {
		s.PackageStatus = info.Package.Status
		s.PackageName = info.Package.PackageName
		if s.PackageName == "" {
			s.PackageName = info.Package.PackageNameCN
		}
		if s.PackageName == "" {
			s.PackageName = info.Package.PackageNameEn
		}
		s.ExpireTime = info.Package.ExpireTime
		s.EffectiveTime = info.Package.EffectiveTime
	}
	// The trial window lives at the payload top level (start_date / end_date).
	// Mirror it into effective/expire when the package block is empty so the date
	// columns and the period detection use the real upstream window.
	s.StartDate = strings.TrimSpace(info.StartDate)
	s.EndDate = strings.TrimSpace(info.EndDate)
	if s.EffectiveTime == "" {
		s.EffectiveTime = s.StartDate
	}
	if s.ExpireTime == "" {
		s.ExpireTime = s.EndDate
	}
	for _, m := range info.Metrics {
		used := m.UsageTokenNum
		pkg := m.PackageTokenAmount
		remain := m.RemainTokenNum
		// 上游试用包常只给 package_token_amount / usage_token_num，不给 remain
		if remain <= 0 && pkg > 0 {
			remain = pkg - used
			if remain < 0 {
				remain = 0
			}
		}
		pct := 0.0
		if pkg > 0 {
			pct = used / pkg * 100
		} else if m.UsageRatio > 0 {
			pct = m.UsageRatio * 100
		}
		s.Metrics = append(s.Metrics, MetricBrief{
			Name: m.Name, UsedTokens: used, PackageTokens: pkg,
			RemainTokens: remain, UsagePercent: pct,
		})
		if m.Name == "usageTokenChatMessages" || s.MetricName == "" {
			s.MetricName = m.Name
			s.UsedTokens = used
			s.PackageTokens = pkg
			s.RemainTokens = remain
			s.UsagePercent = pct
		}
	}
	s.Period, s.PeriodLabel, s.PeriodEvidence = detectQuotaPeriod(info, s)
	s.NextResetAt, s.ResetSource, s.ResetNote = deriveNextQuotaReset(info, s, s.FetchedAt)
	return s
}

func detectQuotaPeriod(info *SubscriptionInfo, s QuotaSummary) (period, label, evidence string) {
	blob := strings.ToLower(s.PackageName + " " + s.MetricName)
	for _, m := range info.Metrics {
		blob += " " + strings.ToLower(m.Name)
	}
	if info.Show != nil {
		b, _ := json.Marshal(info.Show)
		blob += " " + strings.ToLower(string(b))
	}

	switch {
	case strings.Contains(blob, "daily") || strings.Contains(blob, "per_day") ||
		strings.Contains(blob, "per-day") || strings.Contains(blob, "day_quota") ||
		strings.Contains(s.PackageName, "日") || strings.Contains(s.PackageName, "每天"):
		return "daily", "每天", "metric/package name indicates daily"
	case strings.Contains(blob, "monthly") || strings.Contains(blob, "per_month") ||
		strings.Contains(blob, "per-month") || strings.Contains(blob, "month_quota") ||
		strings.Contains(s.PackageName, "月") || strings.Contains(s.PackageName, "每月"):
		return "monthly", "每月", "metric/package name indicates monthly"
	}

	// Infer from package window length.
	if d := packageWindowDays(s.EffectiveTime, s.ExpireTime); d > 0 {
		if d <= 2 {
			return "daily", "每天", fmt.Sprintf("package window ≈ %.1f day(s)", d)
		}
		if d >= 25 && d <= 40 {
			return "monthly", "每月", fmt.Sprintf("package window ≈ %.0f days", d)
		}
		// Trial packages are usually multi-month monthly-billed quotas (e.g. 90 days).
		if d >= 60 {
			return "package", "套餐周期", fmt.Sprintf("package window ≈ %.0f days (trial/subscription total)", d)
		}
		return "package", "套餐周期", fmt.Sprintf("package window ≈ %.0f days", d)
	}

	// CodeArts Agent trial defaults: token pool for the subscribed package period (not daily reset).
	if strings.Contains(strings.ToLower(s.PackageName), "trial") ||
		strings.Contains(s.PackageName, "试用") ||
		s.PackageTokens > 0 {
		return "package", "套餐周期", "no daily/monthly marker; CodeArts trial uses package-total tokens"
	}
	return "unknown", "未知", "insufficient signals"
}

func packageWindowDays(effective, expire string) float64 {
	t0 := parseFlexibleTime(effective)
	t1 := parseFlexibleTime(expire)
	if t0.IsZero() || t1.IsZero() || !t1.After(t0) {
		return 0
	}
	return t1.Sub(t0).Hours() / 24
}

func parseFlexibleTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339, time.RFC3339Nano,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006/01/02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---- quota reset schedule (upstream-owned) ----

// quotaResetZone anchors CodeArts quota days; upstream SG/CN regions settle on UTC+8.
var quotaResetZone = time.FixedZone("UTC+8", 8*3600)

// deriveNextQuotaReset reports when the quota is expected to roll over.
// Priority: explicit upstream reset field > real upstream window end
// (start_date/end_date or expire_time) > daily/monthly estimate.
// The schedule is owned by upstream, so estimates are labelled as such.
func deriveNextQuotaReset(info *SubscriptionInfo, s QuotaSummary, now time.Time) (time.Time, string, string) {
	if now.IsZero() {
		now = time.Now()
	}
	if t, note := findUpstreamReset(info, now); !t.IsZero() {
		return t, "upstream", note
	}
	if t, note := upstreamWindowEnd(s); !t.IsZero() {
		return t, "expire", note
	}
	n := now.In(quotaResetZone)
	switch s.Period {
	case "daily":
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, quotaResetZone).AddDate(0, 0, 1),
			"estimated", "上游未下发有效期/重置字段；按每天 00:00 (UTC+8) 估算"
	case "monthly":
		return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, quotaResetZone).AddDate(0, 1, 0),
			"estimated", "上游未下发有效期/重置字段；按每月 1 日 00:00 (UTC+8) 估算"
	}
	return time.Time{}, "", ""
}

// upstreamWindowEnd is the end of the upstream-declared validity window
// (payload start_date/end_date, else package expire_time). The quota resets or
// has to be re-subscribed when this moment passes.
func upstreamWindowEnd(s QuotaSummary) (time.Time, string) {
	end := strings.TrimSpace(s.EndDate)
	if end == "" {
		end = strings.TrimSpace(s.ExpireTime)
	}
	t := parseFlexibleTimeInZone(end)
	if t.IsZero() {
		return time.Time{}, ""
	}
	// Date-only values mean "valid through that whole day".
	if !strings.ContainsAny(end, ":T") {
		t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, quotaResetZone)
	}
	pkg := strings.TrimSpace(s.PackageName)
	if pkg == "" {
		pkg = "套餐"
	}
	note := "上游到期时间 " + end
	if s.StartDate != "" {
		note = fmt.Sprintf("上游有效期 %s ~ %s", s.StartDate, end)
	}
	note += fmt.Sprintf("（%s，到期后上游重置/换新包）", pkg)
	return t, note
}

// findUpstreamReset walks the raw upstream payload for reset-ish keys and parses
// values that look like timestamps (string or unix seconds/millis).
// Earliest future candidate wins; otherwise the most recent past one.
func findUpstreamReset(info *SubscriptionInfo, now time.Time) (time.Time, string) {
	if info == nil || len(info.Raw) == 0 {
		return time.Time{}, ""
	}
	var root any
	if err := json.Unmarshal(info.Raw, &root); err != nil {
		return time.Time{}, ""
	}
	type cand struct {
		path, raw string
		t         time.Time
	}
	var cands []cand
	appendCand := func(path, raw string, t time.Time) {
		if !t.IsZero() {
			cands = append(cands, cand{path: path, raw: raw, t: t})
		}
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(p, t[k])
			}
		case []any:
			for i, it := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), it)
			}
		case string:
			if isResetField(path) {
				appendCand(path, t, parseFlexibleTimeInZone(t))
			}
		case float64:
			if isResetField(path) {
				secs := int64(t)
				if secs > 1e12 { // milliseconds
					secs /= 1000
				}
				if secs >= 1_000_000_000 && secs <= 4_000_000_000 {
					appendCand(path, strconv.FormatInt(int64(t), 10), time.Unix(secs, 0))
				}
			}
		}
	}
	walk("", root)
	if len(cands) == 0 {
		return time.Time{}, ""
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].t.Before(cands[j].t) })
	for _, c := range cands {
		if c.t.After(now) {
			return c.t, fmt.Sprintf("上游字段 %s=%s", c.path, c.raw)
		}
	}
	last := cands[len(cands)-1]
	return last.t, fmt.Sprintf("上游字段 %s=%s（已过期）", last.path, last.raw)
}

func isResetField(path string) bool {
	p := strings.ToLower(path)
	return strings.Contains(p, "reset") || strings.Contains(p, "next_cycle") || strings.Contains(p, "nextcycle")
}

// parseFlexibleTimeInZone is parseFlexibleTime with an explicit fallback zone for
// zone-less timestamps (upstream sends CodeArts local time).
func parseFlexibleTimeInZone(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339, time.RFC3339Nano,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
		"2006/01/02 15:04:05",
		"2006/01/02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, quotaResetZone); err == nil {
			return t
		}
	}
	return time.Time{}
}
