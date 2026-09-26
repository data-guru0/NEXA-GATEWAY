package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	routing "github.com/evolvue/nexa-gateway/internal/router"
	"github.com/evolvue/nexa-gateway/internal/store"
)

const (
	splitDraws      = 4000 // Monte Carlo draws behind every displayed/used split
	armTimeout      = 120 * time.Second
	jevMinimumTrust = 0.6 // Jev verdicts below this confidence are recorded but not counted
)

// Split is the current traffic allocation of a feedback loop.
type Split struct {
	Shares   map[string]float64    `json:"shares"`    // what traffic uses now (frozen while paused)
	Learned  map[string]float64    `json:"learned"`   // what the votes say, even while paused
	ProbBest map[string]float64    `json:"prob_best"` // chance each model is the best one
	Interval map[string][2]float64 `json:"interval"`  // 90% credible range of each like rate
	Counts   map[string]store.ArmCount
}

type splitCacheEntry struct {
	version int64
	split   Split
}

var (
	splitMu    sync.Mutex
	splitCache = map[string]splitCacheEntry{}
)

// gammaSample draws from Gamma(a, 1) with the Marsaglia–Tsang method.
func gammaSample(a float64) float64 {
	if a < 1 {
		return gammaSample(a+1) * math.Pow(rand.Float64(), 1/a)
	}
	d := a - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := rand.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rand.Float64()
		if u < 1-0.0331*x*x*x*x || math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

// betaSample draws a plausible like rate for a model with the given votes (uniform prior).
func betaSample(up, down float64) float64 {
	x, y := gammaSample(1+up), gammaSample(1+down)
	return x / (x + y)
}

// thompson estimates, by simulation, how often each model would win a Thompson
// draw (its chance of being best) and each model's 90% credible like-rate range.
func thompson(ids []string, counts map[string]store.ArmCount, draws int) (map[string]float64, map[string][2]float64) {
	wins := make(map[string]float64, len(ids))
	samples := make(map[string][]float64, len(ids))
	for i := 0; i < draws; i++ {
		best, bestValue := "", -1.0
		for _, id := range ids {
			v := betaSample(counts[id].Up, counts[id].Down)
			samples[id] = append(samples[id], v)
			if v > bestValue {
				best, bestValue = id, v
			}
		}
		wins[best]++
	}
	// Models with identical votes are exactly tied; use that instead of simulation noise.
	tied := true
	for _, id := range ids {
		tied = tied && counts[id] == counts[ids[0]]
	}
	probBest := make(map[string]float64, len(ids))
	interval := make(map[string][2]float64, len(ids))
	for _, id := range ids {
		probBest[id] = wins[id] / float64(draws)
		if tied {
			probBest[id] = 1 / float64(len(ids))
		}
		s := samples[id]
		sort.Float64s(s)
		interval[id] = [2]float64{s[len(s)*5/100], s[len(s)*95/100]}
	}
	return probBest, interval
}

// withFloor guarantees every model at least `floor` of traffic and shares the rest
// in proportion to its chance of being best (Thompson sampling's probability matching).
func withFloor(ids []string, probBest map[string]float64, floor float64) map[string]float64 {
	out := make(map[string]float64, len(ids))
	rest := 1 - floor*float64(len(ids))
	for _, id := range ids {
		out[id] = floor + rest*probBest[id]
	}
	return out
}

func armIDs(l store.FeedbackLoop) []string {
	ids := make([]string, len(l.Arms))
	for i, a := range l.Arms {
		ids[i] = a.ID
	}
	return ids
}

// LoopSplit returns the loop's split, recomputed only when votes or settings change.
func (g *Gateway) LoopSplit(l store.FeedbackLoop) Split {
	version := g.Store.FeedbackVersion()
	splitMu.Lock()
	cached, ok := splitCache[l.ID]
	splitMu.Unlock()
	if ok && cached.version == version {
		return cached.split
	}
	ids := armIDs(l)
	counts := g.Store.ArmCounts(l)
	probBest, interval := thompson(ids, counts, splitDraws)
	learned := withFloor(ids, probBest, l.MinShare)
	shares := learned
	if l.Status == "paused" && len(l.FrozenShares) > 0 {
		shares = make(map[string]float64, len(ids))
		total := 0.0
		for _, id := range ids {
			shares[id] = l.FrozenShares[id]
			total += shares[id]
		}
		for _, id := range ids { // models added while paused get an even slice
			if total <= 0 {
				shares[id] = 1 / float64(len(ids))
			} else {
				shares[id] /= total
			}
		}
	}
	split := Split{Shares: shares, Learned: learned, ProbBest: probBest, Interval: interval, Counts: counts}
	splitMu.Lock()
	splitCache[l.ID] = splitCacheEntry{version, split}
	splitMu.Unlock()
	return split
}

// SplitHistory replays the loop's votes and returns the learned split at up to
// `points` evenly spaced moments, for the split-over-time chart.
func (g *Gateway) SplitHistory(l store.FeedbackLoop, votes []store.FeedbackVote, points int) []map[string]any {
	ids := armIDs(l)
	even := map[string]float64{}
	for _, id := range ids {
		even[id] = 1 / float64(len(ids))
	}
	history := []map[string]any{{"at": l.CreatedAt, "votes": 0, "shares": even}}
	if len(votes) == 0 {
		return history
	}
	window := map[string][]store.FeedbackVote{}
	step := max(1, len(votes)/points)
	for i, v := range votes {
		if v.Verdict != "uncertain" {
			window[v.ArmID] = append(window[v.ArmID], v)
			if len(window[v.ArmID]) > l.Window {
				window[v.ArmID] = window[v.ArmID][1:]
			}
		}
		if (i+1)%step != 0 && i != len(votes)-1 {
			continue
		}
		counts := map[string]store.ArmCount{}
		for id, list := range window {
			var c store.ArmCount
			for _, w := range list {
				c.Up += w.Up
				c.Down += w.Down
			}
			counts[id] = c
		}
		probBest, _ := thompson(ids, counts, 1500)
		history = append(history, map[string]any{"at": v.CreatedAt, "votes": i + 1, "shares": withFloor(ids, probBest, l.MinShare)})
	}
	return history
}

// pickWeighted draws one model with probability equal to its share.
func pickWeighted(ids []string, shares map[string]float64) string {
	total := 0.0
	for _, id := range ids {
		total += shares[id]
	}
	r := rand.Float64() * total
	for _, id := range ids {
		r -= shares[id]
		if r <= 0 {
			return id
		}
	}
	return ids[len(ids)-1]
}

// feedbackLoop serves feedback/<slug>: it draws a model from the learned split,
// falls back to the next-best models if it fails, and (in Jev mode) has Jev rate
// the answer afterwards.
func (g *Gateway) feedbackLoop(w http.ResponseWriter, r *http.Request, payload map[string]any, rawMessages json.RawMessage, t *store.Trace, start time.Time) {
	slug := strings.TrimPrefix(payload["model"].(string), "feedback/")
	fail := func(status int, msg string) {
		t.StatusCode, t.Error = status, msg
		writeOpenAIError(w, status, msg, "routing_error")
	}
	loop, err := g.Store.FeedbackLoop(slug)
	if err != nil {
		fail(404, fmt.Sprintf("Feedback loop %q not found.", slug))
		return
	}
	t.ProviderName = "feedback/" + loop.Slug
	split := g.LoopSplit(loop)
	candidates := []routeCandidate{}
	available := []string{}
	byID := map[string]routeCandidate{}
	for _, a := range loop.Arms {
		p, err := g.Store.Provider(a.ProviderID)
		if err != nil || !p.Enabled {
			continue
		}
		c := routeCandidate{ID: a.ID, Label: p.Slug + "/" + a.Model, Provider: p, Model: a.Model}
		byID[a.ID] = c
		available = append(available, a.ID)
	}
	if len(available) == 0 {
		fail(503, "No model in this feedback loop has an enabled provider.")
		return
	}
	sampled := pickWeighted(available, split.Shares)
	candidates = append(candidates, byID[sampled])
	rest := []string{}
	for _, id := range available {
		if id != sampled {
			rest = append(rest, id)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return split.Shares[rest[i]] > split.Shares[rest[j]] })
	for _, id := range rest {
		candidates = append(candidates, byID[id])
	}
	attempts, chosen := g.runRoutes(w, r, payload, t, start, candidates, 0, armTimeout, func(h http.Header, c routeCandidate) {
		h.Set("X-Nexa-Feedback-Loop", loop.Slug)
		h.Set("X-Nexa-Feedback-Arm", c.ID)
		h.Set("X-Nexa-Feedback-Mode", loop.Mode)
		h.Set("X-Nexa-Routed-Model", c.Label)
	})
	armID := sampled
	if chosen != nil {
		armID = chosen.ID
	}
	meta, _ := json.Marshal(map[string]any{"feedback_loop": true, "loop_id": loop.ID, "loop_slug": loop.Slug, "mode": loop.Mode, "sampled_arm": sampled, "arm_id": armID, "served": chosen != nil, "shares": split.Shares, "attempts": attempts})
	t.Metadata = string(meta)
	if chosen == nil || t.Status != "success" || loop.Mode != "jev" || loop.Status != "running" || rand.Float64() >= loop.JevSample {
		return
	}
	traceID, response, key := t.ID, t.Response, routing.JevKey(loop.JevAPIKey)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		verdict, confidence, err := g.Router.Judge(ctx, key, rawMessages, response)
		if err != nil { // no vote is better than a guessed one; traffic keeps flowing
			slog.Warn("jev could not rate a feedback-loop answer", "loop", loop.Slug, "trace_id", traceID, "error", err.Error())
			return
		}
		vote := store.FeedbackVote{TraceID: traceID, LoopID: loop.ID, ArmID: armID, Source: "jev", Verdict: verdict, Confidence: confidence}
		switch {
		case confidence < jevMinimumTrust:
			vote.Verdict = "uncertain"
		case verdict == "liked":
			vote.Up = confidence
		default:
			vote.Down = confidence
		}
		_ = g.Store.SaveVote(vote)
	}()
}
