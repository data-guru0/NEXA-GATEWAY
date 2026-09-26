package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Redis holds everything Nexa instances must share: the response cache (with a
// vector index), rate-limit and budget counters, login throttling, Jev
// decisions, and change notifications so every instance drops stale caches.

const eventsChannel = "nexa:events"

func openRedis(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid NEXA_REDIS_URL: %w", err)
	}
	opts.Protocol = 2 // RESP2 keeps FT.SEARCH replies in their documented array shape
	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		err = rdb.Ping(ctx).Err()
		if err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		rdb.Close()
		return nil, err
	}
	if _, err := rdb.Do(ctx, "FT._LIST").Result(); err != nil {
		rdb.Close()
		return nil, errors.New("Redis has no search module; use Redis Stack (redis/redis-stack-server) or Redis 8+")
	}
	return rdb, nil
}

// ---------------------------------------------------------------- events

type eventHub struct {
	mu       sync.Mutex
	handlers map[string][]func()
}

// OnEvent runs fn whenever any instance publishes kind.
func (s *Store) OnEvent(kind string, fn func()) {
	s.events.mu.Lock()
	s.events.handlers[kind] = append(s.events.handlers[kind], fn)
	s.events.mu.Unlock()
}

// Publish tells every instance (including this one) that kind changed.
func (s *Store) Publish(kind string) {
	s.dispatch(kind)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Redis.Publish(ctx, eventsChannel, kind).Err(); err != nil {
		slog.Warn("could not publish change", "kind", kind, "error", err.Error())
	}
}

func (s *Store) dispatch(kind string) {
	s.events.mu.Lock()
	handlers := append([]func(){}, s.events.handlers[kind]...)
	s.events.mu.Unlock()
	for _, fn := range handlers {
		fn()
	}
}

func (s *Store) listen() {
	sub := s.Redis.Subscribe(context.Background(), eventsChannel)
	for msg := range sub.Channel() { // go-redis reconnects and resubscribes on its own
		s.dispatch(msg.Payload)
	}
}

// ---------------------------------------------------------------- rate limits

// Sliding-window request limit: one sorted-set entry per admitted request.
var requestWindow = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, tonumber(ARGV[1]) - 60000)
local n = redis.call('ZCARD', KEYS[1])
if n >= tonumber(ARGV[2]) then
  local oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
  return {0, n, tonumber(oldest[2])}
end
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[3])
redis.call('PEXPIRE', KEYS[1], 61000)
return {1, n + 1, 0}`)

// Tokens used in the last minute: members are "<id>:<tokens>".
var tokenWindow = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, tonumber(ARGV[1]) - 60000)
local items = redis.call('ZRANGE', KEYS[1], 0, -1, 'WITHSCORES')
local used, oldest = 0, 0
for i = 1, #items, 2 do
  used = used + tonumber(string.match(items[i], ':(%d+)$'))
  if oldest == 0 then oldest = tonumber(items[i + 1]) end
end
return {used, oldest}`)

// Adds spend only to a counter that was already seeded from the database.
var addSpend = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return redis.call('INCRBYFLOAT', KEYS[1], ARGV[1]) end
return false`)

// AdmitRequest records a request against a per-minute limit. It returns
// whether it was admitted, how many requests are now in the window, and when
// the oldest request leaves the window.
func (s *Store) AdmitRequest(ctx context.Context, keyID string, limit int, now time.Time) (bool, int, time.Time, error) {
	res, err := requestWindow.Run(ctx, s.Redis, []string{"nexa:rl:req:" + keyID}, now.UnixMilli(), limit, uuid.NewString()).Int64Slice()
	if err != nil {
		return false, 0, time.Time{}, err
	}
	return res[0] == 1, int(res[1]), time.UnixMilli(res[2]), nil
}

// TokensLastMinute is the key's token use over the last minute and the time the oldest use expires.
func (s *Store) TokensLastMinute(ctx context.Context, keyID string, now time.Time) (int, time.Time, error) {
	res, err := tokenWindow.Run(ctx, s.Redis, []string{"nexa:rl:tok:" + keyID}, now.UnixMilli()).Int64Slice()
	if err != nil {
		return 0, time.Time{}, err
	}
	return int(res[0]), time.UnixMilli(res[1]), nil
}

func spendKey(keyID string, now time.Time) string {
	return "nexa:spend:" + keyID + ":" + MonthStart(now).Format("2006-01")
}

// MonthSpend is the key's spend this month. The shared counter is seeded from
// the traces once per month and then kept up to date by RecordKeyUsage.
func (s *Store) MonthSpend(ctx context.Context, keyID string, now time.Time) (float64, error) {
	k := spendKey(keyID, now)
	v, err := s.Redis.Get(ctx, k).Float64()
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, redis.Nil) {
		return 0, err
	}
	seed := s.KeySpend(keyID, MonthStart(now))
	if err := s.Redis.SetNX(ctx, k, strconv.FormatFloat(seed, 'f', -1, 64), 40*24*time.Hour).Err(); err != nil {
		return 0, err
	}
	return s.Redis.Get(ctx, k).Float64()
}

// RecordKeyUsage adds a finished request's cost and tokens to the shared counters.
func (s *Store) RecordKeyUsage(ctx context.Context, keyID string, cost float64, tokens int, now time.Time) {
	if cost > 0 {
		if err := addSpend.Run(ctx, s.Redis, []string{spendKey(keyID, now)}, strconv.FormatFloat(cost, 'f', -1, 64)).Err(); err != nil && !errors.Is(err, redis.Nil) {
			slog.Warn("could not record key spend", "error", err.Error())
		}
	}
	if tokens > 0 {
		k := "nexa:rl:tok:" + keyID
		pipe := s.Redis.TxPipeline()
		pipe.ZAdd(ctx, k, redis.Z{Score: float64(now.UnixMilli()), Member: uuid.NewString() + ":" + strconv.Itoa(tokens)})
		pipe.PExpire(ctx, k, 61*time.Second)
		if _, err := pipe.Exec(ctx); err != nil {
			slog.Warn("could not record key tokens", "error", err.Error())
		}
	}
}

// ---------------------------------------------------------------- login throttling

const (
	loginFailures = 10
	loginWindow   = 10 * time.Minute
)

// LoginBlocked returns how long the address must wait, or 0.
func (s *Store) LoginBlocked(ctx context.Context, addr string) time.Duration {
	k := "nexa:login:" + addr
	n, err := s.Redis.Get(ctx, k).Int()
	if err != nil || n < loginFailures {
		return 0
	}
	ttl, _ := s.Redis.TTL(ctx, k).Result()
	return max(ttl, time.Second)
}

func (s *Store) LoginFailed(ctx context.Context, addr string) {
	k := "nexa:login:" + addr
	pipe := s.Redis.TxPipeline()
	pipe.Incr(ctx, k)
	pipe.ExpireNX(ctx, k, loginWindow)
	_, _ = pipe.Exec(ctx)
}

func (s *Store) LoginSucceeded(ctx context.Context, addr string) {
	s.Redis.Del(ctx, "nexa:login:"+addr)
}

// ---------------------------------------------------------------- shared small caches

// CacheJSON stores v under key for ttl (used for Jev routing decisions).
func (s *Store) CacheJSON(ctx context.Context, key string, v any, ttl time.Duration) {
	b, _ := json.Marshal(v)
	s.Redis.Set(ctx, "nexa:"+key, b, ttl)
}

// CachedJSON loads a value stored with CacheJSON.
func (s *Store) CachedJSON(ctx context.Context, key string, v any) bool {
	b, err := s.Redis.Get(ctx, "nexa:"+key).Bytes()
	return err == nil && json.Unmarshal(b, v) == nil
}

// scanCount counts keys matching pattern.
// ponytail: SCAN is fine for dashboard counts up to ~1M keys; keep counters if it grows beyond.
func (s *Store) scanCount(ctx context.Context, pattern string) int {
	n := 0
	iter := s.Redis.Scan(ctx, 0, pattern, 1000).Iterator()
	for iter.Next(ctx) {
		n++
	}
	return n
}

func (s *Store) scanDelete(ctx context.Context, pattern string) (int64, error) {
	var n int64
	iter := s.Redis.Scan(ctx, 0, pattern, 1000).Iterator()
	batch := []string{}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		d, err := s.Redis.Unlink(ctx, batch...).Result()
		n += d
		batch = batch[:0]
		return err
	}
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) == 500 {
			if err := flush(); err != nil {
				return n, err
			}
		}
	}
	if err := iter.Err(); err != nil {
		return n, err
	}
	return n, flush()
}
