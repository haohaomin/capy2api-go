package server

import (
	"net/http"
	"strings"

	"github.com/haohaomin/capy2api/internal/capy"
)

// modelView 把上游模型转成 OpenAI /v1/models 条目（附带 Capy 元数据）。
func modelView(m capy.Model) map[string]any {
	view := map[string]any{
		"id":       m.ID,
		"object":   "model",
		"created":  0,
		"owned_by": firstNonEmpty(m.Provider, "capy"),
		"name":     m.Name,
		"pricing":  m.Pricing,
		"ads":      m.Ads != nil && *m.Ads,
		"efforts":  m.Efforts,
	}
	if m.Limits != nil {
		view["context_window"] = m.Limits.Context
		view["context_length"] = m.Limits.Context // 兼容读 context_length 的客户端
		view["max_output_tokens"] = m.Limits.Output
		view["max_completion_tokens"] = m.Limits.Output
	}
	if m.DefaultEffort != "" {
		view["default_effort"] = m.DefaultEffort
	}
	if m.Route != nil {
		view["route"] = m.Route.Kind
	}
	view["capy"] = map[string]any{
		"ads":           m.Ads != nil && *m.Ads,
		"auth":          m.Auth,
		"byok":          m.BYOK,
		"pricing":       m.Pricing,
		"limits":        m.Limits,
		"efforts":       m.Efforts,
		"defaultEffort": m.DefaultEffort,
	}
	return view
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	acc, err := s.store.Next()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error(), "no_account")
		return
	}
	models, err := s.catalog(r.Context(), acc, false)
	if err != nil {
		s.failAccount(acc, err)
		writeAPIError(w, http.StatusBadGateway, "拉取模型目录失败: "+err.Error(), "upstream_error")
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, modelView(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	acc, err := s.store.Next()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error(), "no_account")
		return
	}
	models, err := s.catalog(r.Context(), acc, false)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	model, err := resolveModel(id, models)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error(), "model_not_found")
		return
	}
	writeJSON(w, http.StatusOK, modelView(model))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
