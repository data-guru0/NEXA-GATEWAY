package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// KeyLimits caps what one application API key may do. Zero means unlimited;
// an empty AllowedModels list allows every model.
type KeyLimits struct {
	MonthlyBudgetUSD float64  `json:"monthly_budget_usd"`
	RequestsPerMin   int      `json:"requests_per_minute"`
	TokensPerMin     int      `json:"tokens_per_minute"`
	AllowedModels    []string `json:"allowed_models"`
}

func normalizeLimits(l *KeyLimits) error {
	if l.MonthlyBudgetUSD < 0 || l.RequestsPerMin < 0 || l.TokensPerMin < 0 {
		return errors.New("limits cannot be negative")
	}
	models := []string{}
	seen := map[string]bool{}
	for _, m := range l.AllowedModels {
		m = strings.TrimSpace(m)
		if m != "" && !seen[strings.ToLower(m)] {
			seen[strings.ToLower(m)] = true
			models = append(models, m)
		}
	}
	l.AllowedModels = models
	return nil
}

// AllowsModel reports whether a requested model string is permitted. A pattern
// ending in "*" matches every model with that prefix (for example "openai/*").
func (l KeyLimits) AllowsModel(model string) bool {
	if len(l.AllowedModels) == 0 {
		return true
	}
	model = strings.ToLower(model)
	for _, p := range l.AllowedModels {
		p = strings.ToLower(p)
		if p == model || (strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}

// SetAPIKeyLimits replaces a key's limits.
func (s *Store) SetAPIKeyLimits(id string, l KeyLimits) (APIKey, error) {
	if err := normalizeLimits(&l); err != nil {
		return APIKey{}, err
	}
	b, _ := json.Marshal(l)
	res, err := s.DB.Exec("UPDATE api_keys SET limits_json=? WHERE id=?", string(b), id)
	if err != nil {
		return APIKey{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return APIKey{}, sql.ErrNoRows
	}
	s.Publish("apikeys")
	keys, err := s.APIKeys()
	if err != nil {
		return APIKey{}, err
	}
	for _, k := range keys {
		if k.ID == id {
			return k, nil
		}
	}
	return APIKey{}, sql.ErrNoRows
}

// MonthStart is the first instant of the current calendar month (UTC), when budgets reset.
func MonthStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// KeySpend is what a key has spent (estimated USD) since the given time.
func (s *Store) KeySpend(keyID string, since time.Time) float64 {
	var v sql.NullFloat64
	_ = s.DB.QueryRow("SELECT SUM(cost_usd) FROM traces WHERE api_key_id=? AND created_at>=?", keyID, since.UTC()).Scan(&v)
	return v.Float64
}

// KeySpends returns this month's spend for every key.
func (s *Store) KeySpends(since time.Time) map[string]float64 {
	out := map[string]float64{}
	rows, err := s.DB.Query("SELECT api_key_id, SUM(cost_usd) FROM traces WHERE api_key_id!='' AND created_at>=? GROUP BY api_key_id", since.UTC())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var v float64
		if rows.Scan(&id, &v) == nil {
			out[id] = v
		}
	}
	return out
}
