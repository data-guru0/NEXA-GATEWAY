package server

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/evolvue/nexa-gateway/internal/store"
	"github.com/go-chi/chi/v5"
)

func traceFilters(r *http.Request) store.TraceQuery {
	q := r.URL.Query()
	f := store.TraceQuery{Search: q.Get("search"), Status: q.Get("status"), ProviderID: q.Get("provider"), Model: q.Get("model")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	f.From, _ = time.Parse(time.RFC3339, q.Get("from"))
	f.To, _ = time.Parse(time.RFC3339, q.Get("to"))
	return f
}

var exportColumns = []string{"created_at", "id", "request_id", "source", "api_key_name", "provider_name", "model", "status", "status_code", "stream", "input_tokens", "output_tokens", "total_tokens", "cached_tokens", "reasoning_tokens", "latency_ms", "ttft_ms", "upstream_latency_ms", "gateway_latency_ms", "cost_usd", "finish_reason", "error", "prompt", "response", "metadata"}

// exportTraces streams every trace matching the dashboard filters as CSV or JSON Lines.
func (s *Server) exportTraces(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "csv" && format != "jsonl" {
		writeError(w, 400, "format must be csv or jsonl")
		return
	}
	name := fmt.Sprintf("nexa-traces-%s.%s", time.Now().UTC().Format("2006-01-02-150405"), format)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	f := traceFilters(r)
	var err error
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		enc := json.NewEncoder(w)
		err = s.Store.EachTrace(f, func(t store.Trace) error { return enc.Encode(t) })
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		cw := csv.NewWriter(w)
		_ = cw.Write(exportColumns)
		f64 := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
		err = s.Store.EachTrace(f, func(t store.Trace) error {
			return cw.Write([]string{t.CreatedAt.UTC().Format(time.RFC3339Nano), t.ID, t.RequestID, t.Source, t.APIKeyName, t.ProviderName, t.Model, t.Status, strconv.Itoa(t.StatusCode), strconv.FormatBool(t.Stream),
				strconv.Itoa(t.InputTokens), strconv.Itoa(t.OutputTokens), strconv.Itoa(t.TotalTokens), strconv.Itoa(t.CachedTokens), strconv.Itoa(t.ReasoningTokens),
				strconv.FormatInt(t.LatencyMS, 10), f64(t.TTFTMS), f64(t.UpstreamMS), f64(t.GatewayMS), f64(t.CostUSD), t.FinishReason, t.Error, t.Prompt, t.Response, t.Metadata})
		})
		cw.Flush()
	}
	if err != nil {
		s.Log.Warn("trace export failed", "error", err.Error()) // headers are already sent
	}
}

func (s *Server) setKeyLimits(w http.ResponseWriter, r *http.Request) {
	var in store.KeyLimits
	if !decode(w, r, &in) {
		return
	}
	k, err := s.Store.SetAPIKeyLimits(chi.URLParam(r, "id"), in)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "API key not found.")
		return
	}
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	k.SpentMonth = s.Store.KeySpend(k.ID, store.MonthStart(time.Now()))
	writeJSON(w, 200, k)
}

func (s *Server) cacheInfo(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-7 * 24 * time.Hour)
	writeJSON(w, 200, map[string]any{"settings": s.Gateway.CacheSettings(), "stats": s.Store.CacheStats(r.Context(), since), "stats_period": "7d"})
}

func (s *Server) setCache(w http.ResponseWriter, r *http.Request) {
	var in store.CacheSettings
	if !decode(w, r, &in) {
		return
	}
	c, err := s.Gateway.SetCacheSettings(in)
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) clearCache(w http.ResponseWriter, r *http.Request) {
	n, err := s.Gateway.ClearCache(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"removed": n})
}

// concludeFeedbackLoop sends all of a loop's traffic to one model (the leader by default).
func (s *Server) concludeFeedbackLoop(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ArmID string `json:"arm_id"`
	}
	if r.ContentLength > 0 && !decode(w, r, &in) {
		return
	}
	l, err := s.Store.FeedbackLoop(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "Feedback loop not found.")
		return
	}
	if in.ArmID == "" {
		in.ArmID = s.Gateway.LoopLeader(l, s.Gateway.LoopSplit(l)).ArmID
	}
	v, err := s.Store.ConcludeFeedbackLoop(l.ID, in.ArmID, "manual")
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	v.JevAPIKey = ""
	writeJSON(w, 200, v)
}
