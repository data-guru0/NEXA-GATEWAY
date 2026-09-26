package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Durable queue for Jev judging, on Redis Streams with a consumer group.
//
// Every instance runs workers in the group "judges"; Redis gives each job to one
// worker and keeps it pending until the worker acknowledges it after the rating
// is saved. Jobs left pending by a worker that crashed or was restarted are
// reclaimed by another worker. Failed attempts wait in a sorted set (retry with
// backoff); jobs that keep failing go to a list shown on the loop page.
const (
	judgeStream  = "nexa:jobs:judge"
	judgeGroup   = "judges"
	judgeDelayed = "nexa:jobs:judge:delayed"
	judgeFailed  = "nexa:jobs:judge:failed"
	judgeFailMax = 500 // failed jobs kept for inspection and retry
)

// JudgeJob is one answer waiting for Jev's verdict. The Jev key is not stored
// in Redis; the worker reads it from the loop.
type JudgeJob struct {
	TraceID    string    `json:"trace_id"`
	LoopID     string    `json:"loop_id"`
	ArmID      string    `json:"arm_id"`
	Input      string    `json:"input"`    // the conversation as Jev reads it
	Response   string    `json:"response"` // the answer to judge
	Attempts   int       `json:"attempts"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	LastError  string    `json:"last_error,omitempty"`
	FailedAt   time.Time `json:"failed_at,omitzero"`
}

// QueuedJudgeJob is a job read from the stream, with the id used to acknowledge it.
type QueuedJudgeJob struct {
	ID  string
	Job JudgeJob
}

func (s *Store) ensureJudgeGroup(ctx context.Context) error {
	err := s.Redis.XGroupCreateMkStream(ctx, judgeStream, judgeGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// EnqueueJudge adds a job to the queue.
func (s *Store) EnqueueJudge(ctx context.Context, j JudgeJob) error {
	if j.EnqueuedAt.IsZero() {
		j.EnqueuedAt = time.Now().UTC()
	}
	b, _ := json.Marshal(j)
	return s.Redis.XAdd(ctx, &redis.XAddArgs{Stream: judgeStream, Values: map[string]any{"job": b}}).Err()
}

func decodeJudgeMessages(msgs []redis.XMessage) []QueuedJudgeJob {
	out := make([]QueuedJudgeJob, 0, len(msgs))
	for _, m := range msgs {
		var j JudgeJob
		raw, _ := m.Values["job"].(string)
		if json.Unmarshal([]byte(raw), &j) != nil {
			j = JudgeJob{LastError: "unreadable job"}
		}
		out = append(out, QueuedJudgeJob{ID: m.ID, Job: j})
	}
	return out
}

// NextJudgeJobs blocks up to `block` for new jobs for this consumer.
func (s *Store) NextJudgeJobs(ctx context.Context, consumer string, count int, block time.Duration) ([]QueuedJudgeJob, error) {
	res, err := s.Redis.XReadGroup(ctx, &redis.XReadGroupArgs{Group: judgeGroup, Consumer: consumer, Streams: []string{judgeStream, ">"}, Count: int64(count), Block: block}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		if strings.Contains(err.Error(), "NOGROUP") { // the stream was deleted (e.g. FLUSHALL); recreate
			return nil, s.ensureJudgeGroup(ctx)
		}
		return nil, err
	}
	if len(res) == 0 {
		return nil, nil
	}
	return decodeJudgeMessages(res[0].Messages), nil
}

// ReclaimJudgeJobs takes over jobs another worker has held longer than minIdle
// without acknowledging them (it crashed, restarted or hung).
func (s *Store) ReclaimJudgeJobs(ctx context.Context, consumer string, minIdle time.Duration) ([]QueuedJudgeJob, error) {
	msgs, _, err := s.Redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: judgeStream, Group: judgeGroup, Consumer: consumer, MinIdle: minIdle, Start: "0-0", Count: 50}).Result()
	if err != nil {
		if strings.Contains(err.Error(), "NOGROUP") {
			return nil, s.ensureJudgeGroup(ctx)
		}
		return nil, err
	}
	return decodeJudgeMessages(msgs), nil
}

// AckJudge marks a job done and removes it from the stream.
func (s *Store) AckJudge(ctx context.Context, id string) error {
	pipe := s.Redis.TxPipeline()
	pipe.XAck(ctx, judgeStream, judgeGroup, id)
	pipe.XDel(ctx, judgeStream, id)
	_, err := pipe.Exec(ctx)
	return err
}

// RetryJudgeLater acknowledges the attempt and schedules the job again after delay.
func (s *Store) RetryJudgeLater(ctx context.Context, id string, j JudgeJob, delay time.Duration) error {
	b, _ := json.Marshal(j)
	pipe := s.Redis.TxPipeline()
	pipe.ZAdd(ctx, judgeDelayed, redis.Z{Score: float64(time.Now().Add(delay).UnixMilli()), Member: b})
	pipe.XAck(ctx, judgeStream, judgeGroup, id)
	pipe.XDel(ctx, judgeStream, id)
	_, err := pipe.Exec(ctx)
	return err
}

// FailJudge acknowledges a job that will not be retried and keeps it in the failed list.
func (s *Store) FailJudge(ctx context.Context, id string, j JudgeJob) error {
	j.FailedAt = time.Now().UTC()
	b, _ := json.Marshal(j)
	pipe := s.Redis.TxPipeline()
	pipe.LPush(ctx, judgeFailed, b)
	pipe.LTrim(ctx, judgeFailed, 0, judgeFailMax-1)
	pipe.XAck(ctx, judgeStream, judgeGroup, id)
	pipe.XDel(ctx, judgeStream, id)
	_, err := pipe.Exec(ctx)
	return err
}

// Moves due retries back onto the stream, atomically, so two instances never both move one.
var releaseDue = redis.NewScript(`
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, 100)
for _, job in ipairs(due) do
  redis.call('ZREM', KEYS[1], job)
  redis.call('XADD', KEYS[2], '*', 'job', job)
end
return #due`)

// ReleaseDueJudgeRetries returns how many delayed jobs became due and were requeued.
func (s *Store) ReleaseDueJudgeRetries(ctx context.Context) (int, error) {
	return releaseDue.Run(ctx, s.Redis, []string{judgeDelayed, judgeStream}, time.Now().UnixMilli()).Int()
}

// JudgeQueueStats counts the queue, and the failed jobs of one loop.
type JudgeQueueStats struct {
	Waiting  int        `json:"waiting"`  // in the stream, not yet picked up
	Working  int        `json:"working"`  // picked up, not yet acknowledged
	Retrying int        `json:"retrying"` // waiting for their next attempt
	Failed   int        `json:"failed"`   // this loop's jobs that gave up
	Recent   []JudgeJob `json:"recent_failures"`
}

func (s *Store) JudgeQueue(ctx context.Context, loopID string) JudgeQueueStats {
	var st JudgeQueueStats
	length, _ := s.Redis.XLen(ctx, judgeStream).Result()
	if p, err := s.Redis.XPending(ctx, judgeStream, judgeGroup).Result(); err == nil {
		st.Working = int(p.Count)
	}
	st.Waiting = max(int(length)-st.Working, 0)
	retrying, _ := s.Redis.ZCard(ctx, judgeDelayed).Result()
	st.Retrying = int(retrying)
	st.Recent = []JudgeJob{}
	failed, _ := s.Redis.LRange(ctx, judgeFailed, 0, -1).Result()
	for _, raw := range failed {
		var j JudgeJob
		if json.Unmarshal([]byte(raw), &j) == nil && j.LoopID == loopID {
			st.Failed++
			if len(st.Recent) < 5 {
				j.Input, j.Response = "", "" // keep the answer text out of the dashboard payload
				st.Recent = append(st.Recent, j)
			}
		}
	}
	return st
}

// RetryFailedJudges puts a loop's failed jobs back on the queue with a fresh attempt count.
func (s *Store) RetryFailedJudges(ctx context.Context, loopID string) (int, error) {
	failed, err := s.Redis.LRange(ctx, judgeFailed, 0, -1).Result()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, raw := range failed {
		var j JudgeJob
		if json.Unmarshal([]byte(raw), &j) != nil || j.LoopID != loopID {
			continue
		}
		if removed, _ := s.Redis.LRem(ctx, judgeFailed, 1, raw).Result(); removed == 0 {
			continue // another instance already retried it
		}
		j.Attempts, j.LastError, j.FailedAt = 0, "", time.Time{}
		if err := s.EnqueueJudge(ctx, j); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// JudgeDelay is the wait before attempt n+1 (5 s, 30 s, 2 min).
func JudgeDelay(attempt int) time.Duration {
	return []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}[min(max(attempt-1, 0), 2)]
}
