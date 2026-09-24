package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
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
}

type principal struct {
	UserID string
	Master bool
}

func New(s *store.Store, log *slog.Logger, secureCookies bool) http.Handler {
	x := &Server{Store: s, Gateway: gateway.New(s), Log: log, SecureCookies: secureCookies}
	r := chi.NewRouter()
	r.Use(middleware.RealIP, middleware.RequestID, middleware.Recoverer, x.securityHeaders, x.accessLog)
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
			r.Get("/stats", x.stats)
			r.Get("/providers", x.providers)
			r.Post("/providers", x.createProvider)
			r.Put("/providers/{id}", x.updateProvider)
			r.Delete("/providers/{id}", x.deleteProvider)
			r.Get("/providers/{id}/models", x.models)
			r.Get("/traces", x.traces)
			r.Get("/traces/{id}", x.trace)
			r.Get("/users", x.users)
			r.Post("/users", x.createUser)
			r.Delete("/users/{id}", x.deleteUser)
			r.Post("/master-key/rotate", x.rotateMaster)
			r.Post("/playground/chat", x.Gateway.ChatCompletions)
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

func (s *Server) gatewayAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token != "" && s.Store.ValidateMaster(token) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, 401, "A valid Nexa master key is required.")
	})
}
func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("nexa_session")
		if err != nil {
			writeError(w, 401, "Sign in to continue.")
			return
		}
		uid, master, err := s.Store.Session(c.Value)
		if err != nil {
			writeError(w, 401, "Your session expired. Sign in again.")
			return
		}
		r.Header.Set("X-Nexa-User", uid)
		if master {
			r.Header.Set("X-Nexa-Master", "1")
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

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ MasterKey, Username, Password string }
	if !decode(w, r, &in) {
		return
	}
	master := false
	uid := ""
	var username, role string
	if in.MasterKey != "" && s.Store.ValidateMaster(in.MasterKey) {
		master = true
		username = "Master administrator"
		role = "owner"
	} else if in.Username != "" {
		u, err := s.Store.LoginUser(in.Username, in.Password)
		if err != nil {
			writeError(w, 401, "Invalid username or password.")
			return
		}
		uid = u.ID
		username = u.Username
		role = u.Role
	} else {
		writeError(w, 401, "Invalid master key.")
		return
	}
	session, err := s.Store.CreateSession(uid, master)
	if err != nil {
		writeError(w, 500, "Could not create session.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "nexa_session", Value: session, Path: "/", HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 7 * 24 * 3600})
	writeJSON(w, 200, map[string]any{"username": username, "role": role, "master": master})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("nexa_session"); err == nil {
		s.Store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "nexa_session", Value: "", Path: "/", HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(204)
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Nexa-Master") == "1" {
		writeJSON(w, 200, map[string]any{"username": "Master administrator", "role": "owner", "master": true})
		return
	}
	users, _ := s.Store.Users()
	for _, u := range users {
		if u.ID == r.Header.Get("X-Nexa-User") {
			writeJSON(w, 200, map[string]any{"username": u.Username, "role": u.Role, "master": false})
			return
		}
	}
	writeError(w, 401, "User not found.")
}
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	providers, _ := s.Store.Providers()
	writeJSON(w, 200, map[string]any{"name": "Nexa Gateway", "version": "0.1.0", "providers": len(providers)})
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
	Name    string `json:"name"`
	Slug    string `json:"slug"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Enabled *bool  `json:"enabled"`
}

func (in providerInput) provider() store.Provider {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	return store.Provider{Name: in.Name, Slug: in.Slug, Type: in.Type, BaseURL: in.BaseURL, APIKey: in.APIKey, Enabled: enabled}
}
func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Providers()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if !decode(w, r, &in) {
		return
	}
	p := in.provider()
	if p.BaseURL == "" {
		p.BaseURL = defaultBase(p.Type)
	}
	v, err := s.Store.CreateProvider(p)
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	v.APIKey = ""
	writeJSON(w, 201, v)
}
func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if !decode(w, r, &in) {
		return
	}
	p := in.provider()
	p.ID = chi.URLParam(r, "id")
	if p.BaseURL == "" {
		p.BaseURL = defaultBase(p.Type)
	}
	v, err := s.Store.UpdateProvider(p)
	if err != nil {
		writeError(w, 400, store.ErrMessage(err))
		return
	}
	v.APIKey = ""
	writeJSON(w, 200, v)
}
func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteProvider(chi.URLParam(r, "id")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Provider(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "Provider not found.")
		return
	}
	models, err := s.Gateway.Models(r.Context(), p)
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
	failed := 0
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		models, err := s.Gateway.Models(r.Context(), p)
		if err != nil {
			failed++
			continue
		}
		for _, model := range models {
			id, _ := model["id"].(string)
			if id == "" {
				id, _ = model["name"].(string)
			}
			if id == "" {
				continue
			}
			model["id"] = p.Slug + "/" + id
			model["object"] = "model"
			model["owned_by"] = p.Name
			data = append(data, model)
		}
	}
	if failed > 0 {
		w.Header().Set("X-Nexa-Provider-Errors", strconv.Itoa(failed))
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}
func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	v, total, err := s.Store.Traces(limit, offset, r.URL.Query().Get("search"), r.URL.Query().Get("status"))
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
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Nexa-User") == chi.URLParam(r, "id") {
		writeError(w, 400, "You cannot delete your current account.")
		return
	}
	if err := s.Store.DeleteUser(chi.URLParam(r, "id")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}
func (s *Server) rotateMaster(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Nexa-Master") != "1" {
		writeError(w, 403, "Only the master administrator can rotate this key.")
		return
	}
	key, err := s.Store.ResetMasterKey()
	if err != nil {
		writeError(w, 500, err.Error())
		return
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
