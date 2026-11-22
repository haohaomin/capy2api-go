// metrics.go 请求统计：Prometheus 文本端点 + 上游劣化滑窗告警。
//
// 借鉴 workbuddy2api-panel 的 metrics_prom.go / upstreamhealth.go：
// 按模型累计请求、token、缓存与耗时；5 分钟滑窗里「慢请求」或「失败」占比过高时
// 只打一条聚合 WARN，避免每请求刷日志。
package server

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/haohaomin/capy2api/internal/capy"
)

const (
	metricsModelCap = 128
	healthWindow    = 5 * time.Minute
	healthBuckets   = 12
	healthWarnEvery = 10 * time.Minute
	healthMinSample = 8
	healthSlow      = 8 * time.Second
	healthSlowRatio = 0.30
	healthFailRatio = 0.10
	healthMinFail   = 3
)

type modelAcc struct {
	requests     int64
	success      int64
	failed       int64
	streaming    int64
	promptTokens int64
	completion   int64
	cacheTokens  int64
	latSumMS     float64
	latN         int64
	ttfbSumMS    float64
	ttfbN        int64
	tpsSum       float64
	tpsN         int64
	genSumMS     float64
	genN         int64
}

var reqStats = struct {
	mu    sync.Mutex
	since time.Time
	by    map[string]*modelAcc
}{since: time.Now(), by: map[string]*modelAcc{}}

// noteRequest 记录一次请求结果（模型维度）。
func noteRequest(model string, ok, stream bool, u capy.Usage, dur time.Duration, res *capy.TurnResult) {
	if model == "" {
		model = "-"
	}
	reqStats.mu.Lock()
	defer reqStats.mu.Unlock()
	acc := reqStats.by[model]
	if acc == nil {
		if len(reqStats.by) >= metricsModelCap {
			return
		}
		acc = &modelAcc{}
		reqStats.by[model] = acc
	}
	acc.requests++
	if ok {
		acc.success++
	} else {
		acc.failed++
	}
	if stream {
		acc.streaming++
	}
	acc.promptTokens += int64(u.InputTokens)
	acc.completion += int64(u.OutputTokens)
	acc.cacheTokens += int64(u.CacheReadTokens)
	acc.latSumMS += float64(dur.Milliseconds())
	acc.latN++
	if res != nil {
		if res.TTFTMS > 0 {
			acc.ttfbSumMS += float64(res.TTFTMS)
			acc.ttfbN++
		}
		if res.GenMS > 0 {
			acc.genSumMS += float64(res.GenMS)
			acc.genN++
		}
		if res.TokPerSec > 0 {
			acc.tpsSum += res.TokPerSec
			acc.tpsN++
		}
	}
}

// ModelStat 是面板/接口用的单模型统计。
type ModelStat struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	Failed           int64   `json:"failed"`
	Streaming        int64   `json:"streaming"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheTokens      int64   `json:"cache_tokens"`
	AvgLatencyMS     float64 `json:"avg_latency_ms"`
	CacheHitRatio    float64 `json:"cache_hit_ratio"`
	AvgTTFTMS        float64 `json:"avg_ttft_ms"`
	AvgGenMS         float64 `json:"avg_gen_ms"`
	TokensPerSecond  float64 `json:"tokens_per_second"`
}

// StatsSnapshot 汇总所有模型统计。
func statsSnapshot() (time.Time, []ModelStat, ModelStat) {
	reqStats.mu.Lock()
	defer reqStats.mu.Unlock()
	out := make([]ModelStat, 0, len(reqStats.by))
	var total ModelStat
	var ttfbSum, genSum, tpsSum float64
	var ttfbN, genN, tpsN int64
	for name, a := range reqStats.by {
		item := ModelStat{
			Model: name, Requests: a.requests, Success: a.success, Failed: a.failed,
			Streaming: a.streaming, PromptTokens: a.promptTokens,
			CompletionTokens: a.completion, CacheTokens: a.cacheTokens,
		}
		if a.latN > 0 {
			item.AvgLatencyMS = a.latSumMS / float64(a.latN)
		}
		if a.promptTokens > 0 {
			item.CacheHitRatio = float64(a.cacheTokens) / float64(a.promptTokens)
		}
		if a.ttfbN > 0 {
			item.AvgTTFTMS = a.ttfbSumMS / float64(a.ttfbN)
		}
		if a.genN > 0 {
			item.AvgGenMS = a.genSumMS / float64(a.genN)
		}
		if a.tpsN > 0 {
			item.TokensPerSecond = a.tpsSum / float64(a.tpsN)
		}
		ttfbSum += a.ttfbSumMS
		ttfbN += a.ttfbN
		genSum += a.genSumMS
		genN += a.genN
		tpsSum += a.tpsSum
		tpsN += a.tpsN
		out = append(out, item)
		total.Requests += item.Requests
		total.Success += item.Success
		total.Failed += item.Failed
		total.Streaming += item.Streaming
		total.PromptTokens += item.PromptTokens
		total.CompletionTokens += item.CompletionTokens
		total.CacheTokens += item.CacheTokens
	}
	total.Model = "total"
	if total.PromptTokens > 0 {
		total.CacheHitRatio = float64(total.CacheTokens) / float64(total.PromptTokens)
	}
	if total.Requests > 0 {
		var sum float64
		for _, item := range out {
			sum += item.AvgLatencyMS * float64(item.Requests)
		}
		total.AvgLatencyMS = sum / float64(total.Requests)
	}
	if ttfbN > 0 {
		total.AvgTTFTMS = ttfbSum / float64(ttfbN)
	}
	if genN > 0 {
		total.AvgGenMS = genSum / float64(genN)
	}
	if tpsN > 0 {
		total.TokensPerSecond = tpsSum / float64(tpsN)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return reqStats.since, out, total
}

// ---------------------------------------------------------------- 滑窗告警

type healthBucket struct {
	id                int64
	total, slow, fail int
}

var health = struct {
	mu       sync.Mutex
	buckets  [healthBuckets]healthBucket
	lastWarn time.Time
}{}

// recordHealth 采样一条请求；劣化时聚合告警（默认 10 分钟一次）。
func recordHealth(ok bool, dur time.Duration) {
	now := time.Now()
	width := int64(healthWindow / healthBuckets)
	id := now.UnixNano() / width

	health.mu.Lock()
	defer health.mu.Unlock()
	b := &health.buckets[id%healthBuckets]
	if b.id != id {
		*b = healthBucket{id: id}
	}
	b.total++
	if !ok {
		b.fail++
	} else if dur >= healthSlow {
		b.slow++
	}
	var total, slow, fail int
	for _, bucket := range health.buckets {
		if bucket.id <= id-healthBuckets || bucket.id > id {
			continue
		}
		total += bucket.total
		slow += bucket.slow
		fail += bucket.fail
	}
	if total < healthMinSample {
		return
	}
	slowBad := float64(slow) >= float64(total)*healthSlowRatio
	failBad := fail >= healthMinFail && float64(fail) >= float64(total)*healthFailRatio
	if !slowBad && !failBad {
		return
	}
	if !health.lastWarn.IsZero() && now.Sub(health.lastWarn) < healthWarnEvery {
		return
	}
	health.lastWarn = now
	fmt.Printf("[capy2api] WARN 上游劣化：最近 %s 共 %d 条请求，慢（>=%s）%d 条，失败 %d 条\n",
		healthWindow, total, healthSlow, slow, fail)
}

// ---------------------------------------------------------------- /metrics

// handleMetrics 输出 Prometheus 文本格式（0.0.4），零第三方依赖。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsPublic() && !s.keyOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	since, models, total := statsSnapshot()
	w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
	buf := make([]byte, 0, 4096)
	add := func(format string, args ...any) { buf = append(buf, fmt.Sprintf(format, args...)...) }

	add("# HELP capy2api_up 网关进程存活\n# TYPE capy2api_up gauge\ncapy2api_up 1\n")
	add("# HELP capy2api_stats_since_seconds 统计起点\n# TYPE capy2api_stats_since_seconds gauge\ncapy2api_stats_since_seconds %d\n", since.Unix())
	add("# HELP capy2api_requests_total 请求总数\n# TYPE capy2api_requests_total counter\n")
	for _, m := range models {
		add("capy2api_requests_total{model=%q,status=\"ok\"} %d\n", m.Model, m.Success)
		add("capy2api_requests_total{model=%q,status=\"error\"} %d\n", m.Model, m.Failed)
	}
	add("# HELP capy2api_stream_requests_total 流式请求数\n# TYPE capy2api_stream_requests_total counter\n")
	for _, m := range models {
		add("capy2api_stream_requests_total{model=%q} %d\n", m.Model, m.Streaming)
	}
	add("# HELP capy2api_tokens_total token 用量\n# TYPE capy2api_tokens_total counter\n")
	for _, m := range models {
		add("capy2api_tokens_total{model=%q,kind=\"prompt\"} %d\n", m.Model, m.PromptTokens)
		add("capy2api_tokens_total{model=%q,kind=\"completion\"} %d\n", m.Model, m.CompletionTokens)
		add("capy2api_tokens_total{model=%q,kind=\"cache_read\"} %d\n", m.Model, m.CacheTokens)
	}
	add("# HELP capy2api_request_duration_ms 平均请求耗时（毫秒）\n# TYPE capy2api_request_duration_ms gauge\n")
	for _, m := range models {
		add("capy2api_request_duration_ms{model=%q} %.1f\n", m.Model, m.AvgLatencyMS)
	}
	add("# HELP capy2api_cache_hit_ratio 缓存命中率\n# TYPE capy2api_cache_hit_ratio gauge\n")
	for _, m := range models {
		add("capy2api_cache_hit_ratio{model=%q} %.4f\n", m.Model, m.CacheHitRatio)
	}
	add("# HELP capy2api_ttfb_ms 首字延时（毫秒，实时通道观测）\n# TYPE capy2api_ttfb_ms gauge\n")
	for _, m := range models {
		add("capy2api_ttfb_ms{model=%q} %.1f\n", m.Model, m.AvgTTFTMS)
	}
	add("# HELP capy2api_tokens_per_second 生成速度（tokens/s）\n# TYPE capy2api_tokens_per_second gauge\n")
	for _, m := range models {
		add("capy2api_tokens_per_second{model=%q} %.2f\n", m.Model, m.TokensPerSecond)
	}
	accounts := s.store.List()
	ready, cooling, invalid := 0, 0, 0
	for _, a := range accounts {
		switch {
		case !a.Enabled || a.State == "invalid":
			invalid++
		case a.State == "cooldown":
			cooling++
		default:
			ready++
		}
	}
	add("# HELP capy2api_accounts 账号状态\n# TYPE capy2api_accounts gauge\n")
	add("capy2api_accounts{state=\"ready\"} %d\ncapy2api_accounts{state=\"cooldown\"} %d\ncapy2api_accounts{state=\"invalid\"} %d\n", ready, cooling, invalid)
	add("# HELP capy2api_requests_hint 汇总\n# TYPE capy2api_requests_hint gauge\ncapy2api_requests_hint{kind=\"total\"} %d\n", total.Requests)
	_, _ = w.Write(buf)
}

// metricsPublic 是否允许不带密钥抓 /metrics（默认不允许）。
func (s *Server) metricsPublic() bool { return false }
