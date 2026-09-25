package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/evolvue/nexa-gateway/internal/store"
)

type Signals struct {
	Complexity              string             `json:"complexity"`
	Confidence              float64            `json:"confidence"`
	Engine                  string             `json:"engine"`
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
	Objective        string         `json:"objective"`
	EngineError      string         `json:"engine_error,omitempty"`
}
type Engine struct {
	Store  *store.Store
	Client *http.Client
}

func New(s *store.Store) *Engine {
	return &Engine{s, &http.Client{Timeout: 8 * time.Second}}
}

func PromptFromMessages(raw json.RawMessage) string {
	var msgs []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	if json.Unmarshal(raw, &msgs) != nil {
		return string(raw)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == "user" {
			switch v := m.Content.(type) {
			case string:
				return v
			default:
				b, _ := json.Marshal(v)
				return string(b)
			}
		}
	}
	return string(raw)
}

func (e *Engine) Decide(ctx context.Context, p store.RoutingProfile, prompt string, payload map[string]any) (Decision, error) {
	d := Decision{ProfileID: p.ID, ProfileName: p.Name, ProfileSlug: p.Slug, Objective: p.Objective}
	if len(p.Targets) == 0 {
		return d, errors.New("routing profile has no targets")
	}
	d.Signals, d.SelectedTargetID, d.EngineError = e.classifyJev(ctx, p, prompt)
	if messages, ok := payload["messages"]; ok {
		if encoded, encodeErr := json.Marshal(messages); encodeErr == nil {
			d.Signals.ContextTokens = estimateTokens(string(encoded))
		}
	}
	if d.Signals.ContextTokens == 0 {
		d.Signals.ContextTokens = estimateTokens(prompt)
	}
	d.Signals.ExpectedOutputTokens = numeric(payload["max_completion_tokens"])
	if d.Signals.ExpectedOutputTokens == 0 {
		d.Signals.ExpectedOutputTokens = numeric(payload["max_tokens"])
	}
	if d.Signals.ExpectedOutputTokens == 0 {
		d.Signals.ExpectedOutputTokens = 1024
	}
	d.Signals.Capabilities = requestCapabilities(payload)
	d.LowConfidence = d.Signals.Confidence < p.ConfidenceThreshold
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
		h := e.Store.RoutingHealth(t.ProviderID, t.Model)
		healthy := h.Requests < 3 || float64(h.Failures)/float64(h.Requests) < .5
		score, reasons := scoreTarget(t, d.Signals)
		d.Ranked = append(d.Ranked, RankedTarget{t, pr, pr.Name, pr.Slug, score, reasons, healthy})
	}
	filtered := d.Ranked[:0]
	for _, candidate := range d.Ranked {
		if candidate.Score > -99999 {
			filtered = append(filtered, candidate)
		}
	}
	d.Ranked = filtered
	sort.SliceStable(d.Ranked, func(i, j int) bool {
		if d.Ranked[i].Score == d.Ranked[j].Score {
			return d.Ranked[i].Target.Priority < d.Ranked[j].Target.Priority
		}
		return d.Ranked[i].Score > d.Ranked[j].Score
	})
	if len(d.Ranked) == 0 {
		return d, errors.New("no enabled routing target satisfies this request")
	}
	d.SelectedTargetID = d.Ranked[0].Target.ID
	return d, nil
}

func scoreTarget(t store.RoutingTarget, s Signals) (float64, []string) {
	reasons := []string{}
	tier := map[string]int{"low": 0, "medium": 1, "high": 2}[t.Tier]
	want := map[string]int{"low": 0, "medium": 1, "high": 2}[s.Complexity]
	distance := tier - want
	var score float64
	switch {
	case distance == 0:
		score = 100
		reasons = append(reasons, "complexity match")
	case distance > 0:
		score = 100 - float64(distance*25)
		reasons = append(reasons, "higher-capability fallback")
	default:
		score = 25 - float64((-distance)*10)
		reasons = append(reasons, "lower-capability last resort")
	}
	return score, reasons
}

func (e *Engine) classifyJev(ctx context.Context, p store.RoutingProfile, prompt string) (Signals, string, string) {
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
		key = fields[0]
	}
	if key == "" {
		return Signals{Engine: "jev", Complexity: "high", Confidence: 0}, "", "JEV_API_KEY is not configured; used the safe Heavy difficulty lane"
	}
	body := map[string]any{"state": prompt, "model": "jev-latest", "questions": map[string]any{"complexity": map[string]any{"type": "choice", "instructions": "Classify only the reasoning difficulty of this prompt.", "criteria": map[string]string{"low": "Simple lookup, definition, rewrite, extraction, or casual question", "medium": "A multi-step task requiring moderate reasoning or synthesis", "high": "Expert, ambiguous, high-stakes, or deeply technical reasoning"}}}}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.typesafe.ai/v1/systemone", bytes.NewReader(raw))
	if err != nil {
		return Signals{Engine: "jev", Complexity: "high"}, "", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.Client.Do(req)
	if err != nil {
		return Signals{Engine: "jev", Complexity: "high"}, "", err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Signals{Engine: "jev", Complexity: "high"}, "", fmt.Sprintf("Jev returned %d", resp.StatusCode)
	}
	var out struct {
		Answers map[string]struct {
			Choice        string             `json:"choice"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
	}
	if json.Unmarshal(b, &out) != nil {
		return Signals{Engine: "jev", Complexity: "high"}, "", "invalid Jev response"
	}
	complexity := out.Answers["complexity"]
	if complexity.Choice != "low" && complexity.Choice != "medium" && complexity.Choice != "high" {
		return Signals{Engine: "jev", Complexity: "high", Confidence: 0}, "", "Jev returned an invalid difficulty; used the safe Heavy difficulty lane"
	}
	confidence := complexity.Confidence
	if confidence == 0 {
		for _, v := range complexity.Probabilities {
			if v > confidence {
				confidence = v
			}
		}
	}
	return Signals{Complexity: complexity.Choice, Confidence: confidence, Engine: "jev", ComplexityProbabilities: complexity.Probabilities}, "", ""
}

func requestCapabilities(p map[string]any) []string {
	o := []string{}
	if _, ok := p["tools"]; ok {
		o = append(o, "tools")
	}
	if _, ok := p["response_format"]; ok {
		o = append(o, "json")
	}
	b, _ := json.Marshal(p["messages"])
	if bytes.Contains(b, []byte("image_url")) {
		o = append(o, "vision")
	}
	return o
}
func supports(have, need []string) bool {
	for _, n := range need {
		if !contains(have, n) {
			return false
		}
	}
	return true
}
func contains(v []string, x string) bool {
	for _, s := range v {
		if strings.EqualFold(s, x) {
			return true
		}
	}
	return false
}
func numeric(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len([]rune(s)) + 3) / 4
}
