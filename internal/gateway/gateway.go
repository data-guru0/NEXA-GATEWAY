package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"time"

	routing "github.com/evolvue/nexa-gateway/internal/router"
	"github.com/evolvue/nexa-gateway/internal/store"
	"github.com/google/uuid"
)

const maxTraceBody = 2 << 20

var datedModelSuffix = regexp.MustCompile(`-(?:\d{4}|\d{4}-\d{2}-\d{2})$`)

type Gateway struct {
	Store  *store.Store
	Client *http.Client
	Router *routing.Engine
}

type chatRequest struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type usage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Total      int `json:"total_tokens"`
}

func New(s *store.Store) *Gateway {
	return &Gateway{Store: s, Client: &http.Client{Timeout: 10 * time.Minute}, Router: routing.New(s)}
}

func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Request body is too large or invalid.", "invalid_request_error")
		return
	}
	var cr chatRequest
	if err := json.Unmarshal(body, &cr); err != nil || cr.Model == "" || len(cr.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "model and messages are required.", "invalid_request_error")
		return
	}
	if cr.Model == "smart" || strings.HasPrefix(cr.Model, "smart/") {
		g.smartChatCompletions(w, r, body, cr, started)
		return
	}
	providerRef, upstreamModel := splitModel(cr.Model)
	if h := strings.TrimSpace(r.Header.Get("X-Nexa-Provider")); h != "" {
		providerRef = h
		upstreamModel = cr.Model
	}
	if providerRef == "" {
		providers, _ := g.Store.Providers()
		if len(providers) == 1 {
			providerRef = providers[0].ID
			upstreamModel = cr.Model
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "Use provider/model (for example openai/gpt-4o-mini) or send X-Nexa-Provider.", "invalid_request_error")
			return
		}
	}
	p, err := g.Store.Provider(providerRef)
	if err != nil || !p.Enabled {
		writeOpenAIError(w, http.StatusNotFound, "Provider not found or disabled.", "invalid_request_error")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOpenAIError(w, 400, "Invalid JSON.", "invalid_request_error")
		return
	}
	payload["model"] = upstreamModel
	normalizeCompatiblePayload(p.Type, upstreamModel, payload)
	upBody, _ := json.Marshal(payload)
	prompt := currentTraceInput(cr.Messages)
	trace := store.Trace{ProviderID: p.ID, ProviderName: p.Name, Model: upstreamModel, Prompt: prompt, Status: "error"}
	if strings.EqualFold(p.Type, "anthropic") {
		g.forwardAnthropic(w, r, p, payload, cr.Stream, &trace)
	} else {
		g.forwardCompatible(w, r, p, upBody, cr.Stream, &trace)
	}
	totalMS := float64(time.Since(started)) / float64(time.Millisecond)
	trace.LatencyMS = int64(totalMS)
	trace.GatewayMS = totalMS - trace.UpstreamMS
	if trace.GatewayMS < 0 {
		trace.GatewayMS = 0
	}
	trace.CostUSD = estimateCost(upstreamModel, trace.InputTokens, trace.OutputTokens)
	_ = g.Store.AddTrace(trace)
}

type routeAttempt struct {
	TargetID   string  `json:"target_id"`
	Route      string  `json:"route"`
	Attempt    int     `json:"attempt"`
	StatusCode int     `json:"status_code"`
	LatencyMS  float64 `json:"latency_ms"`
	Retryable  bool    `json:"retryable"`
	Error      string  `json:"error,omitempty"`
}

func (g *Gateway) smartChatCompletions(w http.ResponseWriter, r *http.Request, body []byte, cr chatRequest, started time.Time) {
	var profile store.RoutingProfile
	var err error
	if cr.Model == "smart" {
		profile, err = g.Store.ActiveRoutingProfile()
	} else {
		profile, err = g.Store.RoutingProfile(strings.TrimPrefix(cr.Model, "smart/"))
	}
	if err != nil || !profile.Active && cr.Model == "smart" {
		writeOpenAIError(w, 404, "No active smart routing profile is configured.", "routing_error")
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		writeOpenAIError(w, 400, "Invalid JSON.", "invalid_request_error")
		return
	}
	prompt := routing.PromptFromMessages(cr.Messages)
	decision, err := g.Router.Decide(r.Context(), profile, prompt, payload)
	if err != nil {
		writeOpenAIError(w, 503, err.Error(), "routing_error")
		return
	}
	attempts := []routeAttempt{}
	var final *httptest.ResponseRecorder
	var finalTrace store.Trace
	for _, ranked := range decision.Ranked {
		for n := 0; n <= profile.MaxRetries; n++ {
			attemptStarted := time.Now()
			attemptPayload := clonePayload(payload)
			attemptPayload["model"] = ranked.Target.Model
			normalizeCompatiblePayload(ranked.Provider.Type, ranked.Target.Model, attemptPayload)
			upBody, _ := json.Marshal(attemptPayload)
			recorder := httptest.NewRecorder()
			trace := store.Trace{ProviderID: ranked.Provider.ID, ProviderName: ranked.Provider.Name, Model: ranked.Target.Model, Prompt: currentTraceInput(cr.Messages), Status: "error"}
			ctx, cancel := context.WithTimeout(r.Context(), time.Duration(profile.RequestTimeoutMS)*time.Millisecond)
			attemptRequest := r.Clone(ctx)
			if strings.EqualFold(ranked.Provider.Type, "anthropic") {
				g.forwardAnthropic(recorder, attemptRequest, ranked.Provider, attemptPayload, cr.Stream, &trace)
			} else {
				g.forwardCompatible(recorder, attemptRequest, ranked.Provider, upBody, cr.Stream, &trace)
			}
			cancel()
			elapsed := float64(time.Since(attemptStarted)) / float64(time.Millisecond)
			retryable := isRetryable(trace.StatusCode)
			attempts = append(attempts, routeAttempt{ranked.Target.ID, ranked.Provider.Slug + "/" + ranked.Target.Model, n + 1, trace.StatusCode, elapsed, retryable, trace.Error})
			if trace.Status != "success" {
				g.Store.RecordRoutingFailure(ranked.Provider.ID, ranked.Target.Model, elapsed)
			}
			final, finalTrace = recorder, trace
			if trace.Status == "success" {
				break
			}
			if !retryable {
				break
			}
			if n < profile.MaxRetries {
				select {
				case <-time.After(time.Duration(100*(1<<n)) * time.Millisecond):
				case <-r.Context().Done():
					break
				}
			}
		}
		if finalTrace.Status == "success" {
			break
		}
	}
	if final == nil {
		writeOpenAIError(w, 503, "No routing target was available.", "routing_error")
		return
	}
	for k, values := range final.Header() {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Nexa-Routing-Profile", profile.Slug)
	w.Header().Set("X-Nexa-Routing-Engine", profile.Engine)
	w.Header().Set("X-Nexa-Routed-Model", finalTrace.Model)
	w.WriteHeader(final.Code)
	_, _ = w.Write(final.Body.Bytes())
	totalMS := float64(time.Since(started)) / float64(time.Millisecond)
	finalTrace.LatencyMS = int64(totalMS)
	finalTrace.GatewayMS = totalMS - finalTrace.UpstreamMS
	if finalTrace.GatewayMS < 0 {
		finalTrace.GatewayMS = 0
	}
	finalTrace.CostUSD = estimateTargetCost(decision.Ranked, finalTrace.ProviderID, finalTrace.Model, finalTrace.InputTokens, finalTrace.OutputTokens)
	metadata, _ := json.Marshal(map[string]any{"smart_routing": true, "profile_id": profile.ID, "profile_slug": profile.Slug, "engine": profile.Engine, "objective": profile.Objective, "signals": decision.Signals, "confidence_threshold": profile.ConfidenceThreshold, "low_confidence": decision.LowConfidence, "engine_error": decision.EngineError, "attempts": attempts})
	finalTrace.Metadata = string(metadata)
	_ = g.Store.AddTrace(finalTrace)
}

func clonePayload(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func isRetryable(status int) bool {
	return status == 0 || status == 408 || status == 409 || status == 425 || status == 429 || status >= 500
}
func estimateTargetCost(ranked []routing.RankedTarget, providerID, model string, in, out int) float64 {
	for _, r := range ranked {
		if r.Target.ProviderID == providerID && r.Target.Model == model {
			return (float64(in)*r.Target.InputCostPerMillion + float64(out)*r.Target.OutputCostPerMillion) / 1_000_000
		}
	}
	return estimateCost(model, in, out)
}

// currentTraceInput keeps a trace focused on the user turn that caused the
// request. The complete message history is still forwarded to the provider,
// but earlier conversation turns are not repeated in every trace entry.
func currentTraceInput(messages json.RawMessage) string {
	var items []json.RawMessage
	if err := json.Unmarshal(messages, &items); err != nil || len(items) == 0 {
		return compactJSON(messages)
	}

	selected := items[len(items)-1]
	for i := len(items) - 1; i >= 0; i-- {
		var envelope struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(items[i], &envelope) == nil && strings.EqualFold(envelope.Role, "user") {
			selected = items[i]
			break
		}
	}

	isolated, err := json.Marshal([]json.RawMessage{selected})
	if err != nil {
		return compactJSON(selected)
	}
	return compactJSON(isolated)
}

func splitModel(model string) (string, string) {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1]
	}
	return "", model
}

func normalizeCompatiblePayload(providerType, model string, payload map[string]any) {
	providerType = strings.ToLower(providerType)
	model = strings.ToLower(model)
	modernReasoning := strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") || strings.Contains(model, "gpt-oss")
	if modernReasoning && (providerType == "openai" || providerType == "groq" || providerType == "custom" || providerType == "compatible") {
		if value, ok := payload["max_tokens"]; ok {
			if _, exists := payload["max_completion_tokens"]; !exists {
				payload["max_completion_tokens"] = value
			}
			delete(payload, "max_tokens")
		}
		if temperature, ok := payload["temperature"].(float64); ok && temperature != 1 {
			delete(payload, "temperature")
		}
	}
}

func (g *Gateway) forwardCompatible(w http.ResponseWriter, r *http.Request, p store.Provider, body []byte, stream bool, t *store.Trace) {
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		writeOpenAIError(w, 500, err.Error(), "gateway_error")
		t.Error = err.Error()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	copyRequestHeaders(req.Header, r.Header)
	upstreamStarted := time.Now()
	defer func() { t.UpstreamMS = float64(time.Since(upstreamStarted)) / float64(time.Millisecond) }()
	resp, err := g.Client.Do(req)
	if err != nil {
		writeOpenAIError(w, 502, "Upstream request failed: "+err.Error(), "upstream_error")
		t.Error = err.Error()
		t.StatusCode = 502
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Nexa-Provider", p.Slug)
	w.WriteHeader(resp.StatusCode)
	t.StatusCode = resp.StatusCode
	if stream {
		captured := &limitedBuffer{max: maxTraceBody}
		buf := make([]byte, 32*1024)
		for {
			n, e := resp.Body.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				_, _ = w.Write(chunk)
				_, _ = captured.Write(chunk)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if e != nil {
				if !errors.Is(e, io.EOF) {
					t.Error = e.Error()
				}
				break
			}
		}
		t.Response, t.InputTokens, t.OutputTokens, t.TotalTokens = parseSSE(captured.String())
	} else {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_, _ = w.Write(respBody)
		t.Response, t.InputTokens, t.OutputTokens, t.TotalTokens = parseOpenAI(respBody)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Status = "success"
	} else {
		t.Error = truncate(t.Response, 1000)
	}
}

func (g *Gateway) forwardAnthropic(w http.ResponseWriter, r *http.Request, p store.Provider, in map[string]any, stream bool, t *store.Trace) {
	body, err := toAnthropic(in)
	if err != nil {
		writeOpenAIError(w, 400, err.Error(), "invalid_request_error")
		t.Error = err.Error()
		t.StatusCode = 400
		return
	}
	b, _ := json.Marshal(body)
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/messages"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		writeOpenAIError(w, 500, err.Error(), "gateway_error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	upstreamStarted := time.Now()
	defer func() { t.UpstreamMS = float64(time.Since(upstreamStarted)) / float64(time.Millisecond) }()
	resp, err := g.Client.Do(req)
	if err != nil {
		writeOpenAIError(w, 502, "Upstream request failed: "+err.Error(), "upstream_error")
		t.Error = err.Error()
		t.StatusCode = 502
		return
	}
	defer resp.Body.Close()
	t.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		t.Response = string(raw)
		t.Error = string(raw)
		return
	}
	if stream {
		g.streamAnthropic(w, resp, t)
		t.Status = "success"
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	out, text, u, err := anthropicToOpenAI(raw)
	if err != nil {
		writeOpenAIError(w, 502, "Could not translate Anthropic response.", "upstream_error")
		t.Error = err.Error()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Nexa-Provider", p.Slug)
	w.WriteHeader(200)
	_, _ = w.Write(out)
	t.Response = text
	t.InputTokens = u.Prompt
	t.OutputTokens = u.Completion
	t.TotalTokens = u.Total
	t.StatusCode = 200
	t.Status = "success"
}

func toAnthropic(in map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range []string{"model", "temperature", "top_p", "stop_sequences", "stream", "metadata"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	} else if v, ok := in["max_completion_tokens"]; ok {
		out["max_tokens"] = v
	} else {
		out["max_tokens"] = 4096
	}
	msgs, ok := in["messages"].([]any)
	if !ok {
		return nil, errors.New("messages must be an array")
	}
	clean := []any{}
	systems := []string{}
	for _, item := range msgs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" {
			if s, ok := m["content"].(string); ok {
				systems = append(systems, s)
			}
			continue
		}
		if role == "assistant" || role == "user" {
			clean = append(clean, m)
		}
	}
	out["messages"] = clean
	if len(systems) > 0 {
		out["system"] = strings.Join(systems, "\n\n")
	}
	if tools, ok := in["tools"].([]any); ok {
		converted := []any{}
		for _, raw := range tools {
			tm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				converted = append(converted, map[string]any{"name": fn["name"], "description": fn["description"], "input_schema": fn["parameters"]})
			}
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	return out, nil
}

func anthropicToOpenAI(raw []byte) ([]byte, string, usage, error) {
	var a struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input any    `json:"input"`
		} `json:"content"`
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, "", usage{}, err
	}
	textParts := []string{}
	toolCalls := []any{}
	for i, c := range a.Content {
		if c.Type == "text" {
			textParts = append(textParts, c.Text)
		} else if c.Type == "tool_use" {
			args, _ := json.Marshal(c.Input)
			toolCalls = append(toolCalls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": string(args)}, "index": i})
		}
	}
	text := strings.Join(textParts, "")
	u := usage{Prompt: a.Usage.Input, Completion: a.Usage.Output, Total: a.Usage.Input + a.Usage.Output}
	finish := "stop"
	if a.StopReason == "max_tokens" {
		finish = "length"
	} else if a.StopReason == "tool_use" {
		finish = "tool_calls"
	}
	msg := map[string]any{"role": "assistant", "content": text}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	out := map[string]any{"id": a.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": a.Model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": u.Prompt, "completion_tokens": u.Completion, "total_tokens": u.Total}}
	b, err := json.Marshal(out)
	return b, text, u, err
}

func (g *Gateway) streamAnthropic(w http.ResponseWriter, resp *http.Response, t *store.Trace) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	id := "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	textOut := strings.Builder{}
	finish := "stop"
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}})
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) != nil {
			continue
		}
		typ, _ := e["type"].(string)
		switch typ {
		case "message_start":
			if m, ok := e["message"].(map[string]any); ok {
				if u, ok := m["usage"].(map[string]any); ok {
					t.InputTokens = intnum(u["input_tokens"])
				}
			}
		case "content_block_delta":
			if d, ok := e["delta"].(map[string]any); ok {
				if tx, ok := d["text"].(string); ok {
					textOut.WriteString(tx)
					send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": tx}, "finish_reason": nil}}})
				}
			}
		case "message_delta":
			if d, ok := e["delta"].(map[string]any); ok {
				if s, _ := d["stop_reason"].(string); s == "max_tokens" {
					finish = "length"
				}
			}
			if u, ok := e["usage"].(map[string]any); ok {
				t.OutputTokens = intnum(u["output_tokens"])
			}
		}
	}
	send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": t.InputTokens, "completion_tokens": t.OutputTokens, "total_tokens": t.InputTokens + t.OutputTokens}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	t.Response = textOut.String()
	t.TotalTokens = t.InputTokens + t.OutputTokens
}

func (g *Gateway) Models(ctx context.Context, p store.Provider) ([]map[string]any, error) {
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if strings.EqualFold(p.Type, "anthropic") {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("provider returned %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// ChatModels removes provider catalog entries that cannot service chat completions.
// Unknown models from compatible providers remain visible unless they match a known non-chat family.
func ChatModels(providerType string, models []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if id == "" {
			id, _ = model["name"].(string)
		}
		if isChatModel(providerType, id) {
			result = append(result, model)
		}
	}
	return result
}

func isChatModel(providerType, id string) bool {
	id = strings.ToLower(id)
	if id == "" {
		return false
	}
	for _, excluded := range []string{"embedding", "embed-", "babbage", "davinci", "whisper", "tts", "audio", "moderation", "dall-e", "image", "realtime", "transcri", "omni-moderation", "search-preview", "computer-use", "prompt-guard", "safeguard"} {
		if strings.Contains(id, excluded) {
			return false
		}
	}
	switch strings.ToLower(providerType) {
	case "openai":
		if datedModelSuffix.MatchString(id) {
			return false
		}
		for _, specialized := range []string{"codex", "search-api", "gpt-live", "instruct"} {
			if strings.Contains(id, specialized) {
				return false
			}
		}
		if strings.HasPrefix(id, "ft:") {
			return true
		}
		return strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "chatgpt-") || strings.HasPrefix(id, "o1") || strings.HasPrefix(id, "o3") || strings.HasPrefix(id, "o4")
	case "gemini":
		return strings.Contains(id, "gemini")
	case "groq":
		for _, family := range []string{"llama", "mixtral", "gemma", "qwen", "deepseek", "gpt-oss", "compound", "kimi"} {
			if strings.Contains(id, family) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func parseOpenAI(b []byte) (string, int, int, int) {
	var x struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage usage `json:"usage"`
		Error any   `json:"error"`
	}
	if json.Unmarshal(b, &x) != nil {
		return string(b), 0, 0, 0
	}
	content := ""
	if len(x.Choices) > 0 {
		switch v := x.Choices[0].Message.Content.(type) {
		case string:
			content = v
		default:
			c, _ := json.Marshal(v)
			content = string(c)
		}
	}
	if content == "" {
		content = string(b)
	}
	return content, x.Usage.Prompt, x.Usage.Completion, x.Usage.Total
}
func parseSSE(v string) (string, int, int, int) {
	var out strings.Builder
	u := usage{}
	for _, line := range strings.Split(v, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.HasSuffix(line, "[DONE]") {
			continue
		}
		var x struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage usage `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &x) == nil {
			if len(x.Choices) > 0 {
				out.WriteString(x.Choices[0].Delta.Content)
			}
			if x.Usage.Total > 0 {
				u = x.Usage
			}
		}
	}
	return out.String(), u.Prompt, u.Completion, u.Total
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remain := b.max - b.Len()
	if remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
func copyRequestHeaders(dst, src http.Header) {
	for _, h := range []string{"Accept", "OpenAI-Organization", "OpenAI-Project"} {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
}
func copyResponseHeaders(dst, src http.Header) {
	for _, h := range []string{"Content-Type", "Cache-Control", "X-Request-Id", "OpenAI-Processing-Ms"} {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
}
func writeOpenAIError(w http.ResponseWriter, status int, message, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": typ, "code": nil}})
}
func compactJSON(b []byte) string {
	var out bytes.Buffer
	if json.Compact(&out, b) == nil {
		return truncate(out.String(), maxTraceBody)
	}
	return truncate(string(b), maxTraceBody)
}
func truncate(v string, n int) string {
	if len(v) > n {
		return v[:n] + "…"
	}
	return v
}
func intnum(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func estimateCost(model string, in, out int) float64 {
	// USD per million tokens. Unknown models intentionally return zero rather than inventing a price.
	type price struct{ i, o float64 }
	prices := map[string]price{
		"gpt-4o-mini": {0.15, 0.60}, "gpt-4o": {2.50, 10}, "gpt-4.1": {2, 8}, "gpt-4.1-mini": {0.40, 1.60},
		"claude-3-5-haiku-latest": {0.80, 4}, "claude-3-5-sonnet-latest": {3, 15}, "claude-3-7-sonnet-latest": {3, 15},
		"gemini-2.0-flash": {0.10, 0.40}, "gemini-2.5-flash": {0.30, 2.50}, "gemini-2.5-pro": {1.25, 10},
		"llama-3.3-70b-versatile": {0.59, 0.79}, "openai/gpt-oss-120b": {0.15, 0.75}, "openai/gpt-oss-20b": {0.075, 0.30},
	}
	p, ok := prices[strings.ToLower(model)]
	if !ok {
		return 0
	}
	return (float64(in)*p.i + float64(out)*p.o) / 1_000_000
}
