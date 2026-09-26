package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FeedbackLoop is a virtual model (feedback/<slug>) that splits traffic across
// 2-4 models and shifts it toward the ones rated best by people or by Jev.
type FeedbackLoop struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Slug             string             `json:"slug"`
	Mode             string             `json:"mode"`   // human | jev
	Status           string             `json:"status"` // running | paused | concluded
	MinShare         float64            `json:"min_share"`
	Window           int                `json:"window"`
	JevSample        float64            `json:"jev_sample"`
	JevAPIKey        string             `json:"jev_api_key,omitempty"`
	JevKeyConfigured bool               `json:"jev_key_configured"`
	Arms             []FeedbackArm      `json:"arms"`
	FrozenShares     map[string]float64 `json:"frozen_shares,omitempty"`
	// Concluding: once the leader is at least ConcludeConfidence likely best after
	// ConcludeMinVotes counted ratings, AutoConclude sends all traffic to it.
	AutoConclude       bool       `json:"auto_conclude"`
	ConcludeConfidence float64    `json:"conclude_confidence"`
	ConcludeMinVotes   int        `json:"conclude_min_votes"`
	WinnerArmID        string     `json:"winner_arm_id,omitempty"`
	ConcludedAt        *time.Time `json:"concluded_at,omitempty"`
	ConcludedBy        string     `json:"concluded_by,omitempty"` // auto | manual
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type FeedbackArm struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

// FeedbackVote is one rating of one traced response. Up and Down carry the
// vote weight: 1/0 for people, Jev's confidence for Jev, 0/0 when Jev was unsure.
type FeedbackVote struct {
	TraceID    string    `json:"trace_id"`
	LoopID     string    `json:"loop_id"`
	ArmID      string    `json:"arm_id"`
	Source     string    `json:"source"`  // human | jev
	Verdict    string    `json:"verdict"` // liked | disliked | uncertain
	Up         float64   `json:"up"`
	Down       float64   `json:"down"`
	Confidence float64   `json:"confidence"`
	CreatedAt  time.Time `json:"created_at"`
	Question   string    `json:"question,omitempty"`
}

// ArmCount is a model's weighted likes and dislikes inside the learning window.
type ArmCount struct{ Up, Down float64 }

// ArmTraffic summarises the traced requests one model served for a loop.
type ArmTraffic struct {
	Requests     int
	Errors       int
	AvgLatencyMS float64
	P95LatencyMS float64
	Cost         float64
}

// FeedbackVersion changes whenever votes or loops change, so callers can cache derived splits.
func (s *Store) FeedbackVersion() int64 { return s.feedbackVersion.Load() }
func (s *Store) bumpFeedback()          { s.Publish("feedback") } // every instance recomputes splits

func normalizeFeedbackLoop(l *FeedbackLoop) error {
	l.Name = strings.TrimSpace(l.Name)
	l.Slug = slugify(l.Slug)
	if l.Slug == "" {
		l.Slug = slugify(l.Name)
	}
	if l.Name == "" || l.Slug == "" {
		return errors.New("loop name and slug are required")
	}
	if l.Mode != "human" && l.Mode != "jev" {
		return errors.New("mode must be human or jev")
	}
	if l.Status != "paused" && l.Status != "concluded" {
		l.Status = "running"
	}
	if l.ConcludeConfidence == 0 {
		l.ConcludeConfidence = 0.95
	}
	if l.ConcludeConfidence < 0.8 || l.ConcludeConfidence > 0.999 {
		return errors.New("confidence to conclude must be between 80% and 99.9%")
	}
	if l.ConcludeMinVotes == 0 {
		l.ConcludeMinVotes = 30
	}
	l.ConcludeMinVotes = min(max(l.ConcludeMinVotes, 10), 10000)
	if len(l.Arms) < 2 || len(l.Arms) > 4 {
		return errors.New("a feedback loop needs 2 to 4 models")
	}
	seen := map[string]bool{}
	for i := range l.Arms {
		a := &l.Arms[i]
		a.Model = strings.TrimSpace(a.Model)
		if a.ProviderID == "" || a.Model == "" {
			return errors.New("every model needs a provider and a model id")
		}
		key := a.ProviderID + "\x00" + a.Model
		if seen[key] {
			return errors.New("each provider/model can appear only once")
		}
		seen[key] = true
		if a.ID == "" {
			a.ID = uuid.NewString()
		}
	}
	// Every model keeps at least MinShare of traffic; n*MinShare must leave room to learn.
	if l.MinShare < 0 {
		l.MinShare = 0
	}
	if limit := 0.9 / float64(len(l.Arms)); l.MinShare > limit {
		l.MinShare = limit
	}
	if l.Window == 0 {
		l.Window = 200
	}
	l.Window = min(max(l.Window, 10), 5000)
	if l.JevSample <= 0 || l.JevSample > 1 {
		l.JevSample = 1
	}
	l.JevSample = max(l.JevSample, 0.05)
	return nil
}

func (s *Store) saveFeedbackLoop(l FeedbackLoop, insert bool) error {
	stored := l
	stored.JevAPIKey, stored.JevKeyConfigured = "", false
	config, _ := json.Marshal(stored)
	key := ""
	if l.JevAPIKey != "" {
		enc, err := s.encrypt(l.JevAPIKey)
		if err != nil {
			return err
		}
		key = enc
	}
	var err error
	switch {
	case insert:
		_, err = s.DB.Exec("INSERT INTO feedback_loops(id,slug,config_json,jev_api_key,created_at,updated_at) VALUES(?,?,?,?,?,?)", l.ID, l.Slug, string(config), key, l.CreatedAt, l.UpdatedAt)
	case key != "":
		_, err = s.DB.Exec("UPDATE feedback_loops SET slug=?,config_json=?,jev_api_key=?,updated_at=? WHERE id=?", l.Slug, string(config), key, l.UpdatedAt, l.ID)
	default:
		_, err = s.DB.Exec("UPDATE feedback_loops SET slug=?,config_json=?,updated_at=? WHERE id=?", l.Slug, string(config), l.UpdatedAt, l.ID)
	}
	if err == nil {
		s.bumpFeedback()
	}
	return err
}

func (s *Store) CreateFeedbackLoop(l FeedbackLoop) (FeedbackLoop, error) {
	l.Status = "running"
	if err := normalizeFeedbackLoop(&l); err != nil {
		return l, err
	}
	if err := s.checkArmProviders(l.Arms); err != nil {
		return l, err
	}
	l.ID = uuid.NewString()
	l.CreatedAt = time.Now().UTC()
	l.UpdatedAt = l.CreatedAt
	l.FrozenShares = nil
	if err := s.saveFeedbackLoop(l, true); err != nil {
		return l, err
	}
	return s.FeedbackLoop(l.ID)
}

// UpdateFeedbackLoop edits settings and models; status and frozen split are changed only by SetFeedbackStatus.
func (s *Store) UpdateFeedbackLoop(l FeedbackLoop) (FeedbackLoop, error) {
	current, err := s.FeedbackLoop(l.ID)
	if err != nil {
		return l, err
	}
	l.Status = current.Status
	if err = normalizeFeedbackLoop(&l); err != nil {
		return l, err
	}
	if err = s.checkArmProviders(l.Arms); err != nil {
		return l, err
	}
	l.CreatedAt, l.UpdatedAt = current.CreatedAt, time.Now().UTC()
	l.FrozenShares = nil
	if l.Status == "paused" {
		l.FrozenShares = current.FrozenShares
	}
	l.WinnerArmID, l.ConcludedAt, l.ConcludedBy = "", nil, ""
	if l.Status == "concluded" {
		for _, a := range l.Arms {
			if a.ID == current.WinnerArmID {
				l.WinnerArmID, l.ConcludedAt, l.ConcludedBy = current.WinnerArmID, current.ConcludedAt, current.ConcludedBy
			}
		}
		if l.WinnerArmID == "" { // the winning model was removed: learn again
			l.Status = "running"
		}
	}
	if err = s.saveFeedbackLoop(l, false); err != nil {
		return l, err
	}
	return s.FeedbackLoop(l.ID)
}

func (s *Store) checkArmProviders(arms []FeedbackArm) error {
	for _, a := range arms {
		if _, err := s.Provider(a.ProviderID); err != nil {
			return errors.New("unknown provider in model list")
		}
	}
	return nil
}

// SetFeedbackStatus pauses (freezing the given split) or resumes a loop.
func (s *Store) SetFeedbackStatus(id, status string, frozen map[string]float64) (FeedbackLoop, error) {
	l, err := s.FeedbackLoop(id)
	if err != nil {
		return l, err
	}
	l.Status, l.FrozenShares, l.UpdatedAt = status, frozen, time.Now().UTC()
	if status != "paused" {
		l.Status, l.FrozenShares = "running", nil
	}
	l.WinnerArmID, l.ConcludedAt, l.ConcludedBy = "", nil, "" // pausing or resuming reopens a concluded loop
	l.JevAPIKey = ""                                          // keep the stored encrypted key untouched
	if err = s.saveFeedbackLoop(l, false); err != nil {
		return l, err
	}
	return s.FeedbackLoop(id)
}

// CountedVotes is how many ratings of the loop count (unsure Jev verdicts do not).
func (s *Store) CountedVotes(loopID string) int {
	n := 0
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM feedback_votes WHERE loop_id=? AND verdict!='uncertain'", loopID).Scan(&n)
	return n
}

// ConcludeFeedbackLoop sends all traffic to one model. by is "auto" or "manual".
func (s *Store) ConcludeFeedbackLoop(id, armID, by string) (FeedbackLoop, error) {
	l, err := s.FeedbackLoop(id)
	if err != nil {
		return l, err
	}
	found := false
	for _, a := range l.Arms {
		found = found || a.ID == armID
	}
	if !found {
		return l, errors.New("the winner must be one of the loop's models")
	}
	now := time.Now().UTC()
	l.Status, l.FrozenShares, l.WinnerArmID, l.ConcludedAt, l.ConcludedBy, l.UpdatedAt = "concluded", nil, armID, &now, by, now
	l.JevAPIKey = ""
	if err = s.saveFeedbackLoop(l, false); err != nil {
		return l, err
	}
	return s.FeedbackLoop(id)
}

func (s *Store) scanFeedbackLoop(config, enc string, created, updated time.Time, id string) (FeedbackLoop, error) {
	var l FeedbackLoop
	if err := json.Unmarshal([]byte(config), &l); err != nil {
		return l, err
	}
	l.ID, l.CreatedAt, l.UpdatedAt = id, created, updated
	if l.ConcludeConfidence == 0 { // loops saved before concluding existed
		l.ConcludeConfidence = 0.95
	}
	if l.ConcludeMinVotes == 0 {
		l.ConcludeMinVotes = 30
	}
	if enc != "" {
		key, err := s.decrypt(enc)
		if err != nil {
			return l, err
		}
		l.JevAPIKey = key
	}
	l.JevKeyConfigured = l.JevAPIKey != "" || jevKeyFromEnv()
	return l, nil
}

// FeedbackLoop returns a loop by id or slug, including its decrypted Jev key.
func (s *Store) FeedbackLoop(idOrSlug string) (FeedbackLoop, error) {
	var id, config, enc string
	var created, updated time.Time
	err := s.DB.QueryRow("SELECT id,config_json,jev_api_key,created_at,updated_at FROM feedback_loops WHERE id=? OR slug=?", idOrSlug, idOrSlug).Scan(&id, &config, &enc, &created, &updated)
	if err != nil {
		return FeedbackLoop{}, err
	}
	return s.scanFeedbackLoop(config, enc, created, updated, id)
}

// FeedbackLoops lists loops without their Jev keys.
func (s *Store) FeedbackLoops() ([]FeedbackLoop, error) {
	rows, err := s.DB.Query("SELECT id,config_json,jev_api_key,created_at,updated_at FROM feedback_loops ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FeedbackLoop{}
	for rows.Next() {
		var id, config, enc string
		var created, updated time.Time
		if err := rows.Scan(&id, &config, &enc, &created, &updated); err != nil {
			return nil, err
		}
		l, err := s.scanFeedbackLoop(config, enc, created, updated, id)
		if err != nil {
			return nil, err
		}
		l.JevAPIKey = ""
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) DeleteFeedbackLoop(id string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM feedback_votes WHERE loop_id=?", id); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM feedback_loops WHERE id=?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err = tx.Commit(); err == nil {
		s.bumpFeedback()
	}
	return err
}

// ResetFeedback forgets every vote so the loop starts again from an even split.
func (s *Store) ResetFeedback(id string) error {
	_, err := s.DB.Exec("DELETE FROM feedback_votes WHERE loop_id=?", id)
	if err == nil {
		s.bumpFeedback()
	}
	return err
}

func (s *Store) feedbackLoopsUsing(providerID string) []string {
	loops, err := s.FeedbackLoops()
	if err != nil {
		return nil
	}
	names := []string{}
	for _, l := range loops {
		for _, a := range l.Arms {
			if a.ProviderID == providerID {
				names = append(names, l.Name)
				break
			}
		}
	}
	return names
}

// SaveVote records (or replaces) the rating of one trace.
func (s *Store) SaveVote(v FeedbackVote) error {
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	_, err := s.DB.Exec(`INSERT INTO feedback_votes(trace_id,loop_id,arm_id,source,verdict,up,down,confidence,created_at) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(trace_id) DO UPDATE SET source=excluded.source,verdict=excluded.verdict,up=excluded.up,down=excluded.down,confidence=excluded.confidence,created_at=excluded.created_at`,
		v.TraceID, v.LoopID, v.ArmID, v.Source, v.Verdict, v.Up, v.Down, v.Confidence, v.CreatedAt)
	if err == nil {
		s.bumpFeedback()
	}
	return err
}

func (s *Store) DeleteVote(traceID string) error {
	_, err := s.DB.Exec("DELETE FROM feedback_votes WHERE trace_id=?", traceID)
	if err == nil {
		s.bumpFeedback()
	}
	return err
}

func (s *Store) VoteForTrace(traceID string) (FeedbackVote, error) {
	var v FeedbackVote
	err := s.DB.QueryRow("SELECT trace_id,loop_id,arm_id,source,verdict,up,down,confidence,created_at FROM feedback_votes WHERE trace_id=?", traceID).
		Scan(&v.TraceID, &v.LoopID, &v.ArmID, &v.Source, &v.Verdict, &v.Up, &v.Down, &v.Confidence, &v.CreatedAt)
	return v, err
}

// ArmCounts sums each model's weighted likes and dislikes over its latest `window` votes.
func (s *Store) ArmCounts(l FeedbackLoop) map[string]ArmCount {
	out := map[string]ArmCount{}
	for _, a := range l.Arms {
		var c ArmCount
		_ = s.DB.QueryRow(`SELECT COALESCE(SUM(up),0),COALESCE(SUM(down),0) FROM (SELECT up,down FROM feedback_votes WHERE loop_id=? AND arm_id=? AND verdict!='uncertain' ORDER BY created_at DESC LIMIT ?) AS recent`, l.ID, a.ID, l.Window).Scan(&c.Up, &c.Down)
		out[a.ID] = c
	}
	return out
}

// Votes returns every vote of a loop, oldest first, for replaying the split over time.
func (s *Store) Votes(loopID string) ([]FeedbackVote, error) {
	rows, err := s.DB.Query("SELECT trace_id,arm_id,source,verdict,up,down,confidence,created_at FROM feedback_votes WHERE loop_id=? ORDER BY created_at", loopID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FeedbackVote{}
	for rows.Next() {
		v := FeedbackVote{LoopID: loopID}
		if err := rows.Scan(&v.TraceID, &v.ArmID, &v.Source, &v.Verdict, &v.Up, &v.Down, &v.Confidence, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RecentVotes returns the latest ratings with the question that was asked.
func (s *Store) RecentVotes(loopID string, limit int) ([]FeedbackVote, error) {
	rows, err := s.DB.Query(`SELECT v.trace_id,v.arm_id,v.source,v.verdict,v.up,v.down,v.confidence,v.created_at,COALESCE(t.prompt,'') FROM feedback_votes v LEFT JOIN traces t ON t.id=v.trace_id WHERE v.loop_id=? ORDER BY v.created_at DESC LIMIT ?`, loopID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FeedbackVote{}
	for rows.Next() {
		v := FeedbackVote{LoopID: loopID}
		var prompt string
		if err := rows.Scan(&v.TraceID, &v.ArmID, &v.Source, &v.Verdict, &v.Up, &v.Down, &v.Confidence, &v.CreatedAt, &prompt); err != nil {
			return nil, err
		}
		v.Question = promptText(prompt)
		out = append(out, v)
	}
	return out, rows.Err()
}

// promptText extracts the user text from a stored single-message prompt.
func promptText(prompt string) string {
	var msgs []struct {
		Content any `json:"content"`
	}
	text := prompt
	if json.Unmarshal([]byte(prompt), &msgs) == nil && len(msgs) > 0 {
		if s, ok := msgs[0].Content.(string); ok {
			text = s
		}
	}
	if r := []rune(text); len(r) > 160 {
		text = string(r[:160]) + "…"
	}
	return text
}

// ArmTraffic aggregates traced requests per model for a loop (all time).
func (s *Store) LoopTraffic(loopID string) (map[string]*ArmTraffic, error) {
	rows, err := s.DB.Query(`SELECT status,latency_ms,cost_usd,metadata::text FROM traces WHERE metadata->>'loop_id'=?`, loopID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*ArmTraffic{}
	latencies := map[string][]int64{}
	for rows.Next() {
		var status, metadata string
		var latency int64
		var cost float64
		if err := rows.Scan(&status, &latency, &cost, &metadata); err != nil {
			return nil, err
		}
		var meta struct {
			ArmID string `json:"arm_id"`
		}
		if json.Unmarshal([]byte(metadata), &meta) != nil || meta.ArmID == "" {
			continue
		}
		a := out[meta.ArmID]
		if a == nil {
			a = &ArmTraffic{}
			out[meta.ArmID] = a
		}
		a.Requests++
		a.Cost += cost
		if status != "success" {
			a.Errors++
		}
		latencies[meta.ArmID] = append(latencies[meta.ArmID], latency)
	}
	for id, values := range latencies {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		var sum int64
		for _, v := range values {
			sum += v
		}
		out[id].AvgLatencyMS = float64(sum) / float64(len(values))
		out[id].P95LatencyMS = float64(values[(len(values)-1)*95/100])
	}
	return out, rows.Err()
}
