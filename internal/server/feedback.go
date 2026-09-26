package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/evolvue/nexa-gateway/internal/store"
	"github.com/go-chi/chi/v5"
)

func (s *Server) feedbackLoops(w http.ResponseWriter, r *http.Request) {
	loops, err := s.Store.FeedbackLoops()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, loops)
}

func (s *Server) createFeedbackLoop(w http.ResponseWriter, r *http.Request) {
	var l store.FeedbackLoop
	if !decode(w, r, &l) {
		return
	}
	v, err := s.Store.CreateFeedbackLoop(l)
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	v.JevAPIKey = ""
	writeJSON(w, 201, v)
}

func (s *Server) updateFeedbackLoop(w http.ResponseWriter, r *http.Request) {
	var l store.FeedbackLoop
	if !decode(w, r, &l) {
		return
	}
	l.ID = chi.URLParam(r, "id")
	v, err := s.Store.UpdateFeedbackLoop(l)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Feedback loop not found.")
		return
	}
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	v.JevAPIKey = ""
	writeJSON(w, 200, v)
}

func (s *Server) deleteFeedbackLoop(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteFeedbackLoop(chi.URLParam(r, "id")); err != nil {
		writeError(w, 404, "Feedback loop not found.")
		return
	}
	w.WriteHeader(204)
}

// setFeedbackStatus pauses a loop (freezing today's split) or resumes learning.
func (s *Server) setFeedbackStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l, err := s.Store.FeedbackLoop(chi.URLParam(r, "id"))
		if err != nil {
			writeError(w, 404, "Feedback loop not found.")
			return
		}
		var frozen map[string]float64
		if status == "paused" {
			frozen = s.Gateway.LoopSplit(l).Learned
		}
		v, err := s.Store.SetFeedbackStatus(l.ID, status, frozen)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		v.JevAPIKey = ""
		writeJSON(w, 200, v)
	}
}

func (s *Server) resetFeedbackLoop(w http.ResponseWriter, r *http.Request) {
	l, err := s.Store.FeedbackLoop(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "Feedback loop not found.")
		return
	}
	if err := s.Store.ResetFeedback(l.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if l.Status == "paused" { // a reset loop starts again from an even split
		if _, err := s.Store.SetFeedbackStatus(l.ID, "paused", nil); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	w.WriteHeader(204)
}

// feedbackStats returns everything the loop page shows: split, per-model quality and
// traffic metrics, the split over time, coverage and the latest ratings.
func (s *Server) feedbackStats(w http.ResponseWriter, r *http.Request) {
	l, err := s.Store.FeedbackLoop(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "Feedback loop not found.")
		return
	}
	split := s.Gateway.LoopSplit(l)
	votes, err := s.Store.Votes(l.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	traffic, err := s.Store.LoopTraffic(l.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	recent, _ := s.Store.RecentVotes(l.ID, 12)
	allTime := map[string]*store.ArmCount{}
	uncertain, human, jev := 0, 0, 0
	for _, v := range votes {
		if v.Verdict == "uncertain" {
			uncertain++
		} else {
			c := allTime[v.ArmID]
			if c == nil {
				c = &store.ArmCount{}
				allTime[v.ArmID] = c
			}
			c.Up += v.Up
			c.Down += v.Down
		}
		if v.Source == "jev" {
			jev++
		} else {
			human++
		}
	}
	arms := []map[string]any{}
	served := 0
	lead := s.Gateway.LoopLeader(l, split)
	for _, a := range l.Arms {
		p, _ := s.Store.Provider(a.ProviderID)
		counts := split.Counts[a.ID]
		t := traffic[a.ID]
		if t == nil {
			t = &store.ArmTraffic{}
		}
		served += t.Requests
		likeRate := 0.0
		if counts.Up+counts.Down > 0 {
			likeRate = counts.Up / (counts.Up + counts.Down)
		}
		all := allTime[a.ID]
		if all == nil {
			all = &store.ArmCount{}
		}
		costPerLike := 0.0
		if all.Up > 0 {
			costPerLike = t.Cost / all.Up
		}
		avgCost, errorRate := 0.0, 0.0
		if t.Requests > 0 {
			avgCost, errorRate = t.Cost/float64(t.Requests), float64(t.Errors)/float64(t.Requests)
		}
		arms = append(arms, map[string]any{
			"id": a.ID, "provider_id": a.ProviderID, "provider_name": p.Name, "provider_type": p.Type, "provider_enabled": p.Enabled, "model": a.Model, "label": p.Slug + "/" + a.Model,
			"share": split.Shares[a.ID], "learned_share": split.Learned[a.ID], "prob_best": split.ProbBest[a.ID],
			"up": counts.Up, "down": counts.Down, "all_up": all.Up, "all_down": all.Down,
			"like_rate": likeRate, "interval": split.Interval[a.ID],
			"requests": t.Requests, "errors": t.Errors, "error_rate": errorRate,
			"avg_latency_ms": t.AvgLatencyMS, "p95_latency_ms": t.P95LatencyMS, "avg_cost": avgCost, "cost_per_like": costPerLike,
		})
	}
	counted := len(votes) - uncertain
	l.JevAPIKey = ""
	writeJSON(w, 200, map[string]any{
		"loop": l, "arms": arms, "history": s.Gateway.SplitHistory(l, votes, 40), "recent": recent,
		"votes": len(votes), "counted_votes": counted, "uncertain_votes": uncertain, "human_votes": human, "jev_votes": jev,
		"served": served, "coverage": ratio(len(votes), served),
		"leader": lead, "judging": s.Store.JudgeQueue(r.Context(), l.ID),
	})
}

// retryJudging puts the loop's failed Jev ratings back on the queue.
func (s *Server) retryJudging(w http.ResponseWriter, r *http.Request) {
	l, err := s.Store.FeedbackLoop(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "Feedback loop not found.")
		return
	}
	n, err := s.Store.RetryFailedJudges(r.Context(), l.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"requeued": n})
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// vote records a person's rating of a feedback-loop response. It is used by the
// dashboard (session) and by applications through POST /v1/feedback (API key).
func (s *Server) vote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TraceID string `json:"trace_id"`
		Rating  string `json:"rating"` // up | down | none
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Rating != "up" && in.Rating != "down" && in.Rating != "none" {
		writeError(w, 400, "rating must be up, down or none")
		return
	}
	t, err := s.Store.Trace(in.TraceID)
	if err != nil {
		writeError(w, 404, "Trace not found.")
		return
	}
	var meta struct {
		FeedbackLoop bool   `json:"feedback_loop"`
		LoopID       string `json:"loop_id"`
		ArmID        string `json:"arm_id"`
		Served       bool   `json:"served"`
	}
	if json.Unmarshal([]byte(t.Metadata), &meta) != nil || !meta.FeedbackLoop {
		writeError(w, 400, "This response was not served by a feedback loop.")
		return
	}
	if !meta.Served || t.Status != "success" {
		writeError(w, 400, "Only successful responses can be rated.")
		return
	}
	l, err := s.Store.FeedbackLoop(meta.LoopID)
	if err != nil {
		writeError(w, 404, "Feedback loop not found.")
		return
	}
	if l.Mode != "human" {
		writeError(w, 409, "This feedback loop is judged by Jev, not by people.")
		return
	}
	if in.Rating == "none" {
		if err := s.Store.DeleteVote(t.ID); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		w.WriteHeader(204)
		return
	}
	v := store.FeedbackVote{TraceID: t.ID, LoopID: l.ID, ArmID: meta.ArmID, Source: "human", Verdict: "liked", Up: 1, Confidence: 1}
	if in.Rating == "down" {
		v.Verdict, v.Up, v.Down = "disliked", 0, 1
	}
	if err := s.Store.SaveVote(v); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.Gateway.AfterVote(l.ID)
	writeJSON(w, 200, v)
}
