package server

import (
	"context"
	"net/http"
	"time"
)

// handleUsage 汇总「能通过接口拿到的信息」：账号、额度/套餐、模型、缓存与 token 用量。
//
// GET /v1/usage（别名 /capy/account）
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	acc, err := s.store.Next()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error(), "no_account")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	sess := s.sessionFor(acc)

	out := map[string]any{
		"account": map[string]any{
			"id": acc.ID, "name": acc.Name, "email": acc.Email, "user_id": acc.UserID,
			"org_id": acc.OrgID, "org_name": acc.OrgName, "device_id": acc.DeviceID,
			"mode": firstNonEmpty(acc.Mode, s.cfg().Capy.Mode), "state": acc.State,
		},
	}

	// 额度/套餐（没套餐时 balance 为 null、plans 为空）。
	var billing map[string]any
	if err := sess.Do(ctx, "GET", "/app/orgs/"+acc.OrgID+"/billing", acc.OrgID, nil, &billing); err == nil {
		out["billing"] = map[string]any{
			"balance":              billing["balance"],
			"paid_allowed":         billing["paidAllowed"],
			"plans":                billing["plans"],
			"sponsored_allowances": billing["sponsoredAllowances"],
			"machine_memory":       billing["machineMemory"],
			"auto_reload":          billing["autoReload"],
			"default_machine_size": billing["defaultMachineSize"],
		}
	} else {
		out["billing_error"] = err.Error()
	}

	// 可用模型（含价格/上下文/推理档位）。
	if models, err := s.catalog(ctx, acc, false); err == nil {
		list := make([]map[string]any, 0, len(models))
		for _, m := range models {
			item := map[string]any{"id": m.ID, "name": m.Name, "pricing": m.Pricing, "efforts": m.Efforts}
			if m.Limits != nil {
				item["context"] = m.Limits.Context
				item["output"] = m.Limits.Output
			}
			list = append(list, item)
		}
		out["models"] = list
	}

	// 本地统计：请求量、token、缓存命中率、耗时（来自网关自己的请求日志）。
	total, ok, failed, avg := s.log.Stats()
	_, promptTokens, outputTokens, cacheTokens := s.log.Totals()
	ratio := 0.0
	if promptTokens > 0 {
		ratio = float64(cacheTokens) / float64(promptTokens)
	}
	out["local"] = map[string]any{
		"requests":          total,
		"ok":                ok,
		"failed":            failed,
		"avg_ms":            avg,
		"prompt_tokens":     promptTokens,
		"completion_tokens": outputTokens,
		"cache_read_tokens": cacheTokens,
		"cache_hit_ratio":   ratio,
		"window_requests":   len(s.log.Recent(300)),
	}

	// 按模型聚合的统计（与 /metrics 同源）。
	_, modelStats, totalStats := statsSnapshot()
	out["model_stats"] = map[string]any{"total": totalStats, "models": modelStats}

	// 最近一次请求的上游明细（成本/缓存来源）。
	recent := s.log.Recent(1)
	if len(recent) > 0 {
		out["last_request"] = recent[0]
	}
	writeJSON(w, http.StatusOK, out)
}
