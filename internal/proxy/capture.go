// Capture-mode proxying for LLM endpoints: /v1/chat/completions and
// /v1/responses.
//
// Flow: resolve the gateway model via the registry → rewrite alias to the
// upstream model ID → forward → tee the response back to the client while
// parsing it for usage (SSE chunks or a full body) → rewrite the model back
// to the alias in every chunk → record the usage event and the reassembled
// transcript.
//
// Upstream failures pass through verbatim (no retry, no failover); usage
// and transcript recording failures are logged and never affect the
// client's response.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/api"
	"github.com/trfdeer/toll/internal/cost"
	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/store"
	"github.com/trfdeer/toll/internal/wire"
)

// Capture proxies model-aware endpoints with usage capture and transcript
// persistence.
type Capture struct {
	store  *store.Store
	logger *log.Logger
	client *http.Client
	reqSeq atomic.Uint64 // correlation id for the request/response log pair
}

func NewCapture(st *store.Store, logger *log.Logger) *Capture {
	return &Capture{
		store:  st,
		logger: logger,
		client: &http.Client{
			// No client-level timeout: streams can run for minutes. The
			// request context (client disconnect) governs cancellation.
			Transport: sharedTransport(),
		},
	}
}

// pending carries the transcript bookkeeping from request setup to response
// completion.
type pending struct {
	id           int64 // 0 = transcript creation failed, skip completion
	route        *store.Route
	gatewayModel string
	keyID        int64
	isResponses  bool      // responses wire format (vs chat/completions)
	reqID        string    // log correlation id
	start        time.Time // request arrival, for response duration
}

func (c *Capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := c.nextReqID()
	start := time.Now()

	var keyID int64
	if vk := api.VirtualKeyFrom(r.Context()); vk != nil {
		keyID = vk.ID
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		c.reject(w, reqID, http.StatusBadRequest, "unreadable request body")
		return
	}
	c.logger.Debug("request body", "req_id", reqID, "path", r.URL.Path, "body", string(body))

	model, stream, err := wire.ParseRequest(body)
	if err != nil {
		c.reject(w, reqID, http.StatusBadRequest, err.Error())
		return
	}
	c.logger.Info("request", "req_id", reqID, "method", r.Method, "path", r.URL.Path,
		"model", model, "stream", stream, "key_id", keyID, "bytes", len(body))

	route, err := c.store.RouteForModel(r.Context(), model)
	if err != nil {
		if errors.Is(err, store.ErrModelNotFound) {
			c.reject(w, reqID, http.StatusNotFound,
				fmt.Sprintf("model %q is not available on this gateway", model))
			return
		}
		c.logger.Error("model routing failed", "model", model, "err", err)
		c.reject(w, reqID, http.StatusInternalServerError, "registry unavailable")
		return
	}

	// Per-key model visibility, gated on the upstream the model was
	// discovered from (never a prefix of its ID). A disallowed model is
	// indistinguishable from an unknown one (no existence leak).
	if vk := api.VirtualKeyFrom(r.Context()); vk != nil &&
		!keys.Allows(route.UpstreamName, model, vk.AllowAll, vk.Rules) {
		c.reject(w, reqID, http.StatusNotFound,
			fmt.Sprintf("model %q is not available on this gateway", model))
		return
	}

	p := &pending{
		route:        route,
		gatewayModel: model,
		keyID:        keyID,
		isResponses:  strings.HasSuffix(r.URL.Path, "/responses"),
		reqID:        reqID,
		start:        start,
	}
	c.startTranscript(r, body, p)

	upBody, err := wire.RewriteModel(body, route.UpstreamModelID)
	if err != nil {
		c.reject(w, reqID, http.StatusBadRequest, err.Error())
		return
	}

	target := joinURL(route.BaseURL, r.URL.Path)
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(upBody))
	if err != nil {
		c.reject(w, reqID, http.StatusInternalServerError, "proxy setup failed")
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+route.APIKey)
	upReq.Header.Set("Accept", r.Header.Get("Accept"))
	upReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	if r.Header.Get("Accept") == "" {
		upReq.Header.Set("Accept", "application/json")
	}

	resp, err := c.client.Do(upReq)
	if err != nil {
		c.logger.Error("upstream request failed", "req_id", reqID, "upstream", route.UpstreamName, "err", err)
		c.reject(w, reqID, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer resp.Body.Close()

	// Error and other non-200 responses pass through byte-for-byte; the
	// transcript keeps the status and error body for inspection.
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		c.finish(r, p, string(data), resp.StatusCode, nil, nil)
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		c.logResponse(p, resp.StatusCode, len(data), nil, 0)
		if len(data) > 0 {
			c.logger.Debug("response body", "req_id", p.reqID, "status", resp.StatusCode, "body", string(data))
		}
		return
	}

	if stream {
		c.streamResponse(w, r, resp, p)
		return
	}
	c.bufferedResponse(w, r, resp, p)
}

// startTranscript records the request half; failures only disable
// transcript completion for this request.
func (c *Capture) startTranscript(r *http.Request, body []byte, p *pending) {
	convID := store.ConversationID(body, r.Header.Get("X-Toll-Conversation-Id"))
	if err := c.store.EnsureConversation(r.Context(), convID, p.keyID); err != nil {
		c.logger.Warn("conversation persistence failed", "err", err)
		return
	}
	id, err := c.store.CreateTranscript(r.Context(), convID, p.gatewayModel, p.route.UpstreamModelID, string(body))
	if err != nil {
		c.logger.Warn("transcript persistence failed", "err", err)
		return
	}
	p.id = id
}

// bufferedResponse handles non-streaming success: read the full body,
// extract usage, rewrite the model back to the alias, forward.
func (c *Capture) bufferedResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, p *pending) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.logger.Warn("upstream response truncated", "err", err)
	}
	out := data
	if out2, err := wire.RewriteModel(data, p.gatewayModel); err == nil {
		out = out2
	} else {
		c.logger.Warn("could not rewrite model in response body", "err", err)
	}

	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)

	var usage *wire.Usage
	var raw json.RawMessage
	if u, ru, ok := wire.ExtractUsageFromBytes(data); ok {
		usage, raw = &u, ru
	}
	c.finish(r, p, string(out), resp.StatusCode, usage, raw)

	c.logResponse(p, resp.StatusCode, len(out), usage, 0)
	c.logger.Debug("response body", "req_id", p.reqID, "status", resp.StatusCode, "body", string(out))
}

// streamResponse tees an SSE stream: every data line is parsed, its model
// rewritten to the alias, its usage captured and the assistant message
// reassembled (chat deltas / the full response object for responses
// format); everything is flushed to the client immediately.
func (c *Capture) streamResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, p *pending) {
	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)

	c.logger.Debug("stream start", "req_id", p.reqID, "model", p.gatewayModel,
		"upstream", p.route.UpstreamName)

	flusher, canFlush := w.(http.Flusher)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20) // chunks can be large (tool calls, long deltas)

	var assembler *wire.ChatStreamAssembler
	var responseObj json.RawMessage // responses format: full response object
	var usage *wire.Usage
	var rawUsage json.RawMessage
	var chunks, bytesOut int

	for scanner.Scan() {
		line := scanner.Bytes()
		out := line
		if payload, ok := sseData(line); ok && !bytes.Equal(payload, []byte("[DONE]")) {
			var obj map[string]json.RawMessage
			if json.Unmarshal(payload, &obj) == nil {
				obj["model"], _ = json.Marshal(p.gatewayModel) // upstream ID → alias
				if u, raw, ok := wire.ExtractUsage(obj); ok {
					usage = &u
					rawUsage = raw
				}
				if !p.isResponses {
					if assembler == nil {
						assembler = &wire.ChatStreamAssembler{}
					}
					assembler.Feed(obj)
				} else if ro := wire.ResponseObjectFrom(obj); ro != nil {
					responseObj = ro
				}
				if rewritten, err := json.Marshal(obj); err == nil {
					out = append([]byte("data: "), rewritten...)
				}
			}
		}
		written := append(out, '\n')
		if _, err := w.Write(written); err != nil {
			c.logger.Warn("stream aborted by client", "req_id", p.reqID,
				"chunks", chunks, "bytes", bytesOut, "err", err)
			return // client went away
		}
		bytesOut += len(written)
		if len(out) > 0 {
			chunks++
			c.logger.Debug("stream chunk", "req_id", p.reqID, "n", chunks, "data", string(out))
		}
		if canFlush {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil && r.Context().Err() == nil {
		c.logger.Warn("SSE stream terminated early", "req_id", p.reqID,
			"upstream", p.route.UpstreamName, "err", err)
	}

	// Reassembled response for the transcript.
	responseJSON := ""
	switch {
	case p.isResponses && responseObj != nil:
		responseJSON = string(responseObj)
	case assembler != nil:
		if assembled, ok := assembler.Build(p.gatewayModel); ok {
			responseJSON = string(assembled)
		}
	}

	c.finish(r, p, responseJSON, http.StatusOK, usage, rawUsage)

	c.logResponse(p, http.StatusOK, bytesOut, usage, chunks)
	if responseJSON != "" {
		c.logger.Debug("response body", "req_id", p.reqID, "reassembled", true, "body", responseJSON)
	}
}

// logResponse emits the single completion line for one proxied response.
// Optional fields (usage, chunks) are omitted when absent; chunks is 0 for
// non-streaming responses.
func (c *Capture) logResponse(p *pending, status, bytes int, u *wire.Usage, chunks int) {
	fields := []any{
		"req_id", p.reqID,
		"status", status,
		"model", p.gatewayModel,
		"upstream", p.route.UpstreamName,
		"duration", time.Since(p.start).Round(time.Millisecond),
		"bytes", bytes,
	}
	if chunks > 0 {
		fields = append(fields, "chunks", chunks)
	}
	if u != nil {
		fields = append(fields,
			"prompt_tokens", u.PromptTokens,
			"completion_tokens", u.CompletionToken,
			"cached_tokens", u.CachedTokens,
			"reasoning_tokens", u.ReasoningTokens)
	}
	c.logger.Info("response", fields...)
}

// nextReqID returns a process-unique id correlating a request log line with
// its response log line.
func (c *Capture) nextReqID() string {
	return fmt.Sprintf("req-%06d", c.reqSeq.Add(1))
}

// reject logs and answers a request that never reached the upstream.
func (c *Capture) reject(w http.ResponseWriter, reqID string, status int, msg string) {
	c.logger.Info("request rejected", "req_id", reqID, "status", status, "reason", msg)
	c.errorJSON(w, status, msg)
}

// finish persists usage + completes the transcript, on a detached context
// (the request's may already be cancelled by a client disconnect). Never
// fails the request.
func (c *Capture) finish(r *http.Request, p *pending, responseJSON string, status int, u *wire.Usage, raw json.RawMessage) {
	recCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ev := store.UsageEvent{
		KeyID:         p.keyID,
		UpstreamID:    p.route.UpstreamID,
		GatewayModel:  p.gatewayModel,
		UpstreamModel: p.route.UpstreamModelID,
	}
	if u != nil {
		ev.PromptTokens = u.PromptTokens
		ev.CompletionToken = u.CompletionToken
		ev.CachedTokens = u.CachedTokens
		ev.ReasoningTokens = u.ReasoningTokens
		ev.UpstreamCostUSD = u.UpstreamCostUSD
		ev.CostUSD = cost.Compute(cost.TokenCounts{
			PromptTokens:    u.PromptTokens,
			CompletionToken: u.CompletionToken,
			CachedTokens:    u.CachedTokens,
		}, cost.FromMetadata([]byte(p.route.Metadata)))
		if cost.Diverges(ev.CostUSD, ev.UpstreamCostUSD, 0.05) {
			c.logger.Debug("computed cost diverges from upstream-reported",
				"model", p.gatewayModel, "computed", ev.CostUSD, "upstream", ev.UpstreamCostUSD)
		}
		if len(raw) > 0 {
			ev.RawUsage = string(raw)
		}
		if err := c.store.RecordUsage(recCtx, ev); err != nil {
			c.logger.Error("usage recording failed", "err", err)
		}
	}

	if p.id != 0 {
		if err := c.store.CompleteTranscript(recCtx, p.id, responseJSON, status, ev); err != nil {
			c.logger.Error("transcript completion failed", "err", err)
		}
	}
}

func (c *Capture) errorJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error"},
	})
	_, _ = w.Write(body)
}

// sseData reports whether the line is a data field, returning its payload.
func sseData(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	return bytes.TrimSpace(line[len("data:"):]), true
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// joinURL maps a gateway /v1/... path onto the upstream base URL
// (…/v1/chat/completions → {base}/chat/completions).
func joinURL(base, gatewayPath string) string {
	b := strings.TrimSuffix(base, "/")
	rest := strings.TrimPrefix(gatewayPath, "/v1")
	if rest == "" {
		return b
	}
	return b + rest
}
