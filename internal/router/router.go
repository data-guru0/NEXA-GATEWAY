package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evolvue/nexa-gateway/internal/store"
)

const (
	jevURL         = "https://api.typesafe.ai/v1/systemone"
	jevInputChars  = 8000 // conversation tail sent to Jev
	jevCacheTTL    = 10 * time.Minute
	jevCacheMax    = 2000
	jevTurnsToSend = 6
)

var lanes = []string{"low", "medium", "high"}

type Signals struct {
	Complexity              string             `json:"complexity"` // Jev's raw difficulty
	Lane                    string             `json:"lane"`       // lane actually routed to
	Confidence              float64            `json:"confidence"`
	Escalated               bool               `json:"escalated"` // moved up a lane for low confidence
	Cached                  bool               `json:"cached"`
	JevLatencyMS            float64            `json:"jev_latency_ms"`
	ContextTokens           int                `json:"context_tokens"`
	ExpectedOutputTokens    int                `json:"expected_output_tokens"`
	Capabilities            []string           `json:"capabilities"`
	ComplexityProbabilities map[string]float64 `json:"complexity_probabilities,omitempty"`
}
type RankedTarget struct {
	Target       store.RoutingTarget `json:"target"`
	Provider     store.Provider      `json:"-"`
	ProviderName string              `json:"provider_name"`
	ProviderSlug string              `json:"provider_slug"`
	Score        float64             `json:"score"`
	Reasons      []string            `json:"reasons"`
	Healthy      bool                `json:"healthy"`
}
type Decision struct {
	ProfileID        string         `json:"profile_id"`
	ProfileName      string         `json:"profile_name"`
	ProfileSlug      string         `json:"profile_slug"`
	Signals          Signals        `json:"signals"`
	Ranked           []RankedTarget `json:"ranked"`
	SelectedTargetID string         `json:"selected_target_id"`
	LowConfidence    bool           `json:"low_confidence"`
	EngineError      string         `json:"engine_error,omitempty"`
}

type jevResult struct {
	complexity    string
	confidence    float64
	probabilities map[string]float64
	expires       time.Time
}

type Engine struct {
	Store  *store.Store
	Client *http.Client
	mu     sync.Mutex
	cache  map[string]jevResult
}

func New(s *store.Store) *Engine {
	return &Engine{Store: s, Client: &http.Client{Timeout: 3 * time.Second}, cache: map[string]jevResult{}}
}

// JevInput renders what Jev judges: the system prompt plus the last few turns,
// keeping the newest text when it exceeds jevInputChars.
func JevInput(raw json.RawMessage) string {
	var msgs []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	if json.Unmarshal(raw, &msgs) != nil {
		return tail(string(raw), jevInputChars)
	}
	var system []string
	var turns []string
	for _, m := range msgs {
		text := contentText(m.Content)
		if text == "" {
			continue
		}
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, text)
			continue
		}
		turns = append(turns, strings.ToUpper(m.Role)+": "+text)
	}
	if len(turns) > jevTurnsToSend {
		turns = turns[len(turns)-jevTurnsToSend:]
	}
	if len(turns) == 1 && len(system) == 0 {
		return tail(strings.TrimPrefix(turns[0], "USER: "), jevInputChars)
	}
	out := strings.Join(turns, "\n\n")
	if len(system) > 0 {
		out = "SYSTEM: " + strings.Join(system, "\n") + "\n\n" + out
	}
	return tail(out, jevInputChars)
}

func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		parts := []string{}
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				} else if m["type"] == "image_url" {
					parts = append(parts, "[image]")
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

func (e *Engine) Decide(ctx context.Context, p store.RoutingProfile, messages json.RawMessage, payload map[string]any) (Decision, error) {
	d := Decision{ProfileID: p.ID, ProfileName: p.Name, ProfileSlug: p.Slug}
	if len(p.Targets) == 0 {
		return d, errors.New("routing profile has no targets")
	}
	d.Signals, d.EngineError = e.classify(ctx, p, JevInput(messages), true)
	d.Signals.ContextTokens = estimateTokens(string(messages))
	d.Signals.ExpectedOutputTokens = numeric(payload["max_completion_tokens"])
	if d.Signals.ExpectedOutputTokens == 0 {
		d.Signals.ExpectedOutputTokens = numeric(payload["max_tokens"])
	}
	if d.Signals.ExpectedOutputTokens == 0 {
		d.Signals.ExpectedOutputTokens = 1024
	}
	d.Signals.Capabilities = requestCapabilities(payload)
	d.Signals.Lane = d.Signals.Complexity
	d.LowConfidence = d.EngineError == "" && d.Signals.Confidence < p.ConfidenceThreshold
	if d.LowConfidence && d.Signals.Lane != "high" {
		d.Signals.Lane = lanes[laneIndex(d.Signals.Lane)+1]
		d.Signals.Escalated = true
	}
	skipped := []string{}
	for _, t := range p.Targets {
		if !t.Enabled {
			continue
		}
		pr, providerErr := e.Store.Provider(t.ProviderID)
		if providerErr != nil || !pr.Enabled {
			continue
		}
		if t.ContextWindow > 0 && d.Signals.ContextTokens > t.ContextWindow {
			continue
		}
		if t.MaxOutputTokens > 0 && d.Signals.ExpectedOutputTokens > t.MaxOutputTokens {
			continue
		}
		if missing := missingCapability(t.Unsupported, d.Signals.Capabilities); missing != "" {
			skipped = append(skipped, pr.Slug+"/"+t.Model+" lacks "+missing)
			continue
		}
		h := e.Store.RoutingHealth(t.ProviderID, t.Model)
		healthy := h.Requests < 3 || float64(h.Failures)/float64(h.Requests) < .5
		score, reasons := scoreTarget(t, d.Signals.Lane)
		if !healthy {
			reasons = append(reasons, "recent failures")
		}
		d.Ranked = append(d.Ranked, RankedTarget{t, pr, pr.Name, pr.Slug, score, reasons, healthy})
	}
	// Lane match decides first; within an equal score a healthy route beats one
	// that is failing, then the configured priority breaks the tie.
	sort.SliceStable(d.Ranked, func(i, j int) bool {
		a, b := d.Ranked[i], d.Ranked[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Healthy != b.Healthy {
			return a.Healthy
		}
		return a.Target.Priority < b.Target.Priority
	})
	if len(d.Ranked) == 0 {
		if len(skipped) > 0 {
			return d, errors.New("no routing target supports this request: " + strings.Join(skipped, "; "))
		}
		return d, errors.New("no enabled routing target satisfies this request")
	}
	d.SelectedTargetID = d.Ranked[0].Target.ID
	return d, nil
}

func laneIndex(lane string) int {
	for i, l := range lanes {
		if l == lane {
			return i
		}
	}
	return 1
}

func scoreTarget(t store.RoutingTarget, lane string) (float64, []string) {
	distance := laneIndex(t.Tier) - laneIndex(lane)
	switch {
	case distance == 0:
		return 100, []string{"difficulty lane match"}
	case distance > 0:
		return 100 - float64(distance*25), []string{"higher-capability fallback"}
	default:
		return 25 - float64(-distance*10), []string{"lower-capability last resort"}
	}
}

// CheckJev calls Jev directly (bypassing the cache) so the dashboard can verify a profile's key.
func (e *Engine) CheckJev(ctx context.Context, p store.RoutingProfile) (Signals, string) {
	return e.classify(ctx, p, "What is 2+2?", false)
}

func jevKey(p store.RoutingProfile) string {
	key := p.JevAPIKey
	if key == "" {
		key = os.Getenv("JEV_API_KEY")
	}
	if key == "" {
		key = os.Getenv("TYPESAFE_API_KEY")
	}
	// API keys never contain whitespace; accepting the first token prevents an
	// accidental unquoted note in an env file from becoming part of the secret.
	if fields := strings.Fields(key); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// classify asks Jev for the prompt's difficulty. On any failure it returns the
// profile's fallback lane and the reason, so routing always proceeds.
func (e *Engine) classify(ctx context.Context, p store.RoutingProfile, input string, useCache bool) (Signals, string) {
	fallback := func(reason string) (Signals, string) {
		return Signals{Complexity: p.FallbackLane}, reason + "; used the " + strings.ToUpper(p.FallbackLane) + " fallback lane"
	}
	key := jevKey(p)
	if key == "" {
		return fallback("JEV_API_KEY is not configured")
	}
	cacheKey := sha256.Sum256([]byte(key + "\x00" + input))
	id := hex.EncodeToString(cacheKey[:])
	if useCache {
		e.mu.Lock()
		hit, ok := e.cache[id]
		e.mu.Unlock()
		if ok && time.Now().Before(hit.expires) {
			return Signals{Complexity: hit.complexity, Confidence: hit.confidence, ComplexityProbabilities: hit.probabilities, Cached: true}, ""
		}
	}
	body := map[string]any{"state": input, "model": "jev-latest", "questions": map[string]any{"complexity": map[string]any{"type": "choice", "instructions": "Classify only the reasoning difficulty of the latest request in this conversation.", "criteria": map[string]string{"low": "Simple lookup, definition, rewrite, extraction, or casual question", "medium": "A multi-step task requiring moderate reasoning or synthesis", "high": "Expert, ambiguous, high-stakes, or deeply technical reasoning"}}}}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", jevURL, bytes.NewReader(raw))
	if err != nil {
		return fallback(err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := e.Client.Do(req)
	if err != nil {
		return fallback("Jev request failed: " + err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	elapsed := float64(time.Since(started)) / float64(time.Millisecond)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fallback(fmt.Sprintf("Jev returned %d", resp.StatusCode))
	}
	var out struct {
		Answers map[string]struct {
			Choice        string             `json:"choice"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
	}
	if json.Unmarshal(b, &out) != nil {
		return fallback("invalid Jev response")
	}
	c := out.Answers["complexity"]
	if c.Choice != "low" && c.Choice != "medium" && c.Choice != "high" {
		return fallback("Jev returned an invalid difficulty")
	}
	confidence := c.Confidence
	if confidence == 0 {
		for _, v := range c.Probabilities {
			confidence = max(confidence, v)
		}
	}
	e.mu.Lock()
	// ponytail: whole-cache reset at the cap; an LRU is only worth it if hit rates suffer.
	if len(e.cache) >= jevCacheMax {
		e.cache = map[string]jevResult{}
	}
	e.cache[id] = jevResult{c.Choice, confidence, c.Probabilities, time.Now().Add(jevCacheTTL)}
	e.mu.Unlock()
	return Signals{Complexity: c.Choice, Confidence: confidence, ComplexityProbabilities: c.Probabilities, JevLatencyMS: elapsed}, ""
}

func requestCapabilities(p map[string]any) []string {
	o := []string{}
	if tools, ok := p["tools"].([]any); ok && len(tools) > 0 {
		o = append(o, "tools")
	}
	if _, ok := p["response_format"]; ok {
		o = append(o, "json")
	}
	b, _ := json.Marshal(p["messages"])
	if bytes.Contains(b, []byte(`"image_url"`)) {
		o = append(o, "vision")
	}
	return o
}

func missingCapability(unsupported, need []string) string {
	for _, n := range need {
		for _, u := range unsupported {
			if strings.EqualFold(u, n) {
				return n
			}
		}
	}
	return ""
}

func numeric(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len([]rune(s)) + 3) / 4
}
