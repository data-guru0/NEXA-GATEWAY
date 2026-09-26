package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/evolvue/nexa-gateway/internal/store"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// Endpoint serves an OpenAI-compatible endpoint other than chat completions
// (embeddings, responses, moderations, images, audio) by forwarding it to the
// provider named in the model ("openai/text-embedding-3-small"). Bodies may be
// JSON or multipart (audio uploads); responses (JSON, SSE or binary audio) are
// streamed back unchanged and every call is traced with usage and cost.
func (g *Gateway) Endpoint(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info := callInfo(r)
		t := &store.Trace{ID: uuid.NewString(), RequestID: middleware.GetReqID(r.Context()), Source: info.Source, APIKeyName: info.KeyName, APIKeyID: info.KeyID, ProviderName: "—", Status: "error"}
		mergeMetadata(t, "endpoint", "/v1"+path)
		w.Header().Set("X-Nexa-Trace-Id", t.ID)
		defer g.finish(t, info.Start)
		fail := func(status int, msg string) {
			t.StatusCode, t.Error = status, msg
			writeOpenAIError(w, status, msg, "invalid_request_error")
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 26<<20))
		if err != nil {
			fail(400, "Request body is too large (25 MB maximum) or invalid.")
			return
		}
		mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		multipartBody := strings.HasPrefix(mediaType, "multipart/")
		var payload map[string]any
		var parts []formPart
		model := ""
		if multipartBody {
			parts, err = readForm(raw, params["boundary"])
			if err != nil {
				fail(400, "Could not read the multipart form: "+err.Error())
				return
			}
			for _, p := range parts {
				if p.name == "model" && p.filename == "" {
					model = strings.TrimSpace(string(p.data))
				}
			}
			t.Prompt = formSummary(parts)
		} else {
			if payload, err = decodePayload(raw); err != nil {
				fail(400, "The request body must be JSON.")
				return
			}
			model, _ = payload["model"].(string)
			t.Prompt = inputSummary(payload)
			t.Params = traceParams(payload)
			t.Stream, _ = payload["stream"].(bool)
		}
		t.Request = truncate(t.Prompt, maxTraceBody)
		if model == "" {
			fail(400, "model is required.")
			return
		}
		t.Model = model
		if model == "smart" || strings.HasPrefix(model, "smart/") || strings.HasPrefix(model, "feedback/") {
			fail(400, "Smart routing and feedback loops serve /v1/chat/completions only. Use provider/model here.")
			return
		}
		if !g.admit(w, r, info, t, model) {
			return
		}
		p, upstreamModel, err := g.resolve(model, strings.TrimSpace(r.Header.Get("X-Nexa-Provider")))
		if err != nil {
			fail(404, err.Error())
			return
		}
		t.ProviderID, t.ProviderName, t.Model = p.ID, p.Name, upstreamModel
		if strings.EqualFold(p.Type, "anthropic") {
			fail(400, fmt.Sprintf("Anthropic does not offer /v1%s. Use an OpenAI-compatible provider for this endpoint.", path))
			return
		}
		var body []byte
		contentType := "application/json"
		if multipartBody {
			body, contentType = writeForm(parts, upstreamModel)
		} else {
			payload["model"] = upstreamModel
			body, _ = json.Marshal(payload)
		}
		upstreamStarted := msSince(info.Start)
		resp, err := g.send(r.Context(), func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(p.BaseURL, "/")+path, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
			copyRequestHeaders(req.Header, r.Header)
			applyHeaders(req.Header, p.Headers)
			return req, nil
		}, 1)
		if err != nil {
			_ = upstreamFailure(w, t, err)
			return
		}
		defer resp.Body.Close()
		copyResponseHeaders(w.Header(), resp.Header)
		if cl := resp.Header.Get("Content-Length"); cl != "" && !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
			w.Header().Set("Content-Length", cl)
		}
		w.Header().Set("X-Nexa-Provider", p.Slug)
		w.WriteHeader(resp.StatusCode)
		t.StatusCode = resp.StatusCode
		respType := resp.Header.Get("Content-Type")
		textual := strings.Contains(respType, "json") || strings.Contains(respType, "event-stream") || strings.HasPrefix(respType, "text/")
		captured := &limitedBuffer{max: maxTraceBody}
		size, copyErr := streamCopy(w, resp.Body, captured, textual, func() { t.TTFTMS = msSince(info.Start) })
		t.UpstreamMS = msSince(info.Start) - upstreamStarted
		body = captured.Bytes()
		if resp.StatusCode >= 300 {
			t.Error = truncate(errorMessage(body, resp.StatusCode), 1000)
			t.Response = truncate(string(body), maxTraceBody)
			return
		}
		if copyErr != nil {
			t.Error = "response interrupted: " + copyErr.Error()
			return
		}
		t.Status = "success"
		if !textual {
			t.Response = fmt.Sprintf("%s · %.1f KB", respType, float64(size)/1024)
			return
		}
		u := findUsage(body, strings.Contains(respType, "event-stream"))
		t.InputTokens, t.OutputTokens, t.TotalTokens = u.in, u.out, u.total
		if t.TotalTokens == 0 {
			t.TotalTokens = t.InputTokens + t.OutputTokens
		}
		t.Response = responseSummary(path, body)
	}
}

type formPart struct {
	name, filename, contentType string
	data                        []byte
}

func readForm(raw []byte, boundary string) ([]formPart, error) {
	if boundary == "" {
		return nil, fmt.Errorf("missing boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	parts := []formPart{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return parts, nil
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, err
		}
		parts = append(parts, formPart{p.FormName(), p.FileName(), p.Header.Get("Content-Type"), data})
	}
}

// writeForm rebuilds the form with the provider's own model id.
func writeForm(parts []formPart, model string) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.filename != "" {
			h := make(map[string][]string)
			h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name=%q; filename=%q`, p.name, p.filename)}
			ct := p.contentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			h["Content-Type"] = []string{ct}
			fw, _ := mw.CreatePart(h)
			_, _ = fw.Write(p.data)
			continue
		}
		value := p.data
		if p.name == "model" {
			value = []byte(model)
		}
		_ = mw.WriteField(p.name, string(value))
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func formSummary(parts []formPart) string {
	out := []string{}
	for _, p := range parts {
		if p.filename != "" {
			out = append(out, fmt.Sprintf("%s: %s (%.1f KB)", p.name, p.filename, float64(len(p.data))/1024))
		} else if p.name != "model" {
			out = append(out, fmt.Sprintf("%s: %s", p.name, truncate(string(p.data), 500)))
		}
	}
	return strings.Join(out, "\n")
}

// inputSummary is the readable input for the trace: the text that was embedded,
// moderated, spoken or drawn.
func inputSummary(payload map[string]any) string {
	for _, k := range []string{"input", "prompt", "instructions"} {
		switch v := payload[k].(type) {
		case string:
			return truncate(v, 64<<10)
		case []any:
			if len(v) == 1 {
				if s, ok := v[0].(string); ok {
					return truncate(s, 64<<10)
				}
			}
			b, _ := json.Marshal(v)
			return truncate(fmt.Sprintf("%d inputs: %s", len(v), b), 64<<10)
		}
	}
	return ""
}

// streamCopy forwards the body as it arrives (flushing events and audio chunks)
// and keeps textual bodies for the trace.
func streamCopy(w http.ResponseWriter, body io.Reader, captured *limitedBuffer, keep bool, first func()) (int64, error) {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(body, 32<<10)
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if total == 0 {
				first()
			}
			total += int64(n)
			if keep {
				_, _ = captured.Write(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

type endpointUsage struct{ in, out, total int }

// findUsage reads token usage from a JSON body or, for event streams, from the
// last event that carries it (the Responses API reports it in response.completed).
func findUsage(body []byte, sse bool) endpointUsage {
	parse := func(b []byte) (endpointUsage, bool) {
		var x struct {
			Usage    *usageFields `json:"usage"`
			Response struct {
				Usage *usageFields `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(b, &x) != nil {
			return endpointUsage{}, false
		}
		u := x.Usage
		if u == nil {
			u = x.Response.Usage
		}
		if u == nil {
			return endpointUsage{}, false
		}
		return endpointUsage{u.PromptTokens + u.InputTokens, u.CompletionTokens + u.OutputTokens, u.TotalTokens}, true
	}
	if !sse {
		u, _ := parse(body)
		return u
	}
	var last endpointUsage
	for _, line := range bytes.Split(body, []byte("\n")) {
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			if u, ok := parse(bytes.TrimSpace(data)); ok {
				last = u
			}
		}
	}
	return last
}

type usageFields struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// responseSummary keeps traces readable: vectors and images are summarised,
// Responses API output is reduced to its text.
func responseSummary(path string, body []byte) string {
	switch {
	case path == "/embeddings":
		var x struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &x) == nil && len(x.Data) > 0 {
			return fmt.Sprintf("%d embedding(s) · %d dimensions", len(x.Data), len(x.Data[0].Embedding))
		}
	case strings.HasPrefix(path, "/images/"):
		var x struct {
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(body, &x) == nil {
			return fmt.Sprintf("%d image(s) generated", len(x.Data))
		}
	case path == "/responses":
		var x struct {
			Output []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(body, &x) == nil {
			var b strings.Builder
			for _, o := range x.Output {
				for _, c := range o.Content {
					b.WriteString(c.Text)
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	return truncate(string(body), maxTraceBody)
}

func errorMessage(body []byte, status int) string {
	var x struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &x) == nil && x.Error.Message != "" {
		return x.Error.Message
	}
	if len(body) > 0 {
		return string(body)
	}
	return fmt.Sprintf("provider returned %d", status)
}
