package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type Store struct {
	DB   *sql.DB
	aead cipher.AEAD
}

type Provider struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Type      string    `json:"type"`
	BaseURL   string    `json:"base_url"`
	APIKey    string    `json:"-"`
	MaskedKey string    `json:"masked_key"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type Trace struct {
	ID           string    `json:"id"`
	ProviderID   string    `json:"provider_id"`
	ProviderName string    `json:"provider_name"`
	Model        string    `json:"model"`
	Status       string    `json:"status"`
	StatusCode   int       `json:"status_code"`
	Prompt       string    `json:"prompt"`
	Response     string    `json:"response"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	LatencyMS    int64     `json:"latency_ms"`
	UpstreamMS   float64   `json:"upstream_latency_ms"`
	GatewayMS    float64   `json:"gateway_latency_ms"`
	CostUSD      float64   `json:"cost_usd"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Metadata     string    `json:"metadata,omitempty"`
}

type Stats struct {
	Requests24H   int64     `json:"requests_24h"`
	Requests      int64     `json:"requests"`
	SuccessRate   float64   `json:"success_rate"`
	Tokens24H     int64     `json:"tokens_24h"`
	Cost24H       float64   `json:"cost_24h"`
	AvgLatencyMS  float64   `json:"avg_latency_ms"`
	AvgUpstreamMS float64   `json:"avg_upstream_latency_ms"`
	AvgGatewayMS  float64   `json:"avg_gateway_latency_ms"`
	Providers     int64     `json:"providers"`
	Hourly        []Point   `json:"hourly"`
	Range         string    `json:"range"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
}

type Point struct {
	Hour       string    `json:"hour"`
	Timestamp  time.Time `json:"timestamp"`
	Requests   int64     `json:"requests"`
	Errors     int64     `json:"errors"`
	UpstreamMS float64   `json:"upstream_latency_ms"`
	GatewayMS  float64   `json:"gateway_latency_ms"`
}

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// RoutingProfile is a dashboard-managed virtual model. Targets are deliberately
// provider-agnostic: any enabled Nexa provider/model pair can participate.
type RoutingProfile struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Slug                string          `json:"slug"`
	Engine              string          `json:"engine"`
	Objective           string          `json:"objective"`
	QualityWeight       float64         `json:"quality_weight"`
	CostWeight          float64         `json:"cost_weight"`
	LatencyWeight       float64         `json:"latency_weight"`
	ConfidenceThreshold float64         `json:"confidence_threshold"`
	MaxRetries          int             `json:"max_retries"`
	RequestTimeoutMS    int             `json:"request_timeout_ms"`
	Active              bool            `json:"active"`
	JevAPIKey           string          `json:"jev_api_key,omitempty"`
	JevKeyConfigured    bool            `json:"jev_key_configured"`
	Targets             []RoutingTarget `json:"targets"`
	Rules               []RoutingRule   `json:"rules"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

type RoutingTarget struct {
	ID                   string   `json:"id"`
	ProviderID           string   `json:"provider_id"`
	Model                string   `json:"model"`
	Tier                 string   `json:"tier"`
	Description          string   `json:"description"`
	TaskTypes            []string `json:"task_types"`
	Capabilities         []string `json:"capabilities"`
	ContextWindow        int      `json:"context_window"`
	MaxOutputTokens      int      `json:"max_output_tokens"`
	InputCostPerMillion  float64  `json:"input_cost_per_million"`
	OutputCostPerMillion float64  `json:"output_cost_per_million"`
	QualityScore         float64  `json:"quality_score"`
	Priority             int      `json:"priority"`
	Enabled              bool     `json:"enabled"`
}

type RoutingRule struct {
	ID       string `json:"id"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Action   string `json:"action"`
	TargetID string `json:"target_id"`
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
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, aead: aead}
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
CREATE INDEX IF NOT EXISTS idx_traces_created ON traces(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_traces_provider ON traces(provider_id);
CREATE INDEX IF NOT EXISTS idx_routing_active ON routing_profiles(active);
CREATE INDEX IF NOT EXISTS idx_routing_attempt_health ON routing_attempts(provider_id,model,created_at DESC);
`)
	if err != nil {
		return err
	}
	if err := s.ensureColumn("traces", "upstream_latency_ms", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("traces", "gateway_latency_ms", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// The bundled local classifier was removed; existing profiles move to Jev.
	if _, err = s.DB.Exec("UPDATE routing_profiles SET engine='jev' WHERE engine!='jev'"); err != nil {
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

func (s *Store) ensureMasterKey() (string, error) {
	var existing string
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='master_key_hash'").Scan(&existing); err == nil {
		return "", nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	master := "nexa_" + base64.RawURLEncoding.EncodeToString(raw)
	hash, err := bcrypt.GenerateFromPassword([]byte(master), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	_, err = s.DB.Exec("INSERT INTO settings(key,value) VALUES('master_key_hash',?)", string(hash))
	return master, err
}

func (s *Store) ValidateMaster(key string) bool {
	var hash string
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='master_key_hash'").Scan(&hash); err != nil {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(key)) == nil
}

func (s *Store) ResetMasterKey() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	master := "nexa_" + base64.RawURLEncoding.EncodeToString(raw)
	hash, err := bcrypt.GenerateFromPassword([]byte(master), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	_, err = s.DB.Exec("UPDATE settings SET value=? WHERE key='master_key_hash'", string(hash))
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

func (s *Store) CreateProvider(p Provider) (Provider, error) {
	p.ID = uuid.NewString()
	p.Slug = slugify(p.Slug)
	if p.Slug == "" {
		p.Slug = slugify(p.Name)
	}
	if p.Name == "" || p.Slug == "" || p.APIKey == "" {
		return p, errors.New("name, slug and API key are required")
	}
	enc, err := s.encrypt(p.APIKey)
	if err != nil {
		return p, err
	}
	p.CreatedAt = time.Now().UTC()
	_, err = s.DB.Exec("INSERT INTO providers(id,name,slug,type,base_url,api_key,enabled,created_at) VALUES(?,?,?,?,?,?,?,?)",
		p.ID, p.Name, p.Slug, p.Type, strings.TrimRight(p.BaseURL, "/"), enc, p.Enabled, p.CreatedAt)
	p.MaskedKey = MaskKey(p.APIKey)
	return p, err
}

func (s *Store) UpdateProvider(p Provider) (Provider, error) {
	p.Slug = slugify(p.Slug)
	if p.APIKey != "" {
		enc, err := s.encrypt(p.APIKey)
		if err != nil {
			return p, err
		}
		_, err = s.DB.Exec("UPDATE providers SET name=?,slug=?,type=?,base_url=?,api_key=?,enabled=? WHERE id=?",
			p.Name, p.Slug, p.Type, strings.TrimRight(p.BaseURL, "/"), enc, p.Enabled, p.ID)
		if err != nil {
			return p, err
		}
	} else {
		_, err := s.DB.Exec("UPDATE providers SET name=?,slug=?,type=?,base_url=?,enabled=? WHERE id=?",
			p.Name, p.Slug, p.Type, strings.TrimRight(p.BaseURL, "/"), p.Enabled, p.ID)
		if err != nil {
			return p, err
		}
	}
	return s.Provider(p.ID)
}

func (s *Store) Provider(idOrSlug string) (Provider, error) {
	var p Provider
	var enc string
	err := s.DB.QueryRow("SELECT id,name,slug,type,base_url,api_key,enabled,created_at FROM providers WHERE id=? OR slug=?", idOrSlug, idOrSlug).
		Scan(&p.ID, &p.Name, &p.Slug, &p.Type, &p.BaseURL, &enc, &p.Enabled, &p.CreatedAt)
	if err != nil {
		return p, err
	}
	p.APIKey, err = s.decrypt(enc)
	p.MaskedKey = MaskKey(p.APIKey)
	return p, err
}

func (s *Store) Providers() ([]Provider, error) {
	rows, err := s.DB.Query("SELECT id,name,slug,type,base_url,api_key,enabled,created_at FROM providers ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Provider{}
	for rows.Next() {
		var p Provider
		var enc string
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Type, &p.BaseURL, &enc, &p.Enabled, &p.CreatedAt); err != nil {
			return nil, err
		}
		key, err := s.decrypt(enc)
		if err != nil {
			return nil, err
		}
		p.MaskedKey = MaskKey(key)
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) DeleteProvider(id string) error {
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
	return tx.Commit()
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
	p.Engine = "jev" // ponytail: Jev is the only engine; field kept for API/trace compatibility
	switch p.Objective {
	case "balanced", "lowest_cost", "lowest_latency", "highest_quality", "custom":
	default:
		return errors.New("unsupported optimization objective")
	}
	if len(p.Targets) == 0 {
		return errors.New("add at least one routing target")
	}
	if p.ConfidenceThreshold <= 0 || p.ConfidenceThreshold > 1 {
		p.ConfidenceThreshold = .62
	}
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	}
	if p.MaxRetries > 5 {
		p.MaxRetries = 5
	}
	if p.RequestTimeoutMS < 1000 {
		p.RequestTimeoutMS = 30000
	}
	if p.RequestTimeoutMS > 600000 {
		p.RequestTimeoutMS = 600000
	}
	if p.Objective != "custom" {
		p.QualityWeight, p.CostWeight, p.LatencyWeight = 50, 30, 20
	}
	if p.QualityWeight+p.CostWeight+p.LatencyWeight <= 0 {
		return errors.New("optimization weights must total more than zero")
	}
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
		if t.QualityScore <= 0 {
			t.QualityScore = 70
		}
		if t.QualityScore > 100 {
			t.QualityScore = 100
		}
		if t.ContextWindow <= 0 {
			t.ContextWindow = 128000
		}
		if t.MaxOutputTokens <= 0 {
			t.MaxOutputTokens = 8192
		}
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

func (s *Store) CreateRoutingProfile(p RoutingProfile) (RoutingProfile, error) {
	if err := normalizeRoutingProfile(&p); err != nil {
		return p, err
	}
	p.ID = uuid.NewString()
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	for i := range p.Rules {
		if p.Rules[i].ID == "" {
			p.Rules[i].ID = uuid.NewString()
		}
	}
	stored := p
	stored.JevAPIKey = ""
	config, _ := json.Marshal(stored)
	key := ""
	var err error
	if p.JevAPIKey != "" {
		key, err = s.encrypt(p.JevAPIKey)
		if err != nil {
			return p, err
		}
	}
	_, err = s.DB.Exec("INSERT INTO routing_profiles(id,name,slug,engine,objective,config_json,jev_api_key,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)", p.ID, p.Name, p.Slug, p.Engine, p.Objective, string(config), key, false, now, now)
	if err != nil {
		return p, err
	}
	p.JevKeyConfigured = p.JevAPIKey != "" || os.Getenv("JEV_API_KEY") != "" || os.Getenv("TYPESAFE_API_KEY") != ""
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
	for i := range p.Rules {
		if p.Rules[i].ID == "" {
			p.Rules[i].ID = uuid.NewString()
		}
	}
	stored := p
	stored.JevAPIKey = ""
	config, _ := json.Marshal(stored)
	if p.JevAPIKey != "" {
		enc, e := s.encrypt(p.JevAPIKey)
		if e != nil {
			return p, e
		}
		_, err = s.DB.Exec("UPDATE routing_profiles SET name=?,slug=?,engine=?,objective=?,config_json=?,jev_api_key=?,updated_at=? WHERE id=?", p.Name, p.Slug, p.Engine, p.Objective, string(config), enc, p.UpdatedAt, p.ID)
	} else {
		_, err = s.DB.Exec("UPDATE routing_profiles SET name=?,slug=?,engine=?,objective=?,config_json=?,updated_at=? WHERE id=?", p.Name, p.Slug, p.Engine, p.Objective, string(config), p.UpdatedAt, p.ID)
	}
	if err != nil {
		return p, err
	}
	return s.RoutingProfile(p.ID)
}

func (s *Store) scanRouting(row interface{ Scan(...any) error }) (RoutingProfile, error) {
	var p RoutingProfile
	var config, enc string
	var id, name, slug, engine, objective string
	var active bool
	var created, updated time.Time
	err := row.Scan(&id, &name, &slug, &engine, &objective, &config, &enc, &active, &created, &updated)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(config), &p); err != nil {
		return p, err
	}
	p.ID, p.Name, p.Slug, p.Engine, p.Objective, p.Active, p.CreatedAt, p.UpdatedAt = id, name, slug, engine, objective, active, created, updated
	if enc != "" {
		p.JevAPIKey, err = s.decrypt(enc)
		if err != nil {
			return p, err
		}
	}
	p.JevKeyConfigured = p.JevAPIKey != "" || os.Getenv("JEV_API_KEY") != "" || os.Getenv("TYPESAFE_API_KEY") != ""
	return p, nil
}

func (s *Store) RoutingProfile(idOrSlug string) (RoutingProfile, error) {
	return s.scanRouting(s.DB.QueryRow("SELECT id,name,slug,engine,objective,config_json,jev_api_key,active,created_at,updated_at FROM routing_profiles WHERE id=? OR slug=?", idOrSlug, idOrSlug))
}
func (s *Store) ActiveRoutingProfile() (RoutingProfile, error) {
	return s.scanRouting(s.DB.QueryRow("SELECT id,name,slug,engine,objective,config_json,jev_api_key,active,created_at,updated_at FROM routing_profiles WHERE active=1 LIMIT 1"))
}
func (s *Store) RoutingProfiles() ([]RoutingProfile, error) {
	rows, err := s.DB.Query("SELECT id,name,slug,engine,objective,config_json,jev_api_key,active,created_at,updated_at FROM routing_profiles ORDER BY active DESC,updated_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoutingProfile{}
	for rows.Next() {
		p, e := s.scanRouting(rows)
		if e != nil {
			return nil, e
		}
		p.JevAPIKey = ""
		out = append(out, p)
	}
	return out, rows.Err()
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
	return tx.Commit()
}
func (s *Store) DeleteRoutingProfile(id string) error {
	_, e := s.DB.Exec("DELETE FROM routing_profiles WHERE id=?", id)
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

func (s *Store) AddTrace(t Trace) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.DB.Exec(`INSERT INTO traces(id,provider_id,provider_name,model,status,status_code,prompt,response,input_tokens,output_tokens,total_tokens,latency_ms,upstream_latency_ms,gateway_latency_ms,cost_usd,error,metadata,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.ProviderID, t.ProviderName, t.Model, t.Status, t.StatusCode, t.Prompt, t.Response, t.InputTokens, t.OutputTokens, t.TotalTokens, t.LatencyMS, t.UpstreamMS, t.GatewayMS, t.CostUSD, t.Error, t.Metadata, t.CreatedAt)
	return err
}

func (s *Store) Traces(limit, offset int, search, status string) ([]Trace, int, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	q := " FROM traces WHERE 1=1"
	args := []any{}
	for _, term := range strings.Fields(strings.TrimSpace(search)) {
		q += ` AND (LOWER(id) LIKE ? OR LOWER(provider_name) LIKE ? OR LOWER(model) LIKE ? OR LOWER(prompt) LIKE ? OR LOWER(response) LIKE ? OR LOWER(status) LIKE ? OR LOWER(COALESCE(error,'')) LIKE ?)`
		x := "%" + strings.ToLower(term) + "%"
		args = append(args, x, x, x, x, x, x, x)
	}
	if status != "" && status != "all" {
		q += " AND status=?"
		args = append(args, status)
	}
	var total int
	if err := s.DB.QueryRow("SELECT COUNT(*)"+q, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query("SELECT id,provider_id,provider_name,model,status,status_code,prompt,response,input_tokens,output_tokens,total_tokens,latency_ms,upstream_latency_ms,gateway_latency_ms,cost_usd,error,metadata,created_at"+q+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Trace{}
	for rows.Next() {
		var t Trace
		if err := rows.Scan(&t.ID, &t.ProviderID, &t.ProviderName, &t.Model, &t.Status, &t.StatusCode, &t.Prompt, &t.Response, &t.InputTokens, &t.OutputTokens, &t.TotalTokens, &t.LatencyMS, &t.UpstreamMS, &t.GatewayMS, &t.CostUSD, &t.Error, &t.Metadata, &t.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

func (s *Store) Trace(id string) (Trace, error) {
	var t Trace
	err := s.DB.QueryRow("SELECT id,provider_id,provider_name,model,status,status_code,prompt,response,input_tokens,output_tokens,total_tokens,latency_ms,upstream_latency_ms,gateway_latency_ms,cost_usd,error,metadata,created_at FROM traces WHERE id=?", id).Scan(&t.ID, &t.ProviderID, &t.ProviderName, &t.Model, &t.Status, &t.StatusCode, &t.Prompt, &t.Response, &t.InputTokens, &t.OutputTokens, &t.TotalTokens, &t.LatencyMS, &t.UpstreamMS, &t.GatewayMS, &t.CostUSD, &t.Error, &t.Metadata, &t.CreatedAt)
	return t, err
}

func (s *Store) Stats(from, to time.Time, bucket time.Duration, rangeName string) (Stats, error) {
	x := Stats{SuccessRate: 100, Range: rangeName, From: from, To: to, Hourly: []Point{}}
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
	rows, err := s.DB.Query(`SELECT status,total_tokens,cost_usd,latency_ms,upstream_latency_ms,gateway_latency_ms,created_at FROM traces WHERE created_at>=? AND created_at<=? ORDER BY created_at`, from.UTC(), to.UTC())
	if err != nil {
		return x, err
	}
	defer rows.Close()
	var successes int64
	for rows.Next() {
		var status string
		var tokens int64
		var cost float64
		var latency int64
		var upstream, gateway float64
		var created time.Time
		if err := rows.Scan(&status, &tokens, &cost, &latency, &upstream, &gateway, &created); err != nil {
			return x, err
		}
		x.Requests++
		x.Tokens24H += tokens
		x.Cost24H += cost
		x.AvgLatencyMS += float64(latency)
		x.AvgUpstreamMS += float64(upstream)
		x.AvgGatewayMS += float64(gateway)
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
		if status != "success" {
			x.Hourly[idx].Errors++
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
	}
	for i := range x.Hourly {
		if bucketTotals[i].count > 0 {
			x.Hourly[i].UpstreamMS = bucketTotals[i].upstream / float64(bucketTotals[i].count)
			x.Hourly[i].GatewayMS = bucketTotals[i].gateway / float64(bucketTotals[i].count)
		}
	}
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM providers WHERE enabled=1").Scan(&x.Providers)
	return x, nil
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

func (s *Store) CreateSession(userID string, master bool) (string, error) {
	id := uuid.NewString() + uuid.NewString()
	var owner any = userID
	if master {
		owner = nil
	}
	_, err := s.DB.Exec("INSERT INTO sessions(id,user_id,is_master,expires_at) VALUES(?,?,?,?)", id, owner, master, time.Now().Add(7*24*time.Hour).UTC())
	return id, err
}
func (s *Store) Session(id string) (string, bool, error) {
	var uid sql.NullString
	var master bool
	err := s.DB.QueryRow("SELECT user_id,is_master FROM sessions WHERE id=? AND expires_at>?", id, time.Now().UTC()).Scan(&uid, &master)
	return uid.String, master, err
}
func (s *Store) DeleteSession(id string) { s.DB.Exec("DELETE FROM sessions WHERE id=?", id) }
func (s *Store) CreateUser(username, password, role string) (User, error) {
	var u User
	if len(username) < 3 || len(password) < 8 {
		return u, errors.New("username must be 3+ characters and password 8+ characters")
	}
	if role != "admin" {
		role = "member"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return u, err
	}
	u = User{ID: uuid.NewString(), Username: strings.TrimSpace(username), Role: role, CreatedAt: time.Now().UTC()}
	_, err = s.DB.Exec("INSERT INTO users(id,username,password_hash,role,created_at) VALUES(?,?,?,?,?)", u.ID, u.Username, string(hash), u.Role, u.CreatedAt)
	return u, err
}
func (s *Store) LoginUser(username, password string) (User, error) {
	var u User
	var hash string
	err := s.DB.QueryRow("SELECT id,username,password_hash,role,created_at FROM users WHERE username=?", username).Scan(&u.ID, &u.Username, &hash, &u.Role, &u.CreatedAt)
	if err != nil {
		return u, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return u, errors.New("invalid credentials")
	}
	return u, nil
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
