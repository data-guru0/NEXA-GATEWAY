package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	routing "github.com/evolvue/nexa-gateway/internal/router"
	"github.com/evolvue/nexa-gateway/internal/store"
	"github.com/google/uuid"
)

// Jev judging runs through a durable Redis Streams queue (see store/jobs.go):
// a rating survives restarts and crashes, and any instance may do the work.

const judgeMaxAttempts = 4

var errNoJevKey = errors.New("no Jev key: add one to the loop or set JEV_API_KEY")

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// StartJudging runs this instance's judging workers until ctx ends.
// NEXA_JUDGE_WORKERS (default 4, 0 = none on this instance) sets how many Jev
// calls run at once here; NEXA_JUDGE_RECLAIM_AFTER (default 60s) is how long a
// job may stay unacknowledged before another worker takes it over.
func (g *Gateway) StartJudging(ctx context.Context) {
	workers := envInt("NEXA_JUDGE_WORKERS", 4)
	if workers == 0 {
		slog.Info("judging workers disabled on this instance")
		return
	}
	host, _ := os.Hostname()
	instance := fmt.Sprintf("%s-%d-%s", host, os.Getpid(), uuid.NewString()[:8])
	reclaimAfter := envDuration("NEXA_JUDGE_RECLAIM_AFTER", time.Minute)
	for i := 0; i < workers; i++ {
		consumer := fmt.Sprintf("%s-%d", instance, i)
		go func() {
			for ctx.Err() == nil {
				jobs, err := g.Store.NextJudgeJobs(ctx, consumer, 1, 5*time.Second)
				if err != nil {
					if ctx.Err() == nil {
						slog.Warn("judging queue read failed", "error", err.Error())
						time.Sleep(2 * time.Second)
					}
					continue
				}
				for _, j := range jobs {
					g.runJudgeJob(ctx, j)
				}
			}
		}()
	}
	go func() { // janitor: requeue due retries and take over jobs abandoned by dead workers
		tick := time.NewTicker(min(reclaimAfter/2, 5*time.Second))
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if _, err := g.Store.ReleaseDueJudgeRetries(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("could not requeue judging retries", "error", err.Error())
			}
			jobs, err := g.Store.ReclaimJudgeJobs(ctx, instance+"-janitor", reclaimAfter)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("could not reclaim judging jobs", "error", err.Error())
				}
				continue
			}
			for _, j := range jobs {
				slog.Info("reclaimed an unfinished judging job", "trace_id", j.Job.TraceID)
				g.runJudgeJob(ctx, j)
			}
		}
	}()
	slog.Info("judging workers started", "workers", workers, "reclaim_after", reclaimAfter.String())
}

// runJudgeJob judges one answer and saves the rating. The job is acknowledged
// only after the rating is stored, retried with backoff on a transient error,
// and moved to the failed list when it cannot succeed.
func (g *Gateway) runJudgeJob(ctx context.Context, q store.QueuedJudgeJob) {
	job := q.Job
	done := func(err error) {
		if q.ID == "" {
			return // judged in-process while the queue was unavailable
		}
		if err != nil {
			slog.Warn("could not acknowledge a judging job", "trace_id", job.TraceID, "error", err.Error())
		}
	}
	loop, err := g.Store.FeedbackLoop(job.LoopID)
	if err != nil || loop.Mode != "jev" || job.TraceID == "" {
		done(g.Store.AckJudge(ctx, q.ID)) // the loop is gone or no longer judged by Jev
		return
	}
	key := routing.JevKey(loop.JevAPIKey)
	job.Attempts++
	var verdict string
	var confidence float64
	if key == "" {
		err = errNoJevKey
	} else {
		jctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		verdict, confidence, err = g.Router.JudgeText(jctx, key, job.Input, job.Response)
		cancel()
	}
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down: leave it pending, another worker reclaims it
		}
		job.LastError = err.Error()
		if errors.Is(err, errNoJevKey) || job.Attempts >= judgeMaxAttempts || strings.Contains(err.Error(), "returned 401") || strings.Contains(err.Error(), "returned 403") {
			slog.Warn("jev could not rate a feedback-loop answer; giving up", "loop", loop.Slug, "trace_id", job.TraceID, "attempts", job.Attempts, "error", err.Error())
			done(g.Store.FailJudge(ctx, q.ID, job))
			return
		}
		slog.Info("jev rating failed; will retry", "loop", loop.Slug, "trace_id", job.TraceID, "attempt", job.Attempts, "error", err.Error())
		done(g.Store.RetryJudgeLater(ctx, q.ID, job, store.JudgeDelay(job.Attempts)))
		return
	}
	vote := store.FeedbackVote{TraceID: job.TraceID, LoopID: loop.ID, ArmID: job.ArmID, Source: "jev", Verdict: verdict, Confidence: confidence}
	switch {
	case confidence < jevMinimumTrust:
		vote.Verdict = "uncertain"
	case verdict == "liked":
		vote.Up = confidence
	default:
		vote.Down = confidence
	}
	if err := g.Store.SaveVote(vote); err != nil { // not acknowledged: the job is retried after reclaim
		slog.Warn("could not save a Jev rating; the job will be reclaimed", "trace_id", job.TraceID, "error", err.Error())
		return
	}
	done(g.Store.AckJudge(ctx, q.ID))
	g.AfterVote(loop.ID)
}
