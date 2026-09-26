package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/evolvue/nexa-gateway/internal/store"
)

// admit applies the calling key's model allow-list, monthly budget and
// per-minute token and request limits. Counters live in Redis, so the limits
// hold across every Nexa instance. The master key and dashboard sessions are
// unlimited. If Redis cannot be reached the request is allowed (fail open) and
// the outage is logged: a limiter outage should not take the gateway down.
func (g *Gateway) admit(w http.ResponseWriter, r *http.Request, info CallInfo, t *store.Trace, model string) bool {
	if info.KeyID == "" {
		return true
	}
	lim := info.Limits
	reject := func(status int, msg, typ string, retry time.Duration) bool {
		if retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		}
		t.StatusCode, t.Error = status, msg
		writeOpenAIError(w, status, msg, typ)
		return false
	}
	if !lim.AllowsModel(model) {
		return reject(403, fmt.Sprintf("This API key is not allowed to use model %q.", model), "permission_error", 0)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	now := time.Now().UTC()
	limiterDown := func(err error) bool {
		slog.Error("rate limiter unavailable; allowing request", "key", info.KeyName, "error", err.Error())
		return true
	}
	if lim.MonthlyBudgetUSD > 0 {
		spent, err := g.Store.MonthSpend(ctx, info.KeyID, now)
		if err != nil {
			return limiterDown(err)
		}
		if spent >= lim.MonthlyBudgetUSD {
			next := store.MonthStart(now).AddDate(0, 1, 0)
			return reject(429, fmt.Sprintf("This API key's monthly budget of $%.2f is used up ($%.4f spent). It resets on %s.", lim.MonthlyBudgetUSD, spent, next.Format("2 Jan 2006")), "budget_exceeded", next.Sub(now))
		}
		w.Header().Set("X-Nexa-Key-Budget-Remaining", strconv.FormatFloat(lim.MonthlyBudgetUSD-spent, 'f', 6, 64))
	}
	if lim.TokensPerMin > 0 {
		used, oldest, err := g.Store.TokensLastMinute(ctx, info.KeyID, now)
		if err != nil {
			return limiterDown(err)
		}
		if used >= lim.TokensPerMin {
			return reject(429, fmt.Sprintf("This API key is limited to %d tokens per minute (%d used in the last minute).", lim.TokensPerMin, used), "rate_limit_exceeded", oldest.Add(time.Minute).Sub(now))
		}
	}
	if lim.RequestsPerMin > 0 {
		ok, count, oldest, err := g.Store.AdmitRequest(ctx, info.KeyID, lim.RequestsPerMin, now)
		if err != nil {
			return limiterDown(err)
		}
		if !ok {
			return reject(429, fmt.Sprintf("This API key is limited to %d requests per minute.", lim.RequestsPerMin), "rate_limit_exceeded", oldest.Add(time.Minute).Sub(now))
		}
		w.Header().Set("X-Nexa-Key-Requests-Remaining", strconv.Itoa(lim.RequestsPerMin-count))
	}
	return true
}

// recordUsage adds a finished request's cost and tokens to its key's shared counters.
func (g *Gateway) recordUsage(t *store.Trace) {
	if t.APIKeyID == "" || (t.CostUSD == 0 && t.TotalTokens == 0) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	g.Store.RecordKeyUsage(ctx, t.APIKeyID, t.CostUSD, t.TotalTokens, time.Now().UTC())
}
