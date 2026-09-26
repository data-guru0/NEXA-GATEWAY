package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// CacheSettings configures response caching for /v1/chat/completions.
type CacheSettings struct {
	ExactEnabled        bool    `json:"exact_enabled"`
	SemanticEnabled     bool    `json:"semantic_enabled"`
	TTLSeconds          int     `json:"ttl_seconds"`
	Threshold           float64 `json:"threshold"` // cosine similarity needed for a semantic hit
	EmbeddingProviderID string  `json:"embedding_provider_id"`
	EmbeddingModel      string  `json:"embedding_model"`
}

// CacheEntry is one stored answer.
type CacheEntry struct {
	ID            string         `json:"id"`
	Partition     string         `json:"partition"`
	Model         string         `json:"model"`
	Response      CachedResponse `json:"response"`
	SourceTraceID string         `json:"source_trace_id"`
	CostUSD       float64        `json:"cost_usd"`
	CreatedAt     time.Time      `json:"created_at"`
}

// CachedResponse is the part of a completion needed to answer again.
type CachedResponse struct {
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	Content      string `json:"content"`
	FinishReason string `json:"finish_reason"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

func normalizeCacheSettings(c *CacheSettings) error {
	if c.TTLSeconds == 0 {
		c.TTLSeconds = 86400
	}
	if c.TTLSeconds < 60 || c.TTLSeconds > 90*86400 {
		return errors.New("cache lifetime must be between 1 minute and 90 days")
	}
	if c.Threshold == 0 {
		c.Threshold = 0.95
	}
	if c.Threshold < 0.8 || c.Threshold > 0.999 {
		return errors.New("similarity threshold must be between 0.80 and 0.999")
	}
	c.EmbeddingModel = strings.TrimSpace(c.EmbeddingModel)
	if c.SemanticEnabled && (c.EmbeddingProviderID == "" || c.EmbeddingModel == "") {
		return errors.New("semantic caching needs an embedding provider and model")
	}
	return nil
}

// CacheSettings returns the saved settings (caching is off until enabled).
func (s *Store) CacheSettings() CacheSettings {
	var raw string
	c := CacheSettings{}
	if s.DB.QueryRow("SELECT value FROM settings WHERE key='cache'").Scan(&raw) == nil {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	if c.TTLSeconds == 0 {
		c.TTLSeconds = 86400
	}
	if c.Threshold == 0 {
		c.Threshold = 0.95
	}
	return c
}

func (s *Store) SetCacheSettings(c CacheSettings) (CacheSettings, error) {
	if err := normalizeCacheSettings(&c); err != nil {
		return c, err
	}
	if c.SemanticEnabled {
		if _, err := s.Provider(c.EmbeddingProviderID); err != nil {
			return c, errors.New("unknown embedding provider")
		}
	}
	b, _ := json.Marshal(c)
	if _, err := s.DB.Exec("INSERT INTO settings(key,value) VALUES('cache',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(b)); err != nil {
		return c, err
	}
	s.Publish("cache-settings")
	return c, nil
}

const (
	exactPrefix = "nexa:cache:exact:"
	semPrefix   = "nexa:cache:sem:" // + <dimensions>:<id>
)

// semantic entries are Redis hashes indexed by RediSearch: a TAG for the
// partition (same model and parameters) and an HNSW cosine index on the vector.
// One index per embedding size, created on first use.
var semIndexes sync.Map

func semIndexName(dim int) string { return "nexa:cache:idx:" + strconv.Itoa(dim) }

func (s *Store) ensureSemIndex(ctx context.Context, dim int) error {
	if _, ok := semIndexes.Load(dim); ok {
		return nil
	}
	err := s.Redis.Do(ctx, "FT.CREATE", semIndexName(dim), "ON", "HASH", "PREFIX", "1", fmt.Sprintf("%s%d:", semPrefix, dim),
		"SCHEMA", "partition", "TAG", "vec", "VECTOR", "HNSW", "6", "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE").Err()
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	semIndexes.Store(dim, true)
	return nil
}

func vectorBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

// PutCacheEntry stores an answer under its exact key and, with a vector, in the semantic index.
func (s *Store) PutCacheEntry(ctx context.Context, e CacheEntry, exactKey string, vec []float32, ttl time.Duration) error {
	body, _ := json.Marshal(e)
	pipe := s.Redis.TxPipeline()
	pipe.Set(ctx, exactPrefix+exactKey, body, ttl)
	if len(vec) > 0 {
		if err := s.ensureSemIndex(ctx, len(vec)); err != nil {
			return err
		}
		k := fmt.Sprintf("%s%d:%s", semPrefix, len(vec), e.ID)
		pipe.HSet(ctx, k, "partition", e.Partition, "vec", vectorBytes(vec), "entry", body)
		pipe.Expire(ctx, k, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// CacheByKey returns a live entry by its exact-request key.
func (s *Store) CacheByKey(ctx context.Context, exactKey string) (CacheEntry, error) {
	var e CacheEntry
	b, err := s.Redis.Get(ctx, exactPrefix+exactKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return e, sql.ErrNoRows
		}
		return e, err
	}
	return e, json.Unmarshal(b, &e)
}

// CacheNearest returns the most similar live entry in the partition and its cosine similarity.
func (s *Store) CacheNearest(ctx context.Context, partition string, vec []float32) (CacheEntry, float64, error) {
	var e CacheEntry
	if _, ok := semIndexes.Load(len(vec)); !ok {
		if err := s.ensureSemIndex(ctx, len(vec)); err != nil {
			return e, 0, err
		}
	}
	res, err := s.Redis.Do(ctx, "FT.SEARCH", semIndexName(len(vec)), "(@partition:{"+partition+"})=>[KNN 1 @vec $B AS dist]",
		"PARAMS", "2", "B", vectorBytes(vec), "SORTBY", "dist", "RETURN", "2", "dist", "entry", "LIMIT", "0", "1", "DIALECT", "2").Slice()
	if err != nil {
		return e, 0, err
	}
	if len(res) < 3 {
		return e, 0, sql.ErrNoRows
	}
	fields, _ := res[2].([]any)
	var dist float64 = 2
	for i := 0; i+1 < len(fields); i += 2 {
		name, _ := fields[i].(string)
		value, _ := fields[i+1].(string)
		switch name {
		case "dist":
			dist, _ = strconv.ParseFloat(value, 64)
		case "entry":
			if err := json.Unmarshal([]byte(value), &e); err != nil {
				return e, 0, err
			}
		}
	}
	if e.ID == "" {
		return e, 0, sql.ErrNoRows
	}
	return e, 1 - dist, nil // RediSearch COSINE distance is 1 - similarity
}

// ClearCache removes every cached answer (the indexes stay and empty out).
func (s *Store) ClearCache(ctx context.Context) (int64, error) {
	n, err := s.scanDelete(ctx, exactPrefix+"*")
	if err != nil {
		return n, err
	}
	_, err = s.scanDelete(ctx, semPrefix+"*")
	return n, err
}

// CacheStats summarises the cache and how often it answered.
type CacheStats struct {
	Entries         int     `json:"entries"`
	SemanticEntries int     `json:"semantic_entries"`
	Requests        int     `json:"requests"`
	ExactHits       int     `json:"exact_hits"`
	SemanticHits    int     `json:"semantic_hits"`
	HitRate         float64 `json:"hit_rate"`
	SavedUSD        float64 `json:"saved_usd"`
	AvgHitMS        float64 `json:"avg_hit_ms"`
	AvgMissMS       float64 `json:"avg_miss_ms"`
}

// CacheStats counts live entries in Redis and, from the traces, the chat requests
// that were eligible for caching since `since`.
func (s *Store) CacheStats(ctx context.Context, since time.Time) CacheStats {
	st := CacheStats{Entries: s.scanCount(ctx, exactPrefix+"*"), SemanticEntries: s.scanCount(ctx, semPrefix+"*")}
	var saved, hitMS, missMS sql.NullFloat64
	_ = s.DB.QueryRow(`SELECT COUNT(*),
 COUNT(*) FILTER (WHERE metadata->'cache'->>'result'='exact'),
 COUNT(*) FILTER (WHERE metadata->'cache'->>'result'='semantic'),
 SUM((metadata->'cache'->>'saved_usd')::float8) FILTER (WHERE metadata->'cache'->>'result' IN ('exact','semantic')),
 AVG(latency_ms) FILTER (WHERE metadata->'cache'->>'result' IN ('exact','semantic')),
 AVG(latency_ms) FILTER (WHERE metadata->'cache'->>'result'='miss' AND status='success')
 FROM traces WHERE created_at>=? AND metadata->'cache'->>'result' IS NOT NULL`, since.UTC()).
		Scan(&st.Requests, &st.ExactHits, &st.SemanticHits, &saved, &hitMS, &missMS)
	st.SavedUSD, st.AvgHitMS, st.AvgMissMS = saved.Float64, hitMS.Float64, missMS.Float64
	if st.Requests > 0 {
		st.HitRate = float64(st.ExactHits+st.SemanticHits) / float64(st.Requests)
	}
	return st
}
