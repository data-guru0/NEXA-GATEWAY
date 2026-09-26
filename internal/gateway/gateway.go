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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	routing "github.com/evolvue/nexa-gateway/internal/router"
	"github.com/evolvue/nexa-gateway/internal/store"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

const maxTraceBody = 1 << 20

var datedModelSuffix = regexp.MustCompile(`-(?:\d{4}|\d{4}-\d{2}-\d{2})$`)

type Gateway struct {
	Store  *store.Store
	Client *http.Client
	Router *routing.Engine

	modelsMu sync.Mutex
	models   map[string]modelCache

	cache cacheState
}

type modelCache struct {
	data    []map[string]any
	err     error
	expires time.Time
}

func New(s *store.Store) *Gateway {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 256
	t.MaxIdleConnsPerHost = 64
	// Non-streaming completions only send headers once generation finishes, so the
	// header timeout is generous; streams are then unbounded (no Client.Timeout).
	t.ResponseHeaderTimeout = 10 * time.Minute
	g := &Gateway{Store: s, Client: &http.Client{Transport: t}, Router: routing.New(s), models: map[string]modelCache{}}
	s.OnEvent("cache-settings", func() { g.cache.mu.Lock(); g.cache.settings = nil; g.cache.mu.Unlock() })
	return g
}

// CallInfo travels in the request context from the server middleware.
type CallInfo struct {
	Start   time.Time
	Source  string
	KeyName string
	KeyID   string          // set for application API keys, which carry limits
	Limits  store.KeyLimits // limits of that key
}

type ctxKey struct{}

// StartClock stamps the request start before authentication so reported
// gateway overhead includes every step Nexa adds.
func StartClock(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, CallInfo{Start: time.Now()})))
	})
}

// Annotate records who is calling (source is "api" or "playground").
func Annotate(r *http.Request, source, keyName string) *http.Request {
	info := callInfo(r)
	info.Source, info.KeyName = source, keyName
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, info))
}

// AnnotateKey records an application API key and its limits.
func AnnotateKey(r *http.Request, k store.APIKey) *http.Request {
	info := callInfo(r)
	info.Source, info.KeyName, info.KeyID, info.Limits = "api", k.Name, k.ID, k.Limits
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, info))
}

func callInfo(r *http.Request) CallInfo {
	info, _ := r.Context().Value(ctxKey{}).(CallInfo)
	if info.Start.IsZero() {
		info.Start = time.Now()
	}
	return info
}

func msSince(t time.Time) float64 { return float64(time.Since(t)) / float64(time.Millisecond) }

func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	info := callInfo(r)
	t := &store.Trace{ID: uuid.NewString(), RequestID: middleware.GetReqID(r.Context()), Source: info.Source, APIKeyName: info.KeyName, APIKeyID: info.KeyID, ProviderName: "—", Status: "error"}
	w.Header().Set("X-Nexa-Trace-Id", t.ID)
	defer g.finish(t, info.Start)
	fail := func(status int, msg, typ string) {
		t.StatusCode, t.Error = status, msg
		writeOpenAIError(w, status, msg, typ)
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		fail(400, "Request body is too large or invalid.", "invalid_request_error")
		return
	}
	t.Request = compactJSON(body)
	payload, err := decodePayload(body)
	model, _ := payload["model"].(string)
	messages, _ := payload["messages"].([]any)
	if err != nil || model == "" || len(messages) == 0 {
		fail(400, "model and messages are required.", "invalid_request_error")
		return
	}
	t.Model = model
	t.Stream, _ = payload["stream"].(bool)
	rawMessages, _ := json.Marshal(messages)
	t.Prompt = currentTraceInput(rawMessages)
	t.Params = traceParams(payload)
	if !g.admit(w, r, info, t, model) {
		return
	}
	served, pending := g.cacheLookup(w, r, payload, messages, model, t, info.Start)
	if served {
		return
	}
	switch {
	case model == "smart" || strings.HasPrefix(model, "smart/"):
		g.smart(w, r, payload, rawMessages, t, info.Start)
	case strings.HasPrefix(model, "feedback/"):
		g.feedbackLoop(w, r, payload, rawMessages, t, info.Start)
	default:
		p, upstreamModel, err := g.resolve(model, strings.TrimSpace(r.Header.Get("X-Nexa-Provider")))
		if err != nil {
			fail(404, err.Error(), "invalid_request_error")
			return
		}
		t.ProviderID, t.ProviderName, t.Model = p.ID, p.Name, upstreamModel
		g.forward(w, r.Context(), r.Header, attempt{provider: p, model: upstreamModel, payload: payload, stream: t.Stream, retries: 1, start: info.Start}, t)
	}
	g.cacheSave(pending, t)
}

func (g *Gateway) finish(t *store.Trace, start time.Time) {
	totalMS := msSince(start)
	t.LatencyMS = int64(totalMS)
	t.GatewayMS = max(totalMS-t.UpstreamMS, 0)
	if t.CostUSD == 0 {
		t.CostUSD = g.estimateCost(t.Model, t.InputTokens, t.OutputTokens)
	}
	t.CostUSD += t.ExtraCostUSD
	_ = g.Store.AddTrace(*t)
	g.recordUsage(t)
}

// resolve maps a model string to a provider. The leading segment is a provider
// only when it names one, so bare model ids such as "openai/gpt-oss-20b" still
// work when a single provider is configured.
func (g *Gateway) resolve(model, headerProvider string) (store.Provider, string, error) {
	check := func(p store.Provider, m string) (store.Provider, string, error) {
		if !p.Enabled {
			return p, m, fmt.Errorf("Provider %q is disabled.", p.Slug)
		}
		return p, m, nil
	}
	if headerProvider != "" {
		p, err := g.Store.Provider(headerProvider)
		if err != nil {
			return p, model, fmt.Errorf("Provider %q not found.", headerProvider)
		}
		return check(p, model)
	}
	if ref, rest := splitModel(model); ref != "" {
		if p, err := g.Store.Provider(ref); err == nil {
			return check(p, rest)
		}
	}
	all, _ := g.Store.Providers()
	enabled := []store.Provider{}
	for _, p := range all {
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}
	if len(enabled) == 1 {
		return enabled[0], model, nil
	}
	return store.Provider{}, model, errors.New("Unknown provider. Use provider-slug/model (for example openai/gpt-4o-mini) or send X-Nexa-Provider.")
}

func decodePayload(body []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber() // keeps large integers such as seed exact
	var payload map[string]any
	err := d.Decode(&payload)
	return payload, err
}

func traceParams(payload map[string]any) string {
	params := map[string]any{}
	for k, v := range payload {
		if k != "messages" && k != "model" {
			params[k] = v
		}
	}
	b, _ := json.Marshal(params)
	return truncate(string(b), 64<<10)
}

type attempt struct {
	provider  store.Provider
	model     string
	payload   map[string]any
	stream    bool
	retries   int
	start     time.Time
	onHeaders func()
}

func (g *Gateway) forward(w http.ResponseWriter, ctx context.Context, clientHeader http.Header, a attempt, t *store.Trace) {
	a.payload["model"] = a.model
	upstreamStarted := time.Now()
	defer func() { t.UpstreamMS = msSince(upstreamStarted) }()
	var res parsed
	var err error
	if strings.EqualFold(a.provider.Type, "anthropic") {
		res, err = g.forwardAnthropic(w, ctx, a, t)
	} else {
		res, err = g.forwardCompatible(w, ctx, clientHeader, a, t)
	}
	res.apply(t)
	switch {
	case err != nil:
		t.Error = truncate(err.Error(), 1000)
	case res.errMsg != "":
		t.Error = truncate(res.errMsg, 1000)
	case t.StatusCode >= 200 && t.StatusCode < 300:
		t.Status = "success"
	default:
		t.Error = truncate(t.Response, 1000)
	}
}

// send performs a request, retrying network errors and retryable statuses up to
// retries times while honouring Retry-After (at most 10s).
func (g *Gateway) send(ctx context.Context, build func() (*http.Request, error), retries int) (*http.Response, error) {
	for n := 0; ; n++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := g.Client.Do(req)
		if n >= retries || ctx.Err() != nil || (err == nil && !isRetryable(resp.StatusCode)) {
			return resp, err
		}
		wait := 500 * time.Millisecond
		if resp != nil {
			if d, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
				if d > 10*time.Second {
					return resp, nil
				}
				wait = d
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func retryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(s * float64(time.Second)), true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}

func isRetryable(status int) bool {
	return status == 0 || status == 408 || status == 409 || status == 425 || status == 429 || status >= 500
}

func upstreamFailure(w http.ResponseWriter, t *store.Trace, err error) error {
	t.StatusCode = 502
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.StatusCode = 504
	}
	writeOpenAIError(w, t.StatusCode, "Upstream request failed: "+err.Error(), "upstream_error")
	return err
}

func (g *Gateway) forwardCompatible(w http.ResponseWriter, ctx context.Context, clientHeader http.Header, a attempt, t *store.Trace) (parsed, error) {
	p, payload := a.provider, a.payload
	if dropped := normalizeCompatiblePayload(p.Type, a.model, payload); len(dropped) > 0 {
		w.Header().Set("X-Nexa-Dropped-Params", strings.Join(dropped, ","))
	}
	wantsUsage := clientWantsUsage(payload)
	injected := false
	if a.stream && !wantsUsage && injectsStreamUsage(p.Type) {
		opts := map[string]any{}
		if existing, ok := payload["stream_options"].(map[string]any); ok {
			for k, v := range existing {
				opts[k] = v
			}
		}
		opts["include_usage"] = true
		payload["stream_options"] = opts
		injected = true
	}
	body, _ := json.Marshal(payload)
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	resp, err := g.send(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		copyRequestHeaders(req.Header, clientHeader)
		applyHeaders(req.Header, p.Headers)
		return req, nil
	}, a.retries)
	if err != nil {
		return parsed{}, upstreamFailure(w, t, err)
	}
	defer resp.Body.Close()
	if a.onHeaders != nil {
		a.onHeaders()
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Nexa-Provider", p.Slug)
	w.WriteHeader(resp.StatusCode)
	t.StatusCode = resp.StatusCode
	if a.stream && strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		return pipeSSE(w, resp.Body, injected, a.start, t)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	_, _ = w.Write(raw)
	return parseOpenAI(raw), nil
}

func clientWantsUsage(payload map[string]any) bool {
	opts, _ := payload["stream_options"].(map[string]any)
	return opts != nil && opts["include_usage"] == true
}

// OpenAI and Groq only report streamed token usage when asked to.
func injectsStreamUsage(providerType string) bool {
	switch strings.ToLower(providerType) {
	case "openai", "groq":
		return true
	}
	return false
}

func applyHeaders(dst http.Header, extra map[string]string) {
	for k, v := range extra {
		dst.Set(k, v)
	}
}

// pipeSSE streams upstream events to the client as they arrive. When Nexa added
// include_usage itself, the extra usage-only chunk is kept for the trace but not
// forwarded, so clients see exactly the stream they asked for.
func pipeSSE(w http.ResponseWriter, body io.Reader, hideUsageChunk bool, start time.Time, t *store.Trace) (parsed, error) {
	reader := bufio.NewReaderSize(body, 32<<10)
	captured := &limitedBuffer{max: maxTraceBody}
	flusher, _ := w.(http.Flusher)
	skipBlank, first := false, true
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			_, _ = captured.Write(line)
			switch {
			case hideUsageChunk && isUsageOnlyChunk(trimmed):
				skipBlank = true
			case skipBlank && len(trimmed) == 0:
				skipBlank = false
			default:
				if first && bytes.HasPrefix(trimmed, []byte("data:")) {
					t.TTFTMS, first = msSince(start), false
				}
				_, _ = w.Write(line)
				if len(trimmed) == 0 && flusher != nil {
					flusher.Flush()
				}
			}
		}
		if err != nil {
			if flusher != nil {
				flusher.Flush()
			}
			res := parseSSE(captured.String())
			if errors.Is(err, io.EOF) {
				return res, nil
			}
			return res, fmt.Errorf("stream interrupted: %w", err)
		}
	}
}

func isUsageOnlyChunk(line []byte) bool {
	if !bytes.HasPrefix(line, []byte("data:")) || !bytes.Contains(line, []byte(`"usage"`)) {
		return false
	}
	var chunk struct {
		Choices []any `json:"choices"`
		Usage   any   `json:"usage"`
	}
	return json.Unmarshal(bytes.TrimSpace(line[5:]), &chunk) == nil && len(chunk.Choices) == 0 && chunk.Usage != nil
}

func normalizeCompatiblePayload(providerType, model string, payload map[string]any) []string {
	if !strings.EqualFold(providerType, "openai") {
		return nil
	}
	model = strings.ToLower(model)
	if !(strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")) {
		return nil
	}
	if value, ok := payload["max_tokens"]; ok {
		if _, exists := payload["max_completion_tokens"]; !exists {
			payload["max_completion_tokens"] = value
		}
		delete(payload, "max_tokens")
	}
	if temperature, ok := toFloat(payload["temperature"]); ok && temperature != 1 {
		delete(payload, "temperature")
		return []string{"temperature"}
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// ---------------------------------------------------------------- Anthropic

func (g *Gateway) forwardAnthropic(w http.ResponseWriter, ctx context.Context, a attempt, t *store.Trace) (parsed, error) {
	p := a.provider
	body, err := toAnthropic(a.payload, a.model)
	if err != nil {
		t.StatusCode = 400
		writeOpenAIError(w, 400, err.Error(), "invalid_request_error")
		return parsed{}, err
	}
	b, _ := json.Marshal(body)
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/messages"
	resp, err := g.send(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		applyHeaders(req.Header, p.Headers)
		return req, nil
	}, a.retries)
	if err != nil {
		return parsed{}, upstreamFailure(w, t, err)
	}
	defer resp.Body.Close()
	if a.onHeaders != nil {
		a.onHeaders()
	}
	t.StatusCode = resp.StatusCode
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Nexa-Provider", p.Slug)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		msg, typ := anthropicError(raw)
		writeOpenAIError(w, resp.StatusCode, msg, typ)
		return parsed{text: string(raw), errMsg: msg}, nil
	}
	if a.stream {
		return streamAnthropic(w, resp.Body, clientWantsUsage(a.payload), a.start, t)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	out, res, err := anthropicToOpenAI(raw)
	if err != nil {
		t.StatusCode = 502
		writeOpenAIError(w, 502, "Could not translate Anthropic response.", "upstream_error")
		return parsed{text: string(raw)}, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(out)
	return res, nil
}

func anthropicError(raw []byte) (string, string) {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message, e.Error.Type
	}
	return truncate(string(raw), 1000), "upstream_error"
}

func toAnthropic(in map[string]any, model string) (map[string]any, error) {
	out := map[string]any{"model": model}
	for _, k := range []string{"temperature", "top_p", "stream"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	// ponytail: 4096 default is the output cap every Claude model accepts; send max_tokens for more.
	out["max_tokens"] = 4096
	if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	} else if v, ok := in["max_completion_tokens"]; ok {
		out["max_tokens"] = v
	}
	switch stop := in["stop"].(type) {
	case string:
		out["stop_sequences"] = []any{stop}
	case []any:
		out["stop_sequences"] = stop
	}
	if user, ok := in["user"].(string); ok && user != "" {
		out["metadata"] = map[string]any{"user_id": user}
	}
	if tools, ok := in["tools"].([]any); ok {
		converted := []any{}
		for _, raw := range tools {
			tm, _ := raw.(map[string]any)
			if fn, ok := tm["function"].(map[string]any); ok {
				schema := fn["parameters"]
				if schema == nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				tool := map[string]any{"name": fn["name"], "input_schema": schema}
				if d, ok := fn["description"]; ok {
					tool["description"] = d
				}
				converted = append(converted, tool)
			}
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	switch choice := in["tool_choice"].(type) {
	case string:
		out["tool_choice"] = map[string]any{"type": map[string]string{"auto": "auto", "required": "any", "none": "none"}[choice]}
		if choice != "auto" && choice != "required" && choice != "none" {
			delete(out, "tool_choice")
		}
	case map[string]any:
		if fn, ok := choice["function"].(map[string]any); ok {
			out["tool_choice"] = map[string]any{"type": "tool", "name": fn["name"]}
		}
	}
	msgs, ok := in["messages"].([]any)
	if !ok {
		return nil, errors.New("messages must be an array")
	}
	type turn struct {
		role   string
		blocks []any
	}
	turns := []turn{}
	add := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		// Anthropic requires alternating roles, so consecutive same-role turns
		// (for example several tool results) merge into one message.
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			return
		}
		turns = append(turns, turn{role, blocks})
	}
	systems := []string{}
	for _, item := range msgs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if text := partsText(m["content"]); text != "" {
				systems = append(systems, text)
			}
		case "user":
			blocks, err := contentBlocks(m["content"])
			if err != nil {
				return nil, err
			}
			add("user", blocks)
		case "assistant":
			blocks, err := contentBlocks(m["content"])
			if err != nil {
				return nil, err
			}
			calls, _ := m["tool_calls"].([]any)
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				var input any = map[string]any{}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					var parsedArgs any
					if json.Unmarshal([]byte(args), &parsedArgs) == nil {
						input = parsedArgs
					}
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": input})
			}
			add("assistant", blocks)
		case "tool":
			add("user", []any{map[string]any{"type": "tool_result", "tool_use_id": m["tool_call_id"], "content": partsText(m["content"])}})
		}
	}
	converted := make([]any, len(turns))
	for i, t := range turns {
		converted[i] = map[string]any{"role": t.role, "content": t.blocks}
	}
	out["messages"] = converted
	if len(systems) > 0 {
		out["system"] = strings.Join(systems, "\n\n")
	}
	return out, nil
}

func partsText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		texts := []string{}
		for _, raw := range c {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func contentBlocks(content any) ([]any, error) {
	switch c := content.(type) {
	case nil:
		return nil, nil
	case string:
		if c == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "text", "text": c}}, nil
	case []any:
		blocks := []any{}
		for _, raw := range c {
			part, _ := raw.(map[string]any)
			switch part["type"] {
			case "text":
				if text, _ := part["text"].(string); text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				url := ""
				switch v := part["image_url"].(type) {
				case string:
					url = v
				case map[string]any:
					url, _ = v["url"].(string)
				}
				blocks = append(blocks, anthropicImage(url))
			default:
				return nil, fmt.Errorf("content part type %v is not supported for Anthropic models", part["type"])
			}
		}
		return blocks, nil
	}
	return nil, errors.New("unsupported message content")
}

func anthropicImage(url string) map[string]any {
	if strings.HasPrefix(url, "data:") {
		if meta, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ","); ok {
			return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": strings.TrimSuffix(meta, ";base64"), "data": data}}
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

type anthropicUsage struct {
	Input         int `json:"input_tokens"`
	Output        int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) openAI() usage {
	prompt := u.Input + u.CacheRead + u.CacheCreation
	x := usage{Prompt: prompt, Completion: u.Output, Total: prompt + u.Output}
	x.PromptDetails.Cached = u.CacheRead
	return x
}

func anthropicFinish(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

func anthropicToOpenAI(raw []byte) ([]byte, parsed, error) {
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
		Usage anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, parsed{}, err
	}
	res := parsed{finish: anthropicFinish(a.StopReason), u: a.Usage.openAI()}
	textParts := []string{}
	toolCalls := []any{}
	for _, c := range a.Content {
		switch c.Type {
		case "text":
			textParts = append(textParts, c.Text)
		case "tool_use":
			args, _ := json.Marshal(c.Input)
			toolCalls = append(toolCalls, map[string]any{"id": c.ID, "type": "function", "index": len(toolCalls), "function": map[string]any{"name": c.Name, "arguments": string(args)}})
			res.toolCalls = append(res.toolCalls, toolCall{name: c.Name, args: string(args)})
		}
	}
	res.text = strings.Join(textParts, "")
	msg := map[string]any{"role": "assistant", "content": res.text}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if res.text == "" {
			msg["content"] = nil
		}
	}
	out := map[string]any{"id": a.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": a.Model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": res.finish}}, "usage": res.u}
	b, err := json.Marshal(out)
	return b, res, err
}

// streamAnthropic translates Anthropic server-sent events into OpenAI chunks,
// including streamed tool calls.
func streamAnthropic(w http.ResponseWriter, body io.Reader, wantsUsage bool, start time.Time, t *store.Trace) (parsed, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	created := time.Now().Unix()
	send := func(choices []any, extra map[string]any) {
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "choices": choices}
		for k, v := range extra {
			chunk[k] = v
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	delta := func(d map[string]any) { send([]any{map[string]any{"index": 0, "delta": d, "finish_reason": nil}}, nil) }
	markFirst := func() {
		if t.TTFTMS == 0 {
			t.TTFTMS = msSince(start)
		}
	}
	delta(map[string]any{"role": "assistant", "content": ""})
	res := parsed{finish: "stop"}
	var text strings.Builder
	var u anthropicUsage
	toolIndex := map[int]int{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var e struct {
			Type         string         `json:"type"`
			Index        int            `json:"index"`
			Message      map[string]any `json:"message"`
			ContentBlock map[string]any `json:"content_block"`
			Delta        map[string]any `json:"delta"`
			Usage        map[string]any `json:"usage"`
			Error        struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &e) != nil {
			continue
		}
		switch e.Type {
		case "message_start":
			if m, ok := e.Message["usage"].(map[string]any); ok {
				u.Input, u.CacheRead, u.CacheCreation = intnum(m["input_tokens"]), intnum(m["cache_read_input_tokens"]), intnum(m["cache_creation_input_tokens"])
			}
		case "content_block_start":
			if e.ContentBlock["type"] == "tool_use" {
				markFirst()
				idx := len(res.toolCalls)
				toolIndex[e.Index] = idx
				name, _ := e.ContentBlock["name"].(string)
				res.toolCalls = append(res.toolCalls, toolCall{name: name})
				delta(map[string]any{"tool_calls": []any{map[string]any{"index": idx, "id": e.ContentBlock["id"], "type": "function", "function": map[string]any{"name": name, "arguments": ""}}}})
			}
		case "content_block_delta":
			switch e.Delta["type"] {
			case "text_delta":
				tx, _ := e.Delta["text"].(string)
				markFirst()
				text.WriteString(tx)
				delta(map[string]any{"content": tx})
			case "input_json_delta":
				part, _ := e.Delta["partial_json"].(string)
				idx := toolIndex[e.Index]
				if idx < len(res.toolCalls) {
					res.toolCalls[idx].args += part
				}
				delta(map[string]any{"tool_calls": []any{map[string]any{"index": idx, "function": map[string]any{"arguments": part}}}})
			}
		case "message_delta":
			if reason, ok := e.Delta["stop_reason"].(string); ok {
				res.finish = anthropicFinish(reason)
			}
			if e.Usage != nil {
				u.Output = intnum(e.Usage["output_tokens"])
			}
		case "error":
			res.errMsg = "Anthropic stream error: " + e.Error.Message
		}
	}
	if err := scanner.Err(); err != nil && res.errMsg == "" {
		res.errMsg = "stream interrupted: " + err.Error()
	}
	res.text = text.String()
	res.u = u.openAI()
	send([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": res.finish}}, nil)
	if wantsUsage {
		send([]any{}, map[string]any{"usage": res.u})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return res, nil
}

// ---------------------------------------------------------------- smart routing

type routeAttempt struct {
	TargetID   string  `json:"target_id"`
	Route      string  `json:"route"`
	Attempt    int     `json:"attempt"`
	StatusCode int     `json:"status_code"`
	LatencyMS  float64 `json:"latency_ms"`
	Retryable  bool    `json:"retryable"`
	Error      string  `json:"error,omitempty"`
}

// gateWriter holds an attempt's response until the provider answers 2xx. A
// failed attempt stays private so the next route can be tried; a successful
// one is committed and then streams straight to the client.
type gateWriter struct {
	w            http.ResponseWriter
	header       http.Header
	status       int
	buf          bytes.Buffer
	committed    bool
	beforeCommit func(http.Header)
}

func (g *gateWriter) Header() http.Header { return g.header }
func (g *gateWriter) WriteHeader(code int) {
	if g.status != 0 {
		return
	}
	g.status = code
	if code >= 200 && code < 300 {
		g.commit()
	}
}
func (g *gateWriter) commit() {
	dst := g.w.Header()
	for k, v := range g.header {
		dst[k] = v
	}
	if g.beforeCommit != nil {
		g.beforeCommit(dst)
	}
	g.w.WriteHeader(g.status)
	g.committed = true
}
func (g *gateWriter) Write(b []byte) (int, error) {
	if g.status == 0 {
		g.WriteHeader(200)
	}
	if g.committed {
		return g.w.Write(b)
	}
	return g.buf.Write(b)
}
func (g *gateWriter) Flush() {
	if f, ok := g.w.(http.Flusher); ok && g.committed {
		f.Flush()
	}
}
func (g *gateWriter) replay() {
	if g.status == 0 {
		g.status = 502
	}
	g.commit()
	_, _ = g.w.Write(g.buf.Bytes())
}

func (g *Gateway) smart(w http.ResponseWriter, r *http.Request, payload map[string]any, rawMessages json.RawMessage, t *store.Trace, start time.Time) {
	model := payload["model"].(string)
	fail := func(status int, msg string) {
		t.StatusCode, t.Error = status, msg
		writeOpenAIError(w, status, msg, "routing_error")
	}
	var profile store.RoutingProfile
	var err error
	if model == "smart" {
		profile, err = g.Store.ActiveRoutingProfile()
		if err != nil {
			fail(404, "No active smart routing profile is configured.")
			return
		}
	} else if profile, err = g.Store.RoutingProfile(strings.TrimPrefix(model, "smart/")); err != nil {
		fail(404, fmt.Sprintf("Smart routing profile %q not found.", strings.TrimPrefix(model, "smart/")))
		return
	}
	t.ProviderName = "smart/" + profile.Slug
	decision, err := g.Router.Decide(r.Context(), profile, rawMessages, payload)
	if err != nil {
		fail(503, err.Error())
		t.Metadata = smartMetadata(profile, decision, nil)
		return
	}
	candidates := make([]routeCandidate, 0, len(decision.Ranked))
	for _, ranked := range decision.Ranked {
		candidates = append(candidates, routeCandidate{ID: ranked.Target.ID, Label: ranked.ProviderSlug + "/" + ranked.Target.Model, Provider: ranked.Provider, Model: ranked.Target.Model, InputCost: ranked.Target.InputCostPerMillion, OutputCost: ranked.Target.OutputCostPerMillion})
	}
	attempts, chosen := g.runRoutes(w, r, payload, t, start, candidates, profile.MaxRetries, time.Duration(profile.RequestTimeoutMS)*time.Millisecond, func(h http.Header, c routeCandidate) {
		h.Set("X-Nexa-Routing-Profile", profile.Slug)
		h.Set("X-Nexa-Routed-Model", c.Label)
		h.Set("X-Nexa-Routing-Lane", decision.Signals.Lane)
		h.Set("X-Nexa-Routing-Confidence", strconv.FormatFloat(decision.Signals.Confidence, 'f', 2, 64))
	})
	t.Metadata = smartMetadata(profile, decision, attempts)
	if chosen != nil && (chosen.InputCost > 0 || chosen.OutputCost > 0) {
		t.CostUSD = (float64(t.InputTokens)*chosen.InputCost + float64(t.OutputTokens)*chosen.OutputCost) / 1_000_000
	}
}

// routeCandidate is one provider/model that a virtual model (smart/… or feedback/…) may use.
type routeCandidate struct {
	ID, Label             string
	Provider              store.Provider
	Model                 string
	InputCost, OutputCost float64
}

// runRoutes tries candidates in order until one succeeds. An attempt stays private
// until its provider answers 2xx, then streams straight to the client; retryable
// failures are retried, 401/403/404 move to the next candidate, other 4xx stop.
// When nothing succeeds, the last failure (or a 503) is written to the client.
func (g *Gateway) runRoutes(w http.ResponseWriter, r *http.Request, payload map[string]any, t *store.Trace, start time.Time, candidates []routeCandidate, maxRetries int, timeout time.Duration, setHeaders func(http.Header, routeCandidate)) ([]routeAttempt, *routeCandidate) {
	base := *t
	attempts := []routeAttempt{}
	var last *gateWriter
	var chosen *routeCandidate
routes:
	for i := range candidates {
		c := &candidates[i]
		for n := 0; n <= maxRetries; n++ {
			if r.Context().Err() != nil {
				t.StatusCode, t.Error = 499, "client disconnected"
				return attempts, nil
			}
			at := base
			at.ProviderID, at.ProviderName, at.Model = c.Provider.ID, c.Provider.Name, c.Model
			gw := &gateWriter{w: w, header: http.Header{}, beforeCommit: func(h http.Header) { setHeaders(h, *c) }}
			// The timeout covers only the wait for response headers; once the
			// provider starts answering, a long generation is never cut off.
			ctx, cancel := context.WithCancel(r.Context())
			var timedOut atomic.Bool
			timer := time.AfterFunc(timeout, func() { timedOut.Store(true); cancel() })
			attemptStarted := time.Now()
			g.forward(gw, ctx, r.Header, attempt{provider: c.Provider, model: c.Model, payload: clonePayload(payload), stream: t.Stream, start: start, onHeaders: func() { timer.Stop() }}, &at)
			timer.Stop()
			cancel()
			if timedOut.Load() && r.Context().Err() == nil {
				at.StatusCode, at.Error = 504, fmt.Sprintf("no response within %s", timeout)
			}
			retryable := isRetryable(at.StatusCode)
			attempts = append(attempts, routeAttempt{c.ID, c.Label, n + 1, at.StatusCode, msSince(attemptStarted), retryable, at.Error})
			*t = at
			if at.Status == "success" {
				chosen = c
				break routes
			}
			if r.Context().Err() != nil {
				t.StatusCode, t.Error = 499, "client disconnected"
				return attempts, nil
			}
			g.Store.RecordRoutingFailure(c.Provider.ID, c.Model, msSince(attemptStarted))
			if gw.committed {
				break routes // the stream already started; failing over would duplicate output
			}
			last = gw
			if !retryable {
				if at.StatusCode == 401 || at.StatusCode == 403 || at.StatusCode == 404 {
					continue routes // this provider's key or model is broken; try the next lane
				}
				break routes // the request itself is invalid; every lane would reject it
			}
			if n < maxRetries {
				select {
				case <-time.After(time.Duration(100*(1<<n)) * time.Millisecond):
				case <-r.Context().Done():
					t.StatusCode, t.Error = 499, "client disconnected"
					return attempts, nil
				}
			}
		}
	}
	if chosen == nil {
		if last == nil {
			if len(attempts) == 0 {
				t.StatusCode, t.Error = 503, "No routing target was available."
				writeOpenAIError(w, 503, t.Error, "routing_error")
			}
			return attempts, nil
		}
		last.replay()
		return attempts, nil
	}
	return attempts, chosen
}

func smartMetadata(p store.RoutingProfile, d routing.Decision, attempts []routeAttempt) string {
	if attempts == nil {
		attempts = []routeAttempt{}
	}
	b, _ := json.Marshal(map[string]any{"smart_routing": true, "engine": "jev", "profile_id": p.ID, "profile_slug": p.Slug, "signals": d.Signals, "lane": d.Signals.Lane, "confidence_threshold": p.ConfidenceThreshold, "low_confidence": d.LowConfidence, "escalated": d.Signals.Escalated, "engine_error": d.EngineError, "attempts": attempts})
	return string(b)
}

func clonePayload(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// currentTraceInput keeps a trace focused on the user turn that caused the
// request. The complete request is stored separately in Trace.Request.
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

// ---------------------------------------------------------------- model catalog

// Models returns a provider's catalog, cached for five minutes (failures for 30
// seconds, so one dead provider cannot slow every catalog call) unless refresh is set.
func (g *Gateway) Models(ctx context.Context, p store.Provider, refresh bool) ([]map[string]any, error) {
	if !refresh {
		g.modelsMu.Lock()
		c, ok := g.models[p.ID]
		g.modelsMu.Unlock()
		if ok && time.Now().Before(c.expires) {
			return c.data, c.err
		}
	}
	data, err := g.fetchModels(ctx, p)
	if err != nil && ctx.Err() != nil {
		return nil, err // the caller went away; that says nothing about the provider
	}
	ttl := 5 * time.Minute
	if err != nil {
		ttl = 30 * time.Second
	}
	g.modelsMu.Lock()
	g.models[p.ID] = modelCache{data, err, time.Now().Add(ttl)}
	g.modelsMu.Unlock()
	return data, err
}

func (g *Gateway) fetchModels(ctx context.Context, p store.Provider) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if strings.EqualFold(p.Type, "anthropic") {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	applyHeaders(req.Header, p.Headers)
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

func (g *Gateway) ForgetModels(providerID string) {
	g.modelsMu.Lock()
	delete(g.models, providerID)
	g.modelsMu.Unlock()
}

// AllModels fetches every enabled provider's catalog in parallel and prefixes ids with the provider slug.
func (g *Gateway) AllModels(ctx context.Context, providers []store.Provider) ([]map[string]any, int) {
	results := make([][]map[string]any, len(providers))
	var failed atomic.Int32
	var wg sync.WaitGroup
	for i, p := range providers {
		if !p.Enabled {
			continue
		}
		wg.Add(1)
		go func(i int, p store.Provider) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			models, err := g.Models(ctx, p, false)
			if err != nil {
				failed.Add(1)
				return
			}
			for _, model := range models {
				id, _ := model["id"].(string)
				if id == "" {
					id, _ = model["name"].(string)
				}
				if id == "" {
					continue
				}
				entry := make(map[string]any, len(model)+3)
				for k, v := range model {
					entry[k] = v
				}
				entry["id"], entry["object"], entry["owned_by"] = p.Slug+"/"+id, "model", p.Name
				results[i] = append(results[i], entry)
			}
		}(i, p)
	}
	wg.Wait()
	out := []map[string]any{}
	for _, r := range results {
		out = append(out, r...)
	}
	return out, int(failed.Load())
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

// ---------------------------------------------------------------- response parsing

type usage struct {
	Prompt        int `json:"prompt_tokens"`
	Completion    int `json:"completion_tokens"`
	Total         int `json:"total_tokens"`
	PromptDetails struct {
		Cached int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		Reasoning int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type toolCall struct{ name, args string }

type parsed struct {
	text      string
	toolCalls []toolCall
	finish    string
	u         usage
	errMsg    string
}

func (p parsed) apply(t *store.Trace) {
	t.Response = truncate(p.text, maxTraceBody)
	if len(p.toolCalls) > 0 {
		calls := make([]map[string]any, len(p.toolCalls))
		for i, c := range p.toolCalls {
			var args any = c.args
			var decoded any
			if json.Unmarshal([]byte(c.args), &decoded) == nil {
				args = decoded
			}
			calls[i] = map[string]any{"name": c.name, "arguments": args}
		}
		b, _ := json.MarshalIndent(calls, "", "  ")
		t.Response = strings.TrimSpace(t.Response + "\n\n**Tool calls**\n```json\n" + string(b) + "\n```")
	}
	t.FinishReason = p.finish
	t.InputTokens, t.OutputTokens, t.TotalTokens = p.u.Prompt, p.u.Completion, p.u.Total
	if t.TotalTokens == 0 {
		t.TotalTokens = t.InputTokens + t.OutputTokens
	}
	t.CachedTokens, t.ReasoningTokens = p.u.PromptDetails.Cached, p.u.CompletionDetails.Reasoning
}

func parseOpenAI(b []byte) parsed {
	var x struct {
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if json.Unmarshal(b, &x) != nil || len(x.Choices) == 0 {
		return parsed{text: string(b)}
	}
	c := x.Choices[0]
	res := parsed{finish: c.FinishReason, u: x.Usage}
	switch v := c.Message.Content.(type) {
	case string:
		res.text = v
	case nil:
	default:
		encoded, _ := json.Marshal(v)
		res.text = string(encoded)
	}
	for _, tc := range c.Message.ToolCalls {
		res.toolCalls = append(res.toolCalls, toolCall{tc.Function.Name, tc.Function.Arguments})
	}
	return res
}

func parseSSE(v string) parsed {
	var out strings.Builder
	res := parsed{}
	calls := map[int]*toolCall{}
	order := []int{}
	for _, line := range strings.Split(v, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var x struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *usage `json:"usage"`
			XGroq *struct {
				Usage *usage `json:"usage"`
			} `json:"x_groq"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &x) != nil {
			continue
		}
		if x.Error != nil {
			res.errMsg = x.Error.Message
		}
		if len(x.Choices) > 0 {
			c := x.Choices[0]
			out.WriteString(c.Delta.Content)
			for _, tc := range c.Delta.ToolCalls {
				call, ok := calls[tc.Index]
				if !ok {
					call = &toolCall{}
					calls[tc.Index] = call
					order = append(order, tc.Index)
				}
				call.name += tc.Function.Name
				call.args += tc.Function.Arguments
			}
			if c.FinishReason != "" {
				res.finish = c.FinishReason
			}
		}
		if x.Usage != nil && x.Usage.Total+x.Usage.Prompt > 0 {
			res.u = *x.Usage
		} else if x.XGroq != nil && x.XGroq.Usage != nil {
			res.u = *x.XGroq.Usage
		}
	}
	res.text = out.String()
	for _, i := range order {
		res.toolCalls = append(res.toolCalls, *calls[i])
	}
	return res
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

// copyResponseHeaders forwards content metadata plus the rate-limit headers SDKs
// use to back off (Retry-After, x-ratelimit-*, anthropic-ratelimit-*).
func copyResponseHeaders(dst, src http.Header) {
	for k, v := range src {
		lower := strings.ToLower(k)
		switch {
		case lower == "content-type", lower == "cache-control", lower == "x-request-id", lower == "openai-processing-ms", lower == "retry-after", lower == "request-id",
			strings.HasPrefix(lower, "x-ratelimit-"), strings.HasPrefix(lower, "anthropic-ratelimit-"):
			dst[k] = v
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
	switch x := v.(type) {
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

// ---------------------------------------------------------------- pricing

// USD per million input/output tokens for common models. Dashboard prices
// (model_prices) take precedence; unknown models cost zero rather than a guess.
var builtinPrices = map[string][2]float64{
	"gpt-4o-mini": {0.15, 0.60}, "gpt-4o": {2.50, 10}, "gpt-4.1": {2, 8}, "gpt-4.1-mini": {0.40, 1.60}, "gpt-4.1-nano": {0.10, 0.40},
	"gpt-5": {1.25, 10}, "gpt-5-mini": {0.25, 2}, "gpt-5-nano": {0.05, 0.40}, "o1": {15, 60}, "o3": {2, 8}, "o3-mini": {1.10, 4.40}, "o4-mini": {1.10, 4.40}, "gpt-3.5-turbo": {0.50, 1.50},
	"claude-3-haiku": {0.25, 1.25}, "claude-3-5-haiku": {0.80, 4}, "claude-3-5-sonnet": {3, 15}, "claude-3-7-sonnet": {3, 15}, "claude-3-opus": {15, 75},
	"claude-sonnet-4": {3, 15}, "claude-sonnet-4-5": {3, 15}, "claude-opus-4": {15, 75}, "claude-opus-4-1": {15, 75}, "claude-haiku-4-5": {1, 5},
	"gemini-2.0-flash": {0.10, 0.40}, "gemini-2.0-flash-lite": {0.075, 0.30}, "gemini-2.5-flash": {0.30, 2.50}, "gemini-2.5-flash-lite": {0.10, 0.40}, "gemini-2.5-pro": {1.25, 10},
	"text-embedding-3-small": {0.02, 0}, "text-embedding-3-large": {0.13, 0}, "text-embedding-ada-002": {0.10, 0}, "gemini-embedding-001": {0.15, 0},
	"llama-3.3-70b-versatile": {0.59, 0.79}, "llama-3.1-8b-instant": {0.05, 0.08}, "openai/gpt-oss-120b": {0.15, 0.75}, "openai/gpt-oss-20b": {0.075, 0.30},
}

var snapshotSuffix = regexp.MustCompile(`-(?:\d{4}-\d{2}-\d{2}|\d{8}|latest)$`)

func (g *Gateway) estimateCost(model string, in, out int) float64 {
	if in == 0 && out == 0 {
		return 0
	}
	if p, ok := g.Store.CustomPrice(model); ok {
		return (float64(in)*p.Input + float64(out)*p.Output) / 1_000_000
	}
	key := snapshotSuffix.ReplaceAllString(strings.TrimPrefix(strings.ToLower(model), "models/"), "")
	p, ok := builtinPrices[key]
	if !ok {
		return 0
	}
	return (float64(in)*p[0] + float64(out)*p[1]) / 1_000_000
}

// BuiltinPrices exposes the bundled table for the dashboard.
func BuiltinPrices() map[string][2]float64 { return builtinPrices }
