package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/evolvue/nexa-gateway/internal/store"
)

// judgeClient allows longer than routing decisions: the judge reads a whole answer
// and runs after the response has already been sent, so it never delays a user.
var judgeClient = &http.Client{Timeout: 20 * time.Second}

// JevKey resolves a Jev key: the explicit key first, then JEV_API_KEY, then TYPESAFE_API_KEY.
func JevKey(explicit string) string {
	return jevKey(store.RoutingProfile{JevAPIKey: explicit})
}

// Judge asks Jev whether a response is a good answer to the conversation.
// It returns "liked" or "disliked" with Jev's confidence in that verdict.
func (e *Engine) Judge(ctx context.Context, key string, conversation json.RawMessage, response string) (string, float64, error) {
	if key == "" {
		return "", 0, errors.New("JEV_API_KEY is not configured")
	}
	state := JevInput(conversation) + "\n\nASSISTANT RESPONSE:\n" + tail(strings.TrimSpace(response), jevInputChars)
	body := map[string]any{"state": state, "model": "jev-latest", "questions": map[string]any{"quality": map[string]any{
		"type":         "choice",
		"instructions": "Judge only the final ASSISTANT RESPONSE: would the person who asked the latest request be satisfied with it?",
		"criteria": map[string]string{
			"liked":    "Correct, relevant and helpful; answers what was asked and follows the instructions",
			"disliked": "Wrong, unhelpful, off-topic, incomplete, unsafe, or ignores the instructions",
		},
	}}}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", jevURL, bytes.NewReader(raw))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := judgeClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("Jev request failed: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("Jev returned %d", resp.StatusCode)
	}
	var out struct {
		Answers map[string]struct {
			Choice        string             `json:"choice"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
	}
	if json.Unmarshal(b, &out) != nil {
		return "", 0, errors.New("invalid Jev response")
	}
	q := out.Answers["quality"]
	if q.Choice != "liked" && q.Choice != "disliked" {
		return "", 0, errors.New("Jev returned an invalid verdict")
	}
	confidence := q.Confidence
	if confidence == 0 {
		confidence = q.Probabilities[q.Choice]
	}
	return q.Choice, confidence, nil
}
