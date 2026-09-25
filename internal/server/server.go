package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evolvue/nexa-gateway/internal/gateway"
	"github.com/evolvue/nexa-gateway/internal/store"
	webassets "github.com/evolvue/nexa-gateway/web"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	Store         *store.Store
	Gateway       *gateway.Gateway
	Log           *slog.Logger
	SecureCookies bool
	logins        *limiter
	checksMu      sync.Mutex
	checks        map[string]providerCheck
}

type principalKey struct{}

type principal struct {
	store.SessionInfo
	token string
}

type providerCheck struct {
	OK        bool      `json:"ok"`
	Models    int       `json:"models"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

func New(s *store.Store, log *slog.Logger, secureCookies bool) http.Handler {
	x := &Server{Store: s, Gateway: gateway.New(s), Log: log, SecureCookies: secureCookies, logins: &limiter{hits: map[string]*window{}}, checks: map[string]providerCheck{}}
	go x.healthLoop()
	r := chi.NewRouter()
	r.Use(middleware.RealIP, middleware.RequestID, middleware.Recoverer, x.securityHeaders, x.accessLog, gateway.StartClock)
	r.Get("/healthz", x.health)
	r.With(x.gatewayAuth).Post("/v1/chat/completions", x.Gateway.ChatCompletions)
	r.With(x.gatewayAuth).Get("/v1/models", x.allModels)
	r.Route("/api", func(r chi.Router) {
		r.Post("/auth/login", x.login)
		r.Post("/auth/logout", x.logout)
		r.Get("/bootstrap", x.bootstrap)
		r.Group(func(r chi.Router) {
			r.Use(x.dashboardAuth, x.sameOrigin)
			r.Get("/auth/me", x.me)
			r.Post("/auth/password", x.changePassword)
			r.Get("/stats", x.stats)
			r.Get("/providers", x.providers)
			r.Get("/providers/{id}/models", x.models)
			r.Get("/routing/profiles", x.routingProfiles)
			r.Post("/routing/evaluate", x.evaluateRouting)
			r.Get("/traces", x.traces)
			r.Get("/traces/{id}", x.trace)
			r.Get("/users", x.users)
			r.Get("/prices", x.prices)
			r.Get("/api-keys", x.apiKeys)
			r.Post("/playground/chat", x.playground)
			r.Group(func(r chi.Router) {
				r.Use(x.requireAdmin)
				r.Post("/providers", x.createProvider)
				r.Put("/providers/{id}", x.updateProvider)
				r.Delete("/providers/{id}", x.deleteProvider)
				r.Post("/routing/profiles", x.createRoutingProfile)
				r.Put("/routing/profiles/{id}", x.updateRoutingProfile)
				r.Delete("/routing/profiles/{id}", x.deleteRoutingProfile)
				r.Post("/routing/profiles/{id}/activate", x.activateRoutingProfile)
				r.Post("/routing/profiles/{id}/check", x.checkRoutingProfile)
				r.Post("/users", x.createUser)
				r.Put("/users/{id}", x.updateUser)
				r.Delete("/users/{id}", x.deleteUser)
				r.Put("/prices", x.setPrice)
				r.Delete("/prices", x.deletePrice)
				r.Post("/api-keys", x.createAPIKey)
				r.Delete("/api-keys/{id}", x.deleteAPIKey)
				r.Post("/master-key/rotate", x.rotateMaster)
			})
		})
	})
	static, _ := fs.Sub(webassets.Files, ".")
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		body, err := fs.ReadFile(static, p)
		if err != nil {
			p = "index.html"
			body, _ = fs.ReadFile(static, p)
		}
		if contentType := mime.TypeByExtension(filepath.Ext(p)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		// Assets are embedded and unversioned, so always revalidate after a container update.
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
	})
	return r
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		started := time.Now()
		next.ServeHTTP(ww, r)
		if r.URL.Path != "/healthz" {
			s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", ww.Status(), "duration_ms", time.Since(started).Milliseconds(), "request_id", middleware.GetReqID(r.Context()))
		}
	})
}

// gatewayAuth accepts the master key or a revocable per-app API key.
func (s *Server) gatewayAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if s.Store.ValidateMaster(token) {
			next.ServeHTTP(w, gateway.Annotate(r, "api", "master key"))
			return
		}
		if name, ok := s.Store.ValidateAPIKey(token); ok {
			next.ServeHTTP(w, gateway.Annotate(r, "api", name))
			return
		}
		writeError(w, 401, "A valid Nexa API key or master key is required.")
	})
}

// dashboardAuth stores the session principal in the request context. Nothing
// about identity is read from request headers, so clients cannot forge it.
func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("nexa_session")
		if err != nil {
			writeError(w, 401, "Sign in to continue.")
			return
		}
		info, err := s.Store.Session(c.Value)
		if err != nil {
			writeError(w, 401, "Your session expired. Sign in again.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{info, c.Value})))
	})
}

func who(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := who(r); !p.Master && p.Role != "admin" {
			writeError(w, 403, "Administrator access is required.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" || r.Method == "HEAD" {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && !strings.EqualFold(origin, "http://"+r.Host) && !strings.EqualFold(origin, "https://"+r.Host) {
			writeError(w, 403, "Origin check failed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limiter blocks an address after 10 failed logins until its 10-minute window resets.
type limiter struct {
	mu   sync.Mutex
	hits map[string]*window
}
type window struct {
	count int
	reset time.Time
}

const (
	loginFailures = 10
	loginWindow   = 10 * time.Minute
)

func (l *limiter) blocked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w, ok := l.hits[key]; ok && w.count >= loginFailures && time.Now().Before(w.reset) {
		return time.Until(w.reset)
	}
	return 0
}
func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, w := range l.hits { // ponytail: sweep on write; fine for login-rate traffic
		if now.After(w.reset) {
			delete(l.hits, k)
		}
	}
	w, ok := l.hits[key]
	if !ok {
		w = &window{reset: now.Add(loginWindow)}
		l.hits[key] = w
	}
	w.count++
}
func (l *limiter) clear(key string) {
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: "nexa_session", Value: token, Path: "/", HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 7 * 24 * 3600})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if wait := s.logins.blocked(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, 429, "Too many failed sign-in attempts. Try again in a few minutes.")
		return
	}
	var in struct{ MasterKey, Username, Password string }
	if !decode(w, r, &in) {
		return
	}
	var info store.SessionInfo
	switch {
	case in.MasterKey != "":
		if !s.Store.ValidateMaster(in.MasterKey) {
			s.logins.fail(ip)
			writeError(w, 401, "Invalid master key.")
			return
		}
		info = store.SessionInfo{Master: true, Username: "Master administrator", Role: "owner"}
	case in.Username != "":
		u, err := s.Store.LoginUser(in.Username, in.Password)
		if err != nil {
			s.logins.fail(ip)
			writeError(w, 401, "Invalid username or password.")
			return
		}
		info = store.SessionInfo{UserID: u.ID, Username: u.Username, Role: u.Role}
	default:
		writeError(w, 401, "Enter a master key or a username and password.")
		return
	}
	s.logins.clear(ip)
	token, err := s.Store.CreateSession(info.UserID, info.Master)
	if err != nil {
		writeError(w, 500, "Could not create session.")
		return
	}
	s.setSession(w, token)
	writeJSON(w, 200, meJSON(info))
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("nexa_session"); err == nil {
		s.Store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "nexa_session", Value: "", Path: "/", HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(204)
}
func meJSON(i store.SessionInfo) map[string]any {
	return map[string]any{"id": i.UserID, "username": i.Username, "role": i.Role, "master": i.Master}
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, meJSON(who(r).SessionInfo))
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if p.Master {
		writeError(w, 400, "The master administrator signs in with the master key; rotate it instead.")
		return
	}
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.ChangePassword(p.UserID, in.Current, in.New, p.token); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"name": "Nexa Gateway", "version": "0.1.0"})
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Health(r.Context()); err != nil {
		writeError(w, 503, "Database unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "service": "nexa-gateway", "time": time.Now().UTC()})
}
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	from, to, bucket, rangeName, err := statsRange(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	v, err := s.Store.Stats(from, to, bucket, rangeName)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, v)
}

func statsRange(r *http.Request) (time.Time, time.Time, time.Duration, string, error) {
	now := time.Now().UTC()
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "24h"
	}
	spans := map[string]struct{ span, bucket time.Duration }{
		"5m": {5 * time.Minute, 30 * time.Second}, "30m": {30 * time.Minute, 2 * time.Minute},
		"1h": {time.Hour, 5 * time.Minute}, "6h": {6 * time.Hour, 30 * time.Minute},
		"24h": {24 * time.Hour, time.Hour}, "7d": {7 * 24 * time.Hour, 12 * time.Hour},
		"30d": {30 * 24 * time.Hour, 24 * time.Hour},
	}
	if spec, ok := spans[rangeName]; ok {
		from := now.Add(-spec.span).Truncate(spec.bucket)
		return from, now, spec.bucket, rangeName, nil
	}
	if rangeName != "custom" {
		return time.Time{}, time.Time{}, 0, "", errors.New("unsupported stats range")
	}
	from, err := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	if err != nil {
		return time.Time{}, time.Time{}, 0, "", errors.New("custom range requires a valid from timestamp")
	}
	to, err := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err != nil {
		return time.Time{}, time.Time{}, 0, "", errors.New("custom range requires a valid to timestamp")
	}
	span := to.Sub(from)
	if span <= 0 || span > 366*24*time.Hour {
		return time.Time{}, time.Time{}, 0, "", errors.New("custom range must be between one minute and 366 days")
	}
	bucket := span / 60
	if bucket < time.Minute {
		bucket = time.Minute
	}
	return from.UTC(), to.UTC(), bucket, rangeName, nil
}

func defaultBase(t string) string {
	switch strings.ToLower(t) {
	case "openai":
		return "https://api.openai.com/v1"
	case "groq":
		return "https://api.groq.com/openai/v1"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta/openai"
	case "anthropic":
		return "https://api.anthropic.com/v1"
	default:
		return ""
	}
}

type providerInput struct {
	Name         string  `json:"name"`
	Slug         string  `json:"slug"`
	Type         string  `json:"type"`
	BaseURL      string  `json:"base_url"`
	APIKey       string  `json:"api_key"`
	Enabled      *bool   `json:"enabled"`
	ExtraHeaders *string `json:"extra_headers"` // "Name: value" per line; omitted keeps, "" clears
}

func (in providerInput) provider() (store.Provider, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	p := store.Provider{Name: in.Name, Slug: in.Slug, Type: in.Type, BaseURL: in.BaseURL, APIKey: in.APIKey, Enabled: enabled}
	if p.BaseURL == "" {
		p.BaseURL = defaultBase(p.Type)
	}
	if in.ExtraHeaders != nil {
		p.Headers = map[string]string{}
		for _, line := range strings.Split(*in.ExtraHeaders, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			name, value, ok := strings.Cut(line, ":")
			name = strings.TrimSpace(name)
			if !ok || name == "" || strings.ContainsAny(name, " \t\r") {
				return p, errors.New("extra headers must be one \"Name: value\" per line")
			}
			p.Headers[http.CanonicalHeaderKey(name)] = strings.TrimSpace(value)
		}
	}
	return p, nil
}

type providerView struct {
	store.Provider
	Check *providerCheck `json:"check,omitempty"`
}

func (s *Server) view(p store.Provider) providerView {
	s.checksMu.Lock()
	defer s.checksMu.Unlock()
	if c, ok := s.checks[p.ID]; ok {
		return providerView{p, &c}
	}
	return providerView{Provider: p}
}

// checkProvider verifies the key by listing models, so a bad credential shows up on save.
func (s *Server) checkProvider(ctx context.Context, p store.Provider) {
	s.Gateway.ForgetModels(p.ID)
	c := providerCheck{CheckedAt: time.Now().UTC()}
	if p.Enabled {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		models, err := s.Gateway.Models(ctx, p, true)
		if err != nil {
			c.Error = err.Error()
		} else {
			c.OK, c.Models = true, len(models)
		}
	} else {
		c.Error = "paused"
	}
	s.checksMu.Lock()
	s.checks[p.ID] = c
	s.checksMu.Unlock()
}

// healthLoop re-verifies every provider shortly after start and then every ten
// minutes, so dashboard health reflects real reachability rather than the last save.
func (s *Server) healthLoop() {
	time.Sleep(3 * time.Second)
	for {
		if providers, err := s.Store.Providers(); err == nil {
			for _, p := range providers {
				s.checkProvider(context.Background(), p)
			}
		}
		time.Sleep(10 * time.Minute)
	}
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Providers()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := make([]providerView, len(v))
	for i, p := range v {
		out[i] = s.view(p)
	}
	writeJSON(w, 200, out)
}
func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if !decode(w, r, &in) {
		return
	}
	p, err := in.provider()
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	v, err := s.Store.CreateProvider(p)
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	s.checkProvider(r.Context(), v)
	writeJSON(w, 201, s.view(v))
}
func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if !decode(w, r, &in) {
		return
	}
	p, err := in.provider()
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	p.ID = chi.URLParam(r, "id")
	v, err := s.Store.UpdateProvider(p, in.ExtraHeaders != nil)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Provider not found.")
		return
	}
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	s.checkProvider(r.Context(), v)
	writeJSON(w, 200, s.view(v))
}
func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DeleteProvider(id); err != nil {
		status := 500
		if errors.Is(err, store.ErrProviderInUse) {
			status = 409
		}
		writeError(w, status, err.Error())
		return
	}
	s.Gateway.ForgetModels(id)
	s.checksMu.Lock()
	delete(s.checks, id)
	s.checksMu.Unlock()
	w.WriteHeader(204)
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Provider(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "Provider not found.")
		return
	}
	models, err := s.Gateway.Models(r.Context(), p, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	if r.URL.Query().Get("capability") == "chat" {
		models = gateway.ChatModels(p.Type, models)
	}
	writeJSON(w, 200, models)
}
func (s *Server) allModels(w http.ResponseWriter, r *http.Request) {
	providers, err := s.Store.Providers()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	data := []map[string]any{}
	if active, e := s.Store.ActiveRoutingProfile(); e == nil {
		data = append(data, map[string]any{"id": "smart", "object": "model", "owned_by": "Nexa Smart Routing", "profile": active.Slug})
	}
	if profiles, e := s.Store.RoutingProfiles(); e == nil {
		for _, p := range profiles {
			data = append(data, map[string]any{"id": "smart/" + p.Slug, "object": "model", "owned_by": "Nexa Smart Routing", "profile": p.Slug})
		}
	}
	models, failed := s.Gateway.AllModels(r.Context(), providers)
	if failed > 0 {
		w.Header().Set("X-Nexa-Provider-Errors", strconv.Itoa(failed))
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": append(data, models...)})
}

func (s *Server) routingProfiles(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.RoutingProfiles()
	if e != nil {
		writeError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) createRoutingProfile(w http.ResponseWriter, r *http.Request) {
	var p store.RoutingProfile
	if !decode(w, r, &p) {
		return
	}
	v, e := s.Store.CreateRoutingProfile(p)
	if e != nil {
		writeError(w, 400, store.ErrMessage(e))
		return
	}
	writeJSON(w, 201, v)
}
func (s *Server) updateRoutingProfile(w http.ResponseWriter, r *http.Request) {
	var p store.RoutingProfile
	if !decode(w, r, &p) {
		return
	}
	p.ID = chi.URLParam(r, "id")
	v, e := s.Store.UpdateRoutingProfile(p)
	if e != nil {
		writeError(w, 400, store.ErrMessage(e))
		return
	}
	v.JevAPIKey = ""
	writeJSON(w, 200, v)
}
func (s *Server) deleteRoutingProfile(w http.ResponseWriter, r *http.Request) {
	if e := s.Store.DeleteRoutingProfile(chi.URLParam(r, "id")); e != nil {
		writeError(w, 500, e.Error())
		return
	}
	w.WriteHeader(204)
}
func (s *Server) activateRoutingProfile(w http.ResponseWriter, r *http.Request) {
	if e := s.Store.ActivateRoutingProfile(chi.URLParam(r, "id")); e != nil {
		writeError(w, 404, "Routing profile not found.")
		return
	}
	v, _ := s.Store.RoutingProfile(chi.URLParam(r, "id"))
	v.JevAPIKey = ""
	writeJSON(w, 200, v)
}

// checkRoutingProfile makes a live, uncached Jev call with the profile's key.
func (s *Server) checkRoutingProfile(w http.ResponseWriter, r *http.Request) {
	p, e := s.Store.RoutingProfile(chi.URLParam(r, "id"))
	if e != nil {
		writeError(w, 404, "Routing profile not found.")
		return
	}
	started := time.Now()
	signals, engineErr := s.Gateway.Router.CheckJev(r.Context(), p)
	writeJSON(w, 200, map[string]any{"ok": engineErr == "", "error": engineErr, "complexity": signals.Complexity, "confidence": signals.Confidence, "latency_ms": time.Since(started).Milliseconds()})
}

func (s *Server) evaluateRouting(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProfileID       string   `json:"profile_id"`
		Prompt          string   `json:"prompt"`
		MaxOutputTokens int      `json:"max_output_tokens"`
		Capabilities    []string `json:"capabilities"`
	}
	if !decode(w, r, &in) {
		return
	}
	p, e := s.Store.RoutingProfile(in.ProfileID)
	if e != nil {
		writeError(w, 404, "Routing profile not found.")
		return
	}
	messages := []any{map[string]any{"role": "user", "content": in.Prompt}}
	if slicesContains(in.Capabilities, "vision") {
		messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/image.png"}}}})
	}
	raw, _ := json.Marshal(messages)
	payload := map[string]any{"max_completion_tokens": float64(in.MaxOutputTokens), "messages": messages}
	if slicesContains(in.Capabilities, "tools") {
		payload["tools"] = []any{map[string]any{}}
	}
	if slicesContains(in.Capabilities, "json") {
		payload["response_format"] = map[string]any{"type": "json_object"}
	}
	d, e := s.Gateway.Router.Decide(r.Context(), p, raw, payload)
	if e != nil {
		writeError(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, d)
}
func slicesContains(v []string, x string) bool {
	for _, s := range v {
		if s == x {
			return true
		}
	}
	return false
}
func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TraceQuery{Search: q.Get("search"), Status: q.Get("status"), ProviderID: q.Get("provider"), Model: q.Get("model")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	f.From, _ = time.Parse(time.RFC3339, q.Get("from"))
	f.To, _ = time.Parse(time.RFC3339, q.Get("to"))
	v, total, err := s.Store.Traces(f)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"data": v, "total": total})
}
func (s *Server) trace(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Trace(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "Trace not found.")
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) playground(w http.ResponseWriter, r *http.Request) {
	s.Gateway.ChatCompletions(w, gateway.Annotate(r, "playground", who(r).Username))
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Users()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password, Role string }
	if !decode(w, r, &in) {
		return
	}
	v, err := s.Store.CreateUser(in.Username, in.Password, in.Role)
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	writeJSON(w, 201, v)
}
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if who(r).UserID == id && in.Role != "" {
		writeError(w, 400, "You cannot change your own role.")
		return
	}
	v, err := s.Store.UpdateUser(id, in.Role, in.Password, "")
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "User not found.")
		return
	}
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if who(r).UserID == chi.URLParam(r, "id") {
		writeError(w, 400, "You cannot delete your current account.")
		return
	}
	if err := s.Store.DeleteUser(chi.URLParam(r, "id")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) prices(w http.ResponseWriter, r *http.Request) {
	builtin := []store.Price{}
	for model, p := range gateway.BuiltinPrices() {
		builtin = append(builtin, store.Price{Model: model, Input: p[0], Output: p[1]})
	}
	writeJSON(w, 200, map[string]any{"custom": s.Store.Prices(), "builtin": builtin})
}
func (s *Server) setPrice(w http.ResponseWriter, r *http.Request) {
	var p store.Price
	if !decode(w, r, &p) {
		return
	}
	if err := s.Store.SetPrice(p); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) deletePrice(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeletePrice(r.URL.Query().Get("model")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) apiKeys(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.APIKeys()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	k, plain, err := s.Store.CreateAPIKey(in.Name)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"key": plain, "api_key": k, "warning": "This key is shown once. Save it now."})
}
func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteAPIKey(chi.URLParam(r, "id")); err != nil {
		writeError(w, 404, "API key not found.")
		return
	}
	w.WriteHeader(204)
}

// rotateMaster signs out every master session, then re-issues one for the caller.
func (s *Server) rotateMaster(w http.ResponseWriter, r *http.Request) {
	if !who(r).Master {
		writeError(w, 403, "Only the master administrator can rotate this key.")
		return
	}
	key, err := s.Store.ResetMasterKey()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if token, err := s.Store.CreateSession("", true); err == nil {
		s.setSession(w, token)
	}
	writeJSON(w, 200, map[string]string{"master_key": key, "warning": "This key is shown once. Save it now."})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		writeError(w, 400, "Invalid JSON body.")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}
