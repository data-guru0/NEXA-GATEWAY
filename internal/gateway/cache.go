package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evolvue/nexa-gateway/internal/store"
	"github.com/google/uuid"
)

// Response caching for /v1/chat/completions.
//
// Exact: a request identical to an earlier one (model, messages and every
// parameter except stream options) gets the stored answer back.
// Semantic: the conversation is embedded with the configured embedding model
// and compared, by cosine similarity, with earlier conversations sent to the
// same model with the same parameters; at or above the threshold the stored
// answer is returned.
//
// Entries live in Redis and are shared by every Nexa instance: exact answers
// as keys with a TTL, semantic answers in a RediSearch HNSW vector index.

type cacheState struct {
	mu       sync.RWMutex
	settings *store.CacheSettings
}

// CacheSettings returns the live settings.
func (g *Gateway) CacheSettings() store.CacheSettings {
	g.cache.mu.RLock()
	c := g.cache.settings
	g.cache.mu.RUnlock()
	if c != nil {
		return *c
	}
	loaded := g.Store.CacheSettings()
	g.cache.mu.Lock()
	g.cache.settings = &loaded
	g.cache.mu.Unlock()
	return loaded
}

// SetCacheSettings saves and applies new settings.
func (g *Gateway) SetCacheSettings(c store.CacheSettings) (store.CacheSettings, error) {
	saved, err := g.Store.SetCacheSettings(c)
	if err != nil {
		return saved, err
	}
	return saved, nil // the store publishes the change; every instance reloads
}

// ClearCache empties the shared cache.
func (g *Gateway) ClearCache(ctx context.Context) (int64, error) { return g.Store.ClearCache(ctx) }

// cachePending carries a miss from lookup to store.
type cachePending struct {
	settings  store.CacheSettings
	exactKey  string
	partition string
	model     string
	vector    []float32
	meta      map[string]any
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v) // encoding/json sorts map keys, so equal requests hash equally
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// cacheKeys returns the exact-request key and the semantic partition (same
// model and parameters, same number of turns).
func cacheKeys(payload map[string]any, messages []any) (string, string) {
	req, params := map[string]any{}, map[string]any{}
	for k, v := range payload {
		switch k {
		case "stream", "stream_options", "user", "metadata", "store":
			continue
		}
		req[k] = v
		if k != "messages" {
			params[k] = v
		}
	}
	params["_turns"] = len(messages)
	return hashJSON(req), hashJSON(params)
}

// conversationText is what the semantic cache embeds. Non-text content (images,
// audio) returns "" and disables semantic matching for that request.
func conversationText(messages []any) string {
	var b strings.Builder
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		content, ok := msg["content"].(string)
		if !ok {
			return ""
		}
		fmt.Fprintf(&b, "%s: %s\n", msg["role"], content)
	}
	text := b.String()
	if len(text) > 16000 {
		text = text[len(text)-16000:]
	}
	return text
}

func cacheable(r *http.Request, payload map[string]any, model string) bool {
	if strings.EqualFold(r.Header.Get("X-Nexa-Cache"), "off") || strings.HasPrefix(model, "feedback/") {
		return false
	}
	for _, k := range []string{"tools", "functions", "tool_choice"} {
		if _, ok := payload[k]; ok {
			return false
		}
	}
	if n, ok := toFloat(payload["n"]); ok && n > 1 {
		return false
	}
	return true
}

// embed returns a unit-length embedding of text and what it cost.
func (g *Gateway) embed(ctx context.Context, c store.CacheSettings, text string) ([]float32, float64, error) {
	p, err := g.Store.Provider(c.EmbeddingProviderID)
	if err != nil || !p.Enabled {
		return nil, 0, errors.New("embedding provider is missing or paused")
	}
	if strings.EqualFold(p.Type, "anthropic") {
		return nil, 0, errors.New("Anthropic has no embeddings endpoint")
	}
	body, _ := json.Marshal(map[string]any{"model": c.EmbeddingModel, "input": text})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	applyHeaders(req.Header, p.Headers)
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode >= 300 || len(out.Data) == 0 {
		msg := out.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("embedding request failed (%d)", resp.StatusCode)
		}
		return nil, 0, errors.New(msg)
	}
	vec, norm := make([]float32, len(out.Data[0].Embedding)), 0.0
	for _, x := range out.Data[0].Embedding {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return nil, 0, errors.New("empty embedding")
	}
	for i, x := range out.Data[0].Embedding {
		vec[i] = float32(x / norm)
	}
	return vec, g.estimateCost(c.EmbeddingModel, out.Usage.PromptTokens, 0), nil
}

// cacheLookup answers from the cache when it can. When it cannot, it returns
// what cacheSave needs to store the answer that is about to be generated.
func (g *Gateway) cacheLookup(w http.ResponseWriter, r *http.Request, payload map[string]any, messages []any, model string, t *store.Trace, start time.Time) (bool, *cachePending) {
	c := g.CacheSettings()
	if (!c.ExactEnabled && !c.SemanticEnabled) || !cacheable(r, payload, model) {
		return false, nil
	}
	exact, partition := cacheKeys(payload, messages)
	pc := &cachePending{settings: c, exactKey: exact, partition: partition, model: model, meta: map[string]any{"result": "miss"}}
	refresh := strings.EqualFold(r.Header.Get("X-Nexa-Cache"), "refresh")
	if c.ExactEnabled && !refresh {
		if e, err := g.Store.CacheByKey(r.Context(), exact); err == nil {
			g.serveCached(w, payload, e, "exact", 1, t, start)
			return true, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			pc.meta["cache_error"] = err.Error() // a cache outage must never fail the request
		}
	}
	if c.SemanticEnabled {
		if text := conversationText(messages); text != "" {
			started := time.Now()
			vec, cost, err := g.embed(r.Context(), c, text)
			pc.meta["embedding_ms"] = math.Round(msSince(started))
			t.ExtraCostUSD += cost
			if err != nil {
				pc.meta["semantic_error"] = err.Error()
			} else {
				pc.vector = vec
				if !refresh {
					e, sim, err := g.Store.CacheNearest(r.Context(), partition, vec)
					switch {
					case err == nil && sim >= c.Threshold:
						g.serveCached(w, payload, e, "semantic", sim, t, start, pc.meta["embedding_ms"])
						return true, nil
					case err == nil:
						pc.meta["nearest_similarity"] = math.Round(sim*10000) / 10000
					case !errors.Is(err, sql.ErrNoRows):
						pc.meta["cache_error"] = err.Error()
					}
				}
			}
		}
	}
	w.Header().Set("X-Nexa-Cache", "MISS")
	return false, pc
}

// serveCached writes a stored answer in the shape the client asked for.
func (g *Gateway) serveCached(w http.ResponseWriter, payload map[string]any, e store.CacheEntry, kind string, similarity float64, t *store.Trace, start time.Time, embeddingMS ...any) {
	res := e.Response
	id := "chatcmpl-nexa-cache-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	created := time.Now().Unix()
	usage := map[string]any{"prompt_tokens": res.InputTokens, "completion_tokens": res.OutputTokens, "total_tokens": res.InputTokens + res.OutputTokens}
	w.Header().Set("X-Nexa-Cache", "HIT-"+strings.ToUpper(kind))
	w.Header().Set("X-Nexa-Cache-Similarity", strconv.FormatFloat(similarity, 'f', 4, 64))
	w.Header().Set("X-Nexa-Routed-Model", res.Model)
	t.ProviderName, t.Model, t.StatusCode, t.Status, t.Response, t.FinishReason = "Nexa cache", res.Model, 200, "success", res.Content, res.FinishReason
	meta := map[string]any{"result": kind, "similarity": math.Round(similarity*10000) / 10000, "entry_id": e.ID, "source_trace_id": e.SourceTraceID, "saved_usd": e.CostUSD, "cached_at": e.CreatedAt, "served_by": res.Provider}
	if len(embeddingMS) > 0 {
		meta["embedding_ms"] = embeddingMS[0]
	}
	mergeMetadata(t, "cache", meta)
	if stream, _ := payload["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		chunk := func(delta map[string]any, finish any) {
			b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": res.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		t.TTFTMS = msSince(start)
		chunk(map[string]any{"role": "assistant", "content": res.Content}, nil)
		chunk(map[string]any{}, res.FinishReason)
		if clientWantsUsage(payload) {
			b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": res.Model, "choices": []any{}, "usage": usage})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "chat.completion", "created": created, "model": res.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": res.Content}, "finish_reason": res.FinishReason}}, "usage": usage})
}

// cacheSave stores a freshly generated answer when it is a complete text reply.
func (g *Gateway) cacheSave(pc *cachePending, t *store.Trace) {
	if pc == nil {
		return
	}
	defer mergeMetadata(t, "cache", pc.meta)
	if t.Status != "success" || t.FinishReason != "stop" || t.Response == "" || len(t.Response) >= maxTraceBody || strings.Contains(t.Response, "**Tool calls**") {
		return
	}
	cost := t.CostUSD
	if cost == 0 {
		cost = g.estimateCost(t.Model, t.InputTokens, t.OutputTokens)
	}
	e := store.CacheEntry{ID: uuid.NewString(), Partition: pc.partition, Model: pc.model, SourceTraceID: t.ID, CostUSD: cost, CreatedAt: time.Now().UTC(),
		Response: store.CachedResponse{Model: t.Model, Provider: t.ProviderName, Content: t.Response, FinishReason: t.FinishReason, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // the client may already be gone
	defer cancel()
	if err := g.Store.PutCacheEntry(ctx, e, pc.exactKey, pc.vector, time.Duration(pc.settings.TTLSeconds)*time.Second); err != nil {
		slog.Warn("could not store cache entry", "error", err.Error())
		pc.meta["cache_error"] = err.Error()
		return
	}
	pc.meta["stored"] = true
}

// mergeMetadata sets one key in the trace's JSON metadata.
func mergeMetadata(t *store.Trace, key string, value any) {
	m := map[string]any{}
	if t.Metadata != "" {
		_ = json.Unmarshal([]byte(t.Metadata), &m)
	}
	m[key] = value
	b, _ := json.Marshal(m)
	t.Metadata = string(b)
}
