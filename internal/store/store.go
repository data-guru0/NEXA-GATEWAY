package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var ErrProviderInUse = errors.New("provider is used by a routing profile")

type Store struct {
	DB   *sql.DB
	aead cipher.AEAD

	// In-memory caches for data read on every gateway request. Each is nil until
	// first use and reset to nil by any write, so the next read reloads it.
	mu        sync.RWMutex
	providers map[string]Provider // keyed by id and by lower-case slug
	profiles  []RoutingProfile    // decrypted Jev keys included
	prices    map[string]Price
	apiKeys   map[string]APIKey // keyed by SHA-256 of the key
	masterSHA []byte
	keyUsed   map[string]time.Time

	feedbackVersion atomic.Int64
}

type Provider struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Slug        string            `json:"slug"`
	Type        string            `json:"type"`
	BaseURL     string            `json:"base_url"`
	APIKey      string            `json:"-"`
	MaskedKey   string            `json:"masked_key"`
	Headers     map[string]string `json:"-"`
	HeaderNames []string          `json:"header_names"`
	Enabled     bool              `json:"enabled"`
	CreatedAt   time.Time         `json:"created_at"`
}

type Trace struct {
	ID              string    `json:"id"`
	RequestID       string    `json:"request_id"`
	Source          string    `json:"source"`
	APIKeyName      string    `json:"api_key_name"`
	ProviderID      string    `json:"provider_id"`
	ProviderName    string    `json:"provider_name"`
	Model           string    `json:"model"`
	Status          string    `json:"status"`
	StatusCode      int       `json:"status_code"`
	Stream          bool      `json:"stream"`
	Prompt          string    `json:"prompt"`
	Request         string    `json:"request"`
	Params          string    `json:"params"`
	Response        string    `json:"response"`
	FinishReason    string    `json:"finish_reason"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	TotalTokens     int       `json:"total_tokens"`
	CachedTokens    int       `json:"cached_tokens"`
	ReasoningTokens int       `json:"reasoning_tokens"`
	LatencyMS       int64     `json:"latency_ms"`
	TTFTMS          float64   `json:"ttft_ms"`
	UpstreamMS      float64   `json:"upstream_latency_ms"`
	GatewayMS       float64   `json:"gateway_latency_ms"`
	CostUSD         float64   `json:"cost_usd"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	Metadata        string    `json:"metadata,omitempty"`
}

type Stats struct {
	Requests24H   int64            `json:"requests_24h"`
	Requests      int64            `json:"requests"`
	SuccessRate   float64          `json:"success_rate"`
	Tokens24H     int64            `json:"tokens_24h"`
	Cost24H       float64          `json:"cost_24h"`
	AvgLatencyMS  float64          `json:"avg_latency_ms"`
	P50LatencyMS  float64          `json:"p50_latency_ms"`
	P95LatencyMS  float64          `json:"p95_latency_ms"`
	AvgUpstreamMS float64          `json:"avg_upstream_latency_ms"`
	AvgGatewayMS  float64          `json:"avg_gateway_latency_ms"`
	Providers     int64            `json:"providers"`
	Hourly        []Point          `json:"hourly"`
	Previous      StatsPrevious    `json:"previous"`
	ByProvider    []StatsBreakdown `json:"by_provider"`
	ByModel       []StatsBreakdown `json:"by_model"`
	Routing       []RoutingUsage   `json:"routing"`
	SmartRequests int64            `json:"smart_requests"`
	Range         string           `json:"range"`
	From          time.Time        `json:"from"`
	To            time.Time        `json:"to"`
}

type StatsPrevious struct {
	Requests      int64   `json:"requests"`
	SuccessRate   float64 `json:"success_rate"`
	Tokens        int64   `json:"tokens"`
	Cost          float64 `json:"cost"`
	AvgUpstreamMS float64 `json:"avg_upstream_latency_ms"`
	AvgGatewayMS  float64 `json:"avg_gateway_latency_ms"`
}

type StatsBreakdown struct {
	ProviderID   string  `json:"provider_id,omitempty"`
	ProviderName string  `json:"provider_name,omitempty"`
	Model        string  `json:"model,omitempty"`
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	Cost         float64 `json:"cost"`
	P95LatencyMS float64 `json:"p95_latency_ms"`
}

type RoutingUsage struct {
	ProfileSlug string `json:"profile_slug"`
	Lane        string `json:"lane"`
	Requests    int64  `json:"requests"`
}

type Point struct {
	Hour       string    `json:"hour"`
	Timestamp  time.Time `json:"timestamp"`
	Requests   int64     `json:"requests"`
	Errors     int64     `json:"errors"`
	Tokens     int64     `json:"tokens"`
	Cost       float64   `json:"cost"`
	UpstreamMS float64   `json:"upstream_latency_ms"`
	GatewayMS  float64   `json:"gateway_latency_ms"`
}

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionInfo is the authenticated dashboard principal behind a session cookie.
type SessionInfo struct {
	UserID   string
	Username string
	Role     string
	Master   bool
}

type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	hash       string
}

// Price is a dashboard-managed USD price per million tokens. A model ending in
// "*" matches every model with that prefix.
type Price struct {
	Model  string  `json:"model"`
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// RoutingProfile is a dashboard-managed virtual model routed by Jev difficulty.
type RoutingProfile struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Slug                string          `json:"slug"`
	ConfidenceThreshold float64         `json:"confidence_threshold"`
	FallbackLane        string          `json:"fallback_lane"`
	MaxRetries          int             `json:"max_retries"`
	RequestTimeoutMS    int             `json:"request_timeout_ms"`
	Active              bool            `json:"active"`
	JevAPIKey           string          `json:"jev_api_key,omitempty"`
	JevKeyConfigured    bool            `json:"jev_key_configured"`
	Targets             []RoutingTarget `json:"targets"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

type RoutingTarget struct {
	ID                   string   `json:"id"`
	ProviderID           string   `json:"provider_id"`
	Model                string   `json:"model"`
	Tier                 string   `json:"tier"`
	Description          string   `json:"description"`
	Unsupported          []string `json:"unsupported"` // request capabilities (tools, vision, json) this model cannot serve
	ContextWindow        int      `json:"context_window"`
	MaxOutputTokens      int      `json:"max_output_tokens"`
	InputCostPerMillion  float64  `json:"input_cost_per_million"`
	OutputCostPerMillion float64  `json:"output_cost_per_million"`
	Priority             int      `json:"priority"`
	Enabled              bool     `json:"enabled"`
}

func Open(dataDir string) (*Store, string, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, "", err
	}
	key, err := loadOrCreateKey(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		return nil, "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "nexa.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, "", err
	}
	// WAL lets readers run beside the single writer; busy_timeout serializes writes.
	db.SetMaxOpenConns(8)
	s := &Store{DB: db, aead: aead, keyUsed: map[string]time.Time{}}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, "", err
	}
	masterKey, err := s.ensureMasterKey()
	return s, masterKey, err
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != 32 {
			return nil, errors.New("invalid secret.key length")
		}
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Store) migrate() error {
	_, err := s.DB.Exec(`
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE, password_hash TEXT NOT NULL,
 role TEXT NOT NULL DEFAULT 'member', created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS sessions (
 id TEXT PRIMARY KEY, user_id TEXT, is_master INTEGER NOT NULL DEFAULT 0,
 expires_at DATETIME NOT NULL, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS providers (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, slug TEXT NOT NULL UNIQUE COLLATE NOCASE,
 type TEXT NOT NULL, base_url TEXT NOT NULL, api_key TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS traces (
 id TEXT PRIMARY KEY, provider_id TEXT, provider_name TEXT NOT NULL, model TEXT NOT NULL,
 status TEXT NOT NULL, status_code INTEGER NOT NULL, prompt TEXT NOT NULL DEFAULT '',
 response TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0,
 output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
 latency_ms INTEGER NOT NULL DEFAULT 0, upstream_latency_ms REAL NOT NULL DEFAULT 0,
 gateway_latency_ms REAL NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', metadata TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS routing_profiles (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, slug TEXT NOT NULL UNIQUE COLLATE NOCASE,
 engine TEXT NOT NULL, objective TEXT NOT NULL, config_json TEXT NOT NULL,
 jev_api_key TEXT NOT NULL DEFAULT '', active INTEGER NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS routing_attempts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, provider_id TEXT NOT NULL, model TEXT NOT NULL,
 status TEXT NOT NULL, latency_ms REAL NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS api_keys (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL,
 created_at DATETIME NOT NULL, last_used_at DATETIME
);
CREATE TABLE IF NOT EXISTS model_prices (
 model TEXT PRIMARY KEY COLLATE NOCASE, input REAL NOT NULL, output REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_traces_created ON traces(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_traces_provider ON traces(provider_id);
CREATE INDEX IF NOT EXISTS idx_traces_route ON traces(provider_id,model,created_at);
CREATE INDEX IF NOT EXISTS idx_routing_active ON routing_profiles(active);
CREATE INDEX IF NOT EXISTS idx_routing_attempt_health ON routing_attempts(provider_id,model,created_at DESC);
`)
	if err != nil {
		return err
	}
	for _, c := range [][3]string{
		{"traces", "upstream_latency_ms", "REAL NOT NULL DEFAULT 0"},
		{"traces", "gateway_latency_ms", "REAL NOT NULL DEFAULT 0"},
		{"traces", "request_id", "TEXT NOT NULL DEFAULT ''"},
		{"traces", "source", "TEXT NOT NULL DEFAULT ''"},
		{"traces", "api_key_name", "TEXT NOT NULL DEFAULT ''"},
		{"traces", "stream", "INTEGER NOT NULL DEFAULT 0"},
		{"traces", "ttft_ms", "REAL NOT NULL DEFAULT 0"},
		{"traces", "finish_reason", "TEXT NOT NULL DEFAULT ''"},
		{"traces", "params", "TEXT NOT NULL DEFAULT ''"},
		{"traces", "request", "TEXT NOT NULL DEFAULT ''"},
		{"traces", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"traces", "reasoning_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "headers", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn(c[0], c[1], c[2]); err != nil {
			return err
		}
	}
	// Session ids are now stored as SHA-256 hex (64 chars); older raw ids can never match.
	if _, err = s.DB.Exec("DELETE FROM sessions WHERE length(id)!=64 OR expires_at<?", time.Now().UTC()); err != nil {
		return err
	}
	// The bundled local classifier was removed; existing profiles move to Jev.
	if _, err = s.DB.Exec("UPDATE routing_profiles SET engine='jev' WHERE engine!='jev'"); err != nil {
		return err
	}
	if err = s.migrateFeedback(); err != nil {
		return err
	}
	_, err = s.DB.Exec("DELETE FROM routing_attempts WHERE created_at<? OR provider_id NOT IN (SELECT id FROM providers)", time.Now().UTC().Add(-24*time.Hour))
	return err
}

func (s *Store) ensureColumn(table, column, definition string) error {
	rows, err := s.DB.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	_, err = s.DB.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}

// Prune applies trace retention (0 keeps traces forever) and clears expired housekeeping rows.
func (s *Store) Prune(retention time.Duration) error {
	now := time.Now().UTC()
	if retention > 0 {
		if _, err := s.DB.Exec("DELETE FROM traces WHERE created_at<?", now.Add(-retention)); err != nil {
			return err
		}
	}
	if _, err := s.DB.Exec("DELETE FROM routing_attempts WHERE created_at<?", now.Add(-24*time.Hour)); err != nil {
		return err
	}
	_, err := s.DB.Exec("DELETE FROM sessions WHERE expires_at<?", now)
	return err
}

func sha(v string) []byte    { h := sha256.Sum256([]byte(v)); return h[:] }
func shaHex(v string) string { return hex.EncodeToString(sha(v)) }

func randomToken(prefix string, n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// The master key is 192 random bits, so a SHA-256 digest is as safe as bcrypt
// for it and costs microseconds instead of ~70 ms on every API request. The
// bcrypt hash is kept for databases created before the digest existed.
func (s *Store) ensureMasterKey() (string, error) {
	var existing string
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='master_key_hash'").Scan(&existing); err == nil {
		var digest string
		if s.DB.QueryRow("SELECT value FROM settings WHERE key='master_key_sha256'").Scan(&digest) == nil {
			if b, e := hex.DecodeString(digest); e == nil {
				s.masterSHA = b
			}
		}
		return "", nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	master, err := randomToken("nexa_", 24)
	if err != nil {
		return "", err
	}
	return master, s.saveMaster(master)
}

func (s *Store) saveMaster(master string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(master), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]string{"master_key_hash": string(hash), "master_key_sha256": shaHex(master)} {
		if _, err := tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.masterSHA = sha(master)
	s.mu.Unlock()
	return nil
}

func (s *Store) ValidateMaster(key string) bool {
	if key == "" {
		return false
	}
	s.mu.RLock()
	digest := s.masterSHA
	s.mu.RUnlock()
	if digest != nil {
		return subtle.ConstantTimeCompare(sha(key), digest) == 1
	}
	var hash string
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='master_key_hash'").Scan(&hash); err != nil {
		return false
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(key)) != nil {
		return false
	}
	if _, err := s.DB.Exec("INSERT INTO settings(key,value) VALUES('master_key_sha256',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", shaHex(key)); err == nil {
		s.mu.Lock()
		s.masterSHA = sha(key)
		s.mu.Unlock()
	}
	return true
}

// ResetMasterKey issues a new master key and signs out every master session.
func (s *Store) ResetMasterKey() (string, error) {
	master, err := randomToken("nexa_", 24)
	if err != nil {
		return "", err
	}
	if err := s.saveMaster(master); err != nil {
		return "", err
	}
	_, err = s.DB.Exec("DELETE FROM sessions WHERE is_master=1")
	return master, err
}

func (s *Store) encrypt(value string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(value), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *Store) decrypt(value string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(b) < s.aead.NonceSize() {
		return "", errors.New("invalid encrypted secret")
	}
	plain, err := s.aead.Open(nil, b[:s.aead.NonceSize()], b[s.aead.NonceSize():], nil)
	return string(plain), err
}

func MaskKey(key string) string {
	if len(key) <= 8 {
		return "••••••••"
	}
	return key[:4] + "••••••••" + key[len(key)-4:]
}

func (s *Store) invalidate() {
	s.mu.Lock()
	s.providers, s.profiles = nil, nil
	s.mu.Unlock()
}

func validProviderType(t string) bool {
	switch t {
	case "openai", "groq", "gemini", "anthropic", "custom", "compatible":
		return true
	}
	return false
}

func validateProvider(p Provider) error {
	if strings.TrimSpace(p.Name) == "" || p.Slug == "" {
		return errors.New("name and slug are required")
	}
	if !validProviderType(p.Type) {
		return errors.New("unsupported provider type")
	}
	if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
		return errors.New("base URL must start with http:// or https://")
	}
	return nil
}

func (s *Store) encryptHeaders(h map[string]string) (string, error) {
	if len(h) == 0 {
		return "", nil
	}
	b, _ := json.Marshal(h)
	return s.encrypt(string(b))
}

func (s *Store) CreateProvider(p Provider) (Provider, error) {
	p.ID = uuid.NewString()
	p.Name = strings.TrimSpace(p.Name)
	p.Slug = slugify(p.Slug)
	if p.Slug == "" {
		p.Slug = slugify(p.Name)
	}
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if err := validateProvider(p); err != nil {
		return p, err
	}
	if p.APIKey == "" {
		return p, errors.New("API key is required")
	}
	enc, err := s.encrypt(p.APIKey)
	if err != nil {
		return p, err
	}
	headers, err := s.encryptHeaders(p.Headers)
	if err != nil {
		return p, err
	}
	p.CreatedAt = time.Now().UTC()
	_, err = s.DB.Exec("INSERT INTO providers(id,name,slug,type,base_url,api_key,headers,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
		p.ID, p.Name, p.Slug, p.Type, p.BaseURL, enc, headers, p.Enabled, p.CreatedAt)
	if err != nil {
		return p, err
	}
	s.invalidate()
	return s.Provider(p.ID)
}

// UpdateProvider keeps the stored API key when p.APIKey is empty and the stored
// extra headers unless setHeaders is true.
func (s *Store) UpdateProvider(p Provider, setHeaders bool) (Provider, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.Slug = slugify(p.Slug)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if err := validateProvider(p); err != nil {
		return p, err
	}
	q := "UPDATE providers SET name=?,slug=?,type=?,base_url=?,enabled=?"
	args := []any{p.Name, p.Slug, p.Type, p.BaseURL, p.Enabled}
	if p.APIKey != "" {
		enc, err := s.encrypt(p.APIKey)
		if err != nil {
			return p, err
		}
		q += ",api_key=?"
		args = append(args, enc)
	}
	if setHeaders {
		headers, err := s.encryptHeaders(p.Headers)
		if err != nil {
			return p, err
		}
		q += ",headers=?"
		args = append(args, headers)
	}
	res, err := s.DB.Exec(q+" WHERE id=?", append(args, p.ID)...)
	if err != nil {
		return p, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return p, sql.ErrNoRows
	}
	s.invalidate()
	return s.Provider(p.ID)
}

func (s *Store) loadProviders() (map[string]Provider, error) {
	s.mu.RLock()
	cached := s.providers
	s.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}
	rows, err := s.DB.Query("SELECT id,name,slug,type,base_url,api_key,headers,enabled,created_at FROM providers")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Provider{}
	for rows.Next() {
		var p Provider
		var enc, headers string
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Type, &p.BaseURL, &enc, &headers, &p.Enabled, &p.CreatedAt); err != nil {
			return nil, err
		}
		if p.APIKey, err = s.decrypt(enc); err != nil {
			return nil, err
		}
		p.MaskedKey = MaskKey(p.APIKey)
		p.HeaderNames = []string{}
		if headers != "" {
			plain, err := s.decrypt(headers)
			if err != nil {
				return nil, err
			}
			_ = json.Unmarshal([]byte(plain), &p.Headers)
			for name := range p.Headers {
				p.HeaderNames = append(p.HeaderNames, name)
			}
			sort.Strings(p.HeaderNames)
		}
		out[p.ID] = p
		out[strings.ToLower(p.Slug)] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.providers = out
	s.mu.Unlock()
	return out, nil
}

func (s *Store) Provider(idOrSlug string) (Provider, error) {
	all, err := s.loadProviders()
	if err != nil {
		return Provider{}, err
	}
	if p, ok := all[idOrSlug]; ok {
		return p, nil
	}
	if p, ok := all[strings.ToLower(idOrSlug)]; ok {
		return p, nil
	}
	return Provider{}, sql.ErrNoRows
}

func (s *Store) Providers() ([]Provider, error) {
	all, err := s.loadProviders()
	if err != nil {
		return nil, err
	}
	result := []Provider{}
	for key, p := range all {
		if key == p.ID {
			result = append(result, p)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

func (s *Store) DeleteProvider(id string) error {
	profiles, err := s.loadProfiles()
	if err != nil {
		return err
	}
	using := []string{}
	for _, p := range profiles {
		for _, t := range p.Targets {
			if t.ProviderID == id {
				using = append(using, p.Name)
				break
			}
		}
	}
	using = append(using, s.feedbackLoopsUsing(id)...)
	if len(using) > 0 {
		return fmt.Errorf("%w: remove it from %s first", ErrProviderInUse, strings.Join(using, ", "))
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM routing_attempts WHERE provider_id=?", id); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM providers WHERE id=?", id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func normalizeRoutingProfile(p *RoutingProfile) error {
	p.Name = strings.TrimSpace(p.Name)
	p.Slug = slugify(p.Slug)
	if p.Slug == "" {
		p.Slug = slugify(p.Name)
	}
	if p.Name == "" || p.Slug == "" {
		return errors.New("profile name and slug are required")
	}
	if len(p.Targets) == 0 {
		return errors.New("add at least one routing target")
	}
	if p.ConfidenceThreshold < 0 || p.ConfidenceThreshold > 1 {
		return errors.New("confidence threshold must be between 0 and 1")
	}
	if p.FallbackLane != "low" && p.FallbackLane != "high" {
		p.FallbackLane = "medium"
	}
	p.MaxRetries = min(max(p.MaxRetries, 0), 5)
	if p.RequestTimeoutMS < 1000 {
		p.RequestTimeoutMS = 120000
	}
	p.RequestTimeoutMS = min(p.RequestTimeoutMS, 600000)
	seen := map[string]bool{}
	enabledTargets := 0
	for i := range p.Targets {
		t := &p.Targets[i]
		if t.ID == "" {
			t.ID = uuid.NewString()
		}
		if t.ProviderID == "" || strings.TrimSpace(t.Model) == "" {
			return errors.New("every target needs a provider and model")
		}
		if seen[t.ID] {
			return errors.New("target IDs must be unique")
		}
		seen[t.ID] = true
		if t.Tier != "low" && t.Tier != "medium" && t.Tier != "high" {
			t.Tier = "medium"
		}
		unsupported := []string{}
		for _, c := range t.Unsupported {
			if c == "tools" || c == "vision" || c == "json" {
				unsupported = append(unsupported, c)
			}
		}
		t.Unsupported = unsupported
		if t.ContextWindow <= 0 {
			t.ContextWindow = 128000
		}
		if t.MaxOutputTokens <= 0 {
			t.MaxOutputTokens = 8192
		}
		t.InputCostPerMillion = max(t.InputCostPerMillion, 0)
		t.OutputCostPerMillion = max(t.OutputCostPerMillion, 0)
		if t.Enabled {
			enabledTargets++
		}
	}
	if enabledTargets == 0 {
		for i := range p.Targets {
			p.Targets[i].Enabled = true
		}
	}
	return nil
}

func jevKeyFromEnv() bool {
	return os.Getenv("JEV_API_KEY") != "" || os.Getenv("TYPESAFE_API_KEY") != ""
}

// saveRoutingProfile writes a normalized profile. The legacy engine and objective
// columns are NOT NULL, so they receive their only remaining values.
func (s *Store) saveRoutingProfile(p RoutingProfile, insert bool) error {
	stored := p
	stored.JevAPIKey = ""
	config, _ := json.Marshal(stored)
	key := ""
	if p.JevAPIKey != "" {
		enc, err := s.encrypt(p.JevAPIKey)
		if err != nil {
			return err
		}
		key = enc
	}
	var err error
	if insert {
		_, err = s.DB.Exec("INSERT INTO routing_profiles(id,name,slug,engine,objective,config_json,jev_api_key,active,created_at,updated_at) VALUES(?,?,?,'jev','balanced',?,?,?,?,?)", p.ID, p.Name, p.Slug, string(config), key, false, p.CreatedAt, p.UpdatedAt)
	} else if key != "" {
		_, err = s.DB.Exec("UPDATE routing_profiles SET name=?,slug=?,config_json=?,jev_api_key=?,updated_at=? WHERE id=?", p.Name, p.Slug, string(config), key, p.UpdatedAt, p.ID)
	} else {
		_, err = s.DB.Exec("UPDATE routing_profiles SET name=?,slug=?,config_json=?,updated_at=? WHERE id=?", p.Name, p.Slug, string(config), p.UpdatedAt, p.ID)
	}
	if err == nil {
		s.invalidate()
	}
	return err
}

func (s *Store) CreateRoutingProfile(p RoutingProfile) (RoutingProfile, error) {
	if err := normalizeRoutingProfile(&p); err != nil {
		return p, err
	}
	p.ID = uuid.NewString()
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	if err := s.saveRoutingProfile(p, true); err != nil {
		return p, err
	}
	p.JevKeyConfigured = p.JevAPIKey != "" || jevKeyFromEnv()
	p.JevAPIKey = ""
	return p, nil
}

func (s *Store) UpdateRoutingProfile(p RoutingProfile) (RoutingProfile, error) {
	current, err := s.RoutingProfile(p.ID)
	if err != nil {
		return p, err
	}
	if err = normalizeRoutingProfile(&p); err != nil {
		return p, err
	}
	p.Active, p.CreatedAt, p.UpdatedAt = current.Active, current.CreatedAt, time.Now().UTC()
	if err = s.saveRoutingProfile(p, false); err != nil {
		return p, err
	}
	return s.RoutingProfile(p.ID)
}

func (s *Store) loadProfiles() ([]RoutingProfile, error) {
	s.mu.RLock()
	cached := s.profiles
	s.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}
	rows, err := s.DB.Query("SELECT id,name,slug,config_json,jev_api_key,active,created_at,updated_at FROM routing_profiles ORDER BY active DESC,updated_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoutingProfile{}
	for rows.Next() {
		var p RoutingProfile
		var config, enc, id, name, slug string
		var active bool
		var created, updated time.Time
		if err := rows.Scan(&id, &name, &slug, &config, &enc, &active, &created, &updated); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(config), &p); err != nil {
			return nil, err
		}
		p.ID, p.Name, p.Slug, p.Active, p.CreatedAt, p.UpdatedAt = id, name, slug, active, created, updated
		if p.FallbackLane == "" {
			p.FallbackLane = "medium"
		}
		if enc != "" {
			if p.JevAPIKey, err = s.decrypt(enc); err != nil {
				return nil, err
			}
		}
		p.JevKeyConfigured = p.JevAPIKey != "" || jevKeyFromEnv()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.profiles = out
	s.mu.Unlock()
	return out, nil
}

// RoutingProfile and ActiveRoutingProfile include the decrypted Jev key for routing;
// RoutingProfiles strips it for API responses.
func (s *Store) RoutingProfile(idOrSlug string) (RoutingProfile, error) {
	all, err := s.loadProfiles()
	if err != nil {
		return RoutingProfile{}, err
	}
	for _, p := range all {
		if p.ID == idOrSlug || strings.EqualFold(p.Slug, idOrSlug) {
			return p, nil
		}
	}
	return RoutingProfile{}, sql.ErrNoRows
}
func (s *Store) ActiveRoutingProfile() (RoutingProfile, error) {
	all, err := s.loadProfiles()
	if err != nil {
		return RoutingProfile{}, err
	}
	for _, p := range all {
		if p.Active {
			return p, nil
		}
	}
	return RoutingProfile{}, sql.ErrNoRows
}
func (s *Store) RoutingProfiles() ([]RoutingProfile, error) {
	all, err := s.loadProfiles()
	if err != nil {
		return nil, err
	}
	out := make([]RoutingProfile, len(all))
	for i, p := range all {
		p.JevAPIKey = ""
		out[i] = p
	}
	return out, nil
}
func (s *Store) ActivateRoutingProfile(id string) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE routing_profiles SET active=0"); e != nil {
		return e
	}
	res, e := tx.Exec("UPDATE routing_profiles SET active=1,updated_at=? WHERE id=?", time.Now().UTC(), id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if e = tx.Commit(); e == nil {
		s.invalidate()
	}
	return e
}
func (s *Store) DeleteRoutingProfile(id string) error {
	_, e := s.DB.Exec("DELETE FROM routing_profiles WHERE id=?", id)
	if e == nil {
		s.invalidate()
	}
	return e
}

type RouteHealth struct {
	Requests     int
	Failures     int
	AvgLatencyMS float64
}

func (s *Store) RoutingHealth(providerID, model string) RouteHealth {
	var h RouteHealth
	since := time.Now().UTC().Add(-30 * time.Minute)
	_ = s.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN status!='success' THEN 1 ELSE 0 END),0),COALESCE(AVG(latency),0) FROM (
	 SELECT status,upstream_latency_ms AS latency FROM traces WHERE provider_id=? AND model=? AND created_at>=?
	 UNION ALL
	 SELECT status,latency_ms AS latency FROM routing_attempts WHERE provider_id=? AND model=? AND created_at>=?
	)`, providerID, model, since, providerID, model, since).Scan(&h.Requests, &h.Failures, &h.AvgLatencyMS)
	return h
}
func (s *Store) RecordRoutingFailure(providerID, model string, latencyMS float64) {
	_, _ = s.DB.Exec("INSERT INTO routing_attempts(provider_id,model,status,latency_ms,created_at) VALUES(?,?,?,?,?)", providerID, model, "error", latencyMS, time.Now().UTC())
}

func slugify(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	dash := false
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

const traceColumns = "id,request_id,source,api_key_name,provider_id,provider_name,model,status,status_code,stream,prompt,request,params,response,finish_reason,input_tokens,output_tokens,total_tokens,cached_tokens,reasoning_tokens,latency_ms,ttft_ms,upstream_latency_ms,gateway_latency_ms,cost_usd,error,metadata,created_at"

func (t *Trace) fields() []any {
	return []any{&t.ID, &t.RequestID, &t.Source, &t.APIKeyName, &t.ProviderID, &t.ProviderName, &t.Model, &t.Status, &t.StatusCode, &t.Stream, &t.Prompt, &t.Request, &t.Params, &t.Response, &t.FinishReason, &t.InputTokens, &t.OutputTokens, &t.TotalTokens, &t.CachedTokens, &t.ReasoningTokens, &t.LatencyMS, &t.TTFTMS, &t.UpstreamMS, &t.GatewayMS, &t.CostUSD, &t.Error, &t.Metadata, &t.CreatedAt}
}

func (s *Store) AddTrace(t Trace) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	values := []any{}
	for _, f := range t.fields() {
		values = append(values, reflectDeref(f))
	}
	_, err := s.DB.Exec("INSERT INTO traces("+traceColumns+") VALUES(?"+strings.Repeat(",?", len(values)-1)+")", values...)
	return err
}

// reflectDeref turns the scan pointers from fields() back into values for INSERT.
func reflectDeref(p any) any {
	switch v := p.(type) {
	case *string:
		return *v
	case *int:
		return *v
	case *int64:
		return *v
	case *float64:
		return *v
	case *bool:
		return *v
	case *time.Time:
		return *v
	}
	return nil
}

type TraceQuery struct {
	Limit, Offset     int
	Search, Status    string
	ProviderID, Model string
	From, To          time.Time
}

func (s *Store) Traces(f TraceQuery) ([]Trace, int, error) {
	if f.Limit < 1 || f.Limit > 200 {
		f.Limit = 50
	}
	q := " FROM traces WHERE 1=1"
	args := []any{}
	for _, term := range strings.Fields(strings.TrimSpace(f.Search)) {
		q += ` AND (LOWER(id) LIKE ? OR LOWER(provider_name) LIKE ? OR LOWER(model) LIKE ? OR LOWER(prompt) LIKE ? OR LOWER(response) LIKE ? OR LOWER(status) LIKE ? OR LOWER(COALESCE(error,'')) LIKE ?)`
		x := "%" + strings.ToLower(term) + "%"
		args = append(args, x, x, x, x, x, x, x)
	}
	if f.Status != "" && f.Status != "all" {
		q += " AND status=?"
		args = append(args, f.Status)
	}
	if f.ProviderID != "" {
		q += " AND provider_id=?"
		args = append(args, f.ProviderID)
	}
	if f.Model != "" {
		q += " AND LOWER(model) LIKE ?"
		args = append(args, "%"+strings.ToLower(f.Model)+"%")
	}
	if !f.From.IsZero() {
		q += " AND created_at>=?"
		args = append(args, f.From.UTC())
	}
	if !f.To.IsZero() {
		q += " AND created_at<=?"
		args = append(args, f.To.UTC())
	}
	var total int
	if err := s.DB.QueryRow("SELECT COUNT(*)"+q, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query("SELECT "+traceColumns+q+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, f.Limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Trace{}
	for rows.Next() {
		var t Trace
		if err := rows.Scan(t.fields()...); err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

func (s *Store) Trace(id string) (Trace, error) {
	var t Trace
	err := s.DB.QueryRow("SELECT "+traceColumns+" FROM traces WHERE id=?", id).Scan(t.fields()...)
	return t, err
}

func (s *Store) Stats(from, to time.Time, bucket time.Duration, rangeName string) (Stats, error) {
	x := Stats{SuccessRate: 100, Range: rangeName, From: from, To: to, Hourly: []Point{}, ByProvider: []StatsBreakdown{}, ByModel: []StatsBreakdown{}, Routing: []RoutingUsage{}}
	if bucket <= 0 {
		bucket = time.Hour
	}
	count := int(to.Sub(from)/bucket) + 1
	if count < 1 {
		count = 1
	}
	if count > 180 {
		count = 180
		bucket = to.Sub(from) / time.Duration(count)
	}
	for i := 0; i < count; i++ {
		at := from.Add(time.Duration(i) * bucket)
		x.Hourly = append(x.Hourly, Point{Hour: pointLabel(at, to.Sub(from)), Timestamp: at})
	}
	type totals struct {
		upstream, gateway float64
		count             int64
	}
	bucketTotals := make([]totals, len(x.Hourly))
	latencies := []int64{}
	type breakdownAccumulator struct {
		StatsBreakdown
		latencies []int64
	}
	providers := map[string]*breakdownAccumulator{}
	models := map[string]*breakdownAccumulator{}
	routing := map[string]*RoutingUsage{}
	rows, err := s.DB.Query(`SELECT COALESCE(provider_id,''),provider_name,model,status,total_tokens,cost_usd,latency_ms,upstream_latency_ms,gateway_latency_ms,metadata,created_at FROM traces WHERE created_at>=? AND created_at<=? ORDER BY created_at`, from.UTC(), to.UTC())
	if err != nil {
		return x, err
	}
	defer rows.Close()
	var successes int64
	for rows.Next() {
		var providerID, providerName, model, status, metadata string
		var tokens int64
		var cost float64
		var latency int64
		var upstream, gateway float64
		var created time.Time
		if err := rows.Scan(&providerID, &providerName, &model, &status, &tokens, &cost, &latency, &upstream, &gateway, &metadata, &created); err != nil {
			return x, err
		}
		x.Requests++
		x.Tokens24H += tokens
		x.Cost24H += cost
		x.AvgLatencyMS += float64(latency)
		x.AvgUpstreamMS += float64(upstream)
		x.AvgGatewayMS += float64(gateway)
		latencies = append(latencies, latency)
		if status == "success" {
			successes++
		}
		idx := int(created.Sub(from) / bucket)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(x.Hourly) {
			idx = len(x.Hourly) - 1
		}
		x.Hourly[idx].Requests++
		x.Hourly[idx].Tokens += tokens
		x.Hourly[idx].Cost += cost
		if status != "success" {
			x.Hourly[idx].Errors++
		}
		providerKey := providerID
		if providerKey == "" {
			providerKey = providerName
		}
		if providers[providerKey] == nil {
			providers[providerKey] = &breakdownAccumulator{StatsBreakdown: StatsBreakdown{ProviderID: providerID, ProviderName: providerName}}
		}
		provider := providers[providerKey]
		provider.Requests++
		provider.Cost += cost
		provider.latencies = append(provider.latencies, latency)
		if status != "success" {
			provider.Errors++
		}
		modelKey := providerKey + "\x00" + model
		if models[modelKey] == nil {
			models[modelKey] = &breakdownAccumulator{StatsBreakdown: StatsBreakdown{ProviderID: providerID, ProviderName: providerName, Model: model}}
		}
		modelStats := models[modelKey]
		modelStats.Requests++
		modelStats.Cost += cost
		modelStats.latencies = append(modelStats.latencies, latency)
		if status != "success" {
			modelStats.Errors++
		}
		var meta struct {
			SmartRouting bool   `json:"smart_routing"`
			ProfileSlug  string `json:"profile_slug"`
			Lane         string `json:"lane"`
			Signals      struct {
				Lane       string `json:"lane"`
				Complexity string `json:"complexity"`
			} `json:"signals"`
		}
		if json.Unmarshal([]byte(metadata), &meta) == nil && meta.SmartRouting {
			x.SmartRequests++
			lane := meta.Signals.Lane
			if lane == "" {
				lane = meta.Lane
			}
			if lane == "" {
				lane = meta.Signals.Complexity
			}
			key := meta.ProfileSlug + "\x00" + lane
			if routing[key] == nil {
				routing[key] = &RoutingUsage{ProfileSlug: meta.ProfileSlug, Lane: lane}
			}
			routing[key].Requests++
		}
		bucketTotals[idx].upstream += float64(upstream)
		bucketTotals[idx].gateway += float64(gateway)
		bucketTotals[idx].count++
	}
	if err := rows.Err(); err != nil {
		return x, err
	}
	x.Requests24H = x.Requests
	if x.Requests > 0 {
		x.SuccessRate = 100 * float64(successes) / float64(x.Requests)
		x.AvgLatencyMS /= float64(x.Requests)
		x.AvgUpstreamMS /= float64(x.Requests)
		x.AvgGatewayMS /= float64(x.Requests)
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		x.P50LatencyMS = float64(latencies[(len(latencies)-1)*50/100])
		x.P95LatencyMS = float64(latencies[(len(latencies)-1)*95/100])
	}
	for i := range x.Hourly {
		if bucketTotals[i].count > 0 {
			x.Hourly[i].UpstreamMS = bucketTotals[i].upstream / float64(bucketTotals[i].count)
			x.Hourly[i].GatewayMS = bucketTotals[i].gateway / float64(bucketTotals[i].count)
		}
	}
	finishBreakdown := func(values map[string]*breakdownAccumulator) []StatsBreakdown {
		out := make([]StatsBreakdown, 0, len(values))
		for _, value := range values {
			sort.Slice(value.latencies, func(i, j int) bool { return value.latencies[i] < value.latencies[j] })
			if len(value.latencies) > 0 {
				value.P95LatencyMS = float64(value.latencies[(len(value.latencies)-1)*95/100])
			}
			out = append(out, value.StatsBreakdown)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
		return out
	}
	x.ByProvider = finishBreakdown(providers)
	x.ByModel = finishBreakdown(models)
	for _, value := range routing {
		x.Routing = append(x.Routing, *value)
	}
	sort.Slice(x.Routing, func(i, j int) bool {
		if x.Routing[i].ProfileSlug == x.Routing[j].ProfileSlug {
			return x.Routing[i].Lane < x.Routing[j].Lane
		}
		return x.Routing[i].ProfileSlug < x.Routing[j].ProfileSlug
	})
	x.Previous = s.statsPrevious(from.Add(-to.Sub(from)), from)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM providers WHERE enabled=1").Scan(&x.Providers)
	return x, nil
}

func (s *Store) statsPrevious(from, to time.Time) StatsPrevious {
	var out StatsPrevious
	var successes int64
	_ = s.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COALESCE(AVG(upstream_latency_ms),0),COALESCE(AVG(gateway_latency_ms),0) FROM traces WHERE created_at>=? AND created_at<?`, from.UTC(), to.UTC()).Scan(&out.Requests, &successes, &out.Tokens, &out.Cost, &out.AvgUpstreamMS, &out.AvgGatewayMS)
	if out.Requests > 0 {
		out.SuccessRate = 100 * float64(successes) / float64(out.Requests)
	}
	return out
}

func pointLabel(at time.Time, span time.Duration) string {
	if span <= 24*time.Hour {
		return at.Local().Format("15:04")
	}
	if span <= 14*24*time.Hour {
		return at.Local().Format("Mon 15:04")
	}
	return at.Local().Format("02 Jan")
}

// Sessions store only the SHA-256 of the cookie token.
func (s *Store) CreateSession(userID string, master bool) (string, error) {
	token, err := randomToken("", 32)
	if err != nil {
		return "", err
	}
	var owner any = userID
	if master {
		owner = nil
	}
	now := time.Now().UTC()
	_, _ = s.DB.Exec("DELETE FROM sessions WHERE expires_at<?", now)
	_, err = s.DB.Exec("INSERT INTO sessions(id,user_id,is_master,expires_at,created_at) VALUES(?,?,?,?,?)", shaHex(token), owner, master, now.Add(7*24*time.Hour), now)
	return token, err
}
func (s *Store) Session(token string) (SessionInfo, error) {
	var info SessionInfo
	var uid sql.NullString
	err := s.DB.QueryRow(`SELECT s.user_id,s.is_master,COALESCE(u.username,''),COALESCE(u.role,'') FROM sessions s LEFT JOIN users u ON u.id=s.user_id WHERE s.id=? AND s.expires_at>?`, shaHex(token), time.Now().UTC()).
		Scan(&uid, &info.Master, &info.Username, &info.Role)
	info.UserID = uid.String
	if err == nil && info.Master {
		info.Username, info.Role = "Master administrator", "owner"
	}
	if err == nil && !info.Master && info.Role == "" {
		err = sql.ErrNoRows
	}
	return info, err
}
func (s *Store) DeleteSession(token string) {
	s.DB.Exec("DELETE FROM sessions WHERE id=?", shaHex(token))
}

// dummyHash keeps failed logins for unknown usernames as slow as wrong passwords.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("nexa-timing-equalizer"), bcrypt.DefaultCost)

func validRole(role string) bool { return role == "admin" || role == "member" }

func (s *Store) CreateUser(username, password, role string) (User, error) {
	var u User
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(password) < 8 {
		return u, errors.New("username must be 3+ characters and password 8+ characters")
	}
	if !validRole(role) {
		role = "member"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return u, err
	}
	u = User{ID: uuid.NewString(), Username: username, Role: role, CreatedAt: time.Now().UTC()}
	_, err = s.DB.Exec("INSERT INTO users(id,username,password_hash,role,created_at) VALUES(?,?,?,?,?)", u.ID, u.Username, string(hash), u.Role, u.CreatedAt)
	return u, err
}
func (s *Store) LoginUser(username, password string) (User, error) {
	var u User
	var hash string
	err := s.DB.QueryRow("SELECT id,username,password_hash,role,created_at FROM users WHERE username=?", strings.TrimSpace(username)).Scan(&u.ID, &u.Username, &hash, &u.Role, &u.CreatedAt)
	if err != nil {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return u, errors.New("invalid credentials")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return u, errors.New("invalid credentials")
	}
	return u, nil
}

// UpdateUser changes the role and/or password (empty = unchanged). A password
// change signs out all of the user's sessions except keepToken.
func (s *Store) UpdateUser(id, role, password, keepToken string) (User, error) {
	if role != "" {
		if !validRole(role) {
			return User{}, errors.New("role must be admin or member")
		}
		if _, err := s.DB.Exec("UPDATE users SET role=? WHERE id=?", role, id); err != nil {
			return User{}, err
		}
	}
	if password != "" {
		if len(password) < 8 {
			return User{}, errors.New("password must be 8+ characters")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return User{}, err
		}
		if _, err := s.DB.Exec("UPDATE users SET password_hash=? WHERE id=?", string(hash), id); err != nil {
			return User{}, err
		}
		if _, err := s.DB.Exec("DELETE FROM sessions WHERE user_id=? AND id!=?", id, shaHex(keepToken)); err != nil {
			return User{}, err
		}
	}
	return s.User(id)
}

// ChangePassword verifies the current password before replacing it.
func (s *Store) ChangePassword(id, current, next, keepToken string) error {
	var hash string
	if err := s.DB.QueryRow("SELECT password_hash FROM users WHERE id=?", id).Scan(&hash); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		return errors.New("current password is incorrect")
	}
	_, err := s.UpdateUser(id, "", next, keepToken)
	return err
}

func (s *Store) User(id string) (User, error) {
	var u User
	err := s.DB.QueryRow("SELECT id,username,role,created_at FROM users WHERE id=?", id).Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt)
	return u, err
}
func (s *Store) Users() ([]User, error) {
	rows, err := s.DB.Query("SELECT id,username,role,created_at FROM users ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
func (s *Store) DeleteUser(id string) error {
	_, err := s.DB.Exec("DELETE FROM users WHERE id=?", id)
	return err
}

// CreateAPIKey returns the plaintext key once; only its SHA-256 is stored.
func (s *Store) CreateAPIKey(name string) (APIKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return APIKey{}, "", errors.New("key name must be 1-60 characters")
	}
	plain, err := randomToken("nexa_sk_", 24)
	if err != nil {
		return APIKey{}, "", err
	}
	k := APIKey{ID: uuid.NewString(), Name: name, Prefix: plain[:14] + "…", CreatedAt: time.Now().UTC()}
	if _, err = s.DB.Exec("INSERT INTO api_keys(id,name,key_hash,prefix,created_at) VALUES(?,?,?,?,?)", k.ID, k.Name, shaHex(plain), k.Prefix, k.CreatedAt); err != nil {
		return APIKey{}, "", err
	}
	s.mu.Lock()
	s.apiKeys = nil
	s.mu.Unlock()
	return k, plain, nil
}

func (s *Store) APIKeys() ([]APIKey, error) {
	rows, err := s.DB.Query("SELECT id,name,key_hash,prefix,created_at,last_used_at FROM api_keys ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		var used sql.NullTime
		if err := rows.Scan(&k.ID, &k.Name, &k.hash, &k.Prefix, &k.CreatedAt, &used); err != nil {
			return nil, err
		}
		if used.Valid {
			k.LastUsedAt = &used.Time
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIKey(id string) error {
	res, err := s.DB.Exec("DELETE FROM api_keys WHERE id=?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	s.mu.Lock()
	s.apiKeys = nil
	s.mu.Unlock()
	return nil
}

// ValidateAPIKey returns the key's name. last_used_at is written at most once a minute per key.
func (s *Store) ValidateAPIKey(plain string) (string, bool) {
	if !strings.HasPrefix(plain, "nexa_sk_") {
		return "", false
	}
	s.mu.RLock()
	keys := s.apiKeys
	s.mu.RUnlock()
	if keys == nil {
		list, err := s.APIKeys()
		if err != nil {
			return "", false
		}
		keys = map[string]APIKey{}
		for _, k := range list {
			keys[k.hash] = k
		}
		s.mu.Lock()
		s.apiKeys = keys
		s.mu.Unlock()
	}
	digest := shaHex(plain)
	k, ok := keys[digest]
	if !ok {
		return "", false
	}
	now := time.Now().UTC()
	s.mu.Lock()
	stale := now.Sub(s.keyUsed[digest]) > time.Minute
	if stale {
		s.keyUsed[digest] = now
	}
	s.mu.Unlock()
	if stale {
		_, _ = s.DB.Exec("UPDATE api_keys SET last_used_at=? WHERE key_hash=?", now, digest)
	}
	return k.Name, true
}

func (s *Store) loadPrices() map[string]Price {
	s.mu.RLock()
	cached := s.prices
	s.mu.RUnlock()
	if cached != nil {
		return cached
	}
	out := map[string]Price{}
	rows, err := s.DB.Query("SELECT model,input,output FROM model_prices")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p Price
		if rows.Scan(&p.Model, &p.Input, &p.Output) == nil {
			out[strings.ToLower(p.Model)] = p
		}
	}
	s.mu.Lock()
	s.prices = out
	s.mu.Unlock()
	return out
}

func (s *Store) Prices() []Price {
	out := []Price{}
	for _, p := range s.loadPrices() {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func (s *Store) SetPrice(p Price) error {
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" || p.Input < 0 || p.Output < 0 {
		return errors.New("model is required and prices cannot be negative")
	}
	_, err := s.DB.Exec("INSERT INTO model_prices(model,input,output) VALUES(?,?,?) ON CONFLICT(model) DO UPDATE SET input=excluded.input,output=excluded.output", p.Model, p.Input, p.Output)
	s.mu.Lock()
	s.prices = nil
	s.mu.Unlock()
	return err
}

func (s *Store) DeletePrice(model string) error {
	_, err := s.DB.Exec("DELETE FROM model_prices WHERE model=?", model)
	s.mu.Lock()
	s.prices = nil
	s.mu.Unlock()
	return err
}

// CustomPrice finds an exact dashboard price, else the longest matching "prefix*" entry.
func (s *Store) CustomPrice(model string) (Price, bool) {
	prices := s.loadPrices()
	model = strings.ToLower(model)
	if p, ok := prices[model]; ok {
		return p, true
	}
	best, found := Price{}, false
	for key, p := range prices {
		if strings.HasSuffix(key, "*") && strings.HasPrefix(model, strings.TrimSuffix(key, "*")) && len(key) > len(best.Model) {
			best, found = p, true
		}
	}
	return best, found
}

func (s *Store) Health(ctx context.Context) error { return s.DB.PingContext(ctx) }
func (s *Store) Close() error                     { return s.DB.Close() }
func DuplicateError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}
func ErrMessage(err error) string {
	if DuplicateError(err) {
		return "That name or slug is already in use."
	}
	return fmt.Sprintf("%v", err)
}
