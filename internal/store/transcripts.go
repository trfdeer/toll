package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrTranscriptNotFound is returned when a transcript id does not exist.
var ErrTranscriptNotFound = errors.New("transcript not found")

// ConversationID derives the conversation a request belongs to:
//   - the X-Toll-Conversation-Id header value, when the client provides one;
//   - otherwise a deterministic digest of the request's message payload, so
//     retries and exact replays of the same request group together.
//
// Follow-up requests that extend a prior exchange start a new conversation
// unless the client opts in via the header — a documented MVP limitation;
// the schema carries no assumption about how IDs are derived.
func ConversationID(requestBody []byte, header string) string {
	if header != "" {
		return "conv-h-" + sanitizeID(header)
	}
	h := sha256.Sum256(requestBody)
	return "conv-" + hex.EncodeToString(h[:16])
}

// sanitizeID strips anything outside [a-zA-Z0-9-_] and caps the length so a
// client-supplied header is safe to persist and render.
func sanitizeID(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	if len(out) == 0 {
		out = append(out, 'u')
	}
	return string(out)
}

// EnsureConversation creates the conversation row when it doesn't exist.
func (s *Store) EnsureConversation(ctx context.Context, id string, keyID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversations (id, key_id) VALUES (?, ?)
		ON CONFLICT(id) DO NOTHING`, id, keyID)
	if err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}
	return nil
}

// CreateTranscript records the request half of a transcript and returns its
// id. Metadata lives in the main database; the prompt body is written to the
// content database when prompt storage is enabled.
func (s *Store) CreateTranscript(ctx context.Context, conversationID string, gatewayModel, upstreamModel, requestJSON string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO transcripts (conversation_id, gateway_model, upstream_model)
		VALUES (?, ?, ?)`,
		conversationID, gatewayModel, upstreamModel)
	if err != nil {
		return 0, fmt.Errorf("create transcript: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create transcript: %w", err)
	}
	s.putContent(ctx, id, requestJSON, "")
	return id, nil
}

// CompleteTranscript fills in the response half after the upstream finishes
// (or the stream ends).
func (s *Store) CompleteTranscript(ctx context.Context, id int64, responseJSON string, status int, e UsageEvent) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE transcripts SET
			status = ?,
			prompt_tokens = ?, completion_tokens = ?, cached_tokens = ?,
			cost_usd = ?, completed_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		status,
		e.PromptTokens, e.CompletionToken, e.CachedTokens,
		e.CostUSD, id)
	if err != nil {
		return fmt.Errorf("complete transcript %d: %w", id, err)
	}
	s.putContent(ctx, id, "", responseJSON)
	return nil
}

// putContent upserts a transcript body in the content database. It is a no-op
// when prompt storage is disabled or both halves are empty. Failures are
// deliberately swallowed: body persistence must never fail the request, and
// the metadata row in the main database is already durable.
func (s *Store) putContent(ctx context.Context, id int64, requestJSON, responseJSON string) {
	if s.content == nil || !s.prompts.Load() {
		return
	}
	if requestJSON == "" && responseJSON == "" {
		return
	}
	_, _ = s.content.ExecContext(ctx, `
		INSERT INTO transcript_content (transcript_id, request_json, response_json, updated_at)
		VALUES (?, ?, NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(transcript_id) DO UPDATE SET
			request_json = CASE WHEN excluded.request_json = '' THEN transcript_content.request_json ELSE excluded.request_json END,
			response_json = CASE WHEN excluded.response_json IS NULL THEN transcript_content.response_json ELSE excluded.response_json END,
			updated_at = excluded.updated_at`,
		id, requestJSON, responseJSON)
}

// transcriptContent is one stored request/response body pair.
type transcriptContent struct {
	Request  string
	Response string
}

// contentByID loads every stored body, keyed by transcript id.
func (s *Store) contentByID(ctx context.Context) (map[int64]transcriptContent, error) {
	out := make(map[int64]transcriptContent)
	if s.content == nil {
		return out, nil
	}
	rows, err := s.content.QueryContext(ctx, `
		SELECT transcript_id, request_json, COALESCE(response_json, '')
		FROM transcript_content`)
	if err != nil {
		return nil, fmt.Errorf("list transcript content: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c transcriptContent
		if err := rows.Scan(&id, &c.Request, &c.Response); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// TranscriptRow is a stored request/response pair.
type TranscriptRow struct {
	ID              int64
	ConversationID  string
	GatewayModel    string
	UpstreamModel   string
	RequestJSON     string
	ResponseJSON    string
	Status          int
	PromptTokens    int
	CompletionToken int
	CachedTokens    int
	CostUSD         *float64
	CreatedAt       string
	CompletedAt     string
}

// Transcripts returns all transcripts, oldest first (test/admin access).
func (s *Store) Transcripts(ctx context.Context) ([]TranscriptRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_id, gateway_model, upstream_model,
		       COALESCE(status, 0),
		       COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0), COALESCE(cached_tokens,0),
		       cost_usd, created_at, COALESCE(completed_at,'')
		FROM transcripts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	content, err := s.contentByID(ctx)
	if err != nil {
		return nil, err
	}
	var out []TranscriptRow
	for rows.Next() {
		var t TranscriptRow
		if err := rows.Scan(&t.ID, &t.ConversationID, &t.GatewayModel, &t.UpstreamModel,
			&t.Status,
			&t.PromptTokens, &t.CompletionToken, &t.CachedTokens,
			&t.CostUSD, &t.CreatedAt, &t.CompletedAt); err != nil {
			return nil, err
		}
		if c, ok := content[t.ID]; ok {
			t.RequestJSON, t.ResponseJSON = c.Request, c.Response
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Transcript returns one transcript by id, or ErrTranscriptNotFound. The
// request/response bodies come from the content database; when prompt storage
// is disabled they are empty.
func (s *Store) Transcript(ctx context.Context, id int64) (TranscriptRow, error) {
	var t TranscriptRow
	err := s.db.QueryRowContext(ctx, `
		SELECT id, conversation_id, gateway_model, upstream_model,
		       COALESCE(status, 0),
		       COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0), COALESCE(cached_tokens,0),
		       cost_usd, created_at, COALESCE(completed_at,'')
		FROM transcripts WHERE id = ?`, id).
		Scan(&t.ID, &t.ConversationID, &t.GatewayModel, &t.UpstreamModel,
			&t.Status,
			&t.PromptTokens, &t.CompletionToken, &t.CachedTokens,
			&t.CostUSD, &t.CreatedAt, &t.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TranscriptRow{}, ErrTranscriptNotFound
	}
	if err != nil {
		return TranscriptRow{}, fmt.Errorf("get transcript %d: %w", id, err)
	}
	if s.content != nil {
		err := s.content.QueryRowContext(ctx, `
			SELECT request_json, COALESCE(response_json, '')
			FROM transcript_content WHERE transcript_id = ?`, id).
			Scan(&t.RequestJSON, &t.ResponseJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return TranscriptRow{}, fmt.Errorf("get transcript content %d: %w", id, err)
		}
	}
	return t, nil
}

// RequestRow is a recent request for the admin usage page: a transcript
// plus the key that made it.
type RequestRow struct {
	ID              int64
	ConversationID  string
	KeyName         string
	GatewayModel    string
	Status          int
	PromptTokens    int
	CompletionToken int
	CachedTokens    int
	CostUSD         *float64
	CreatedAt       string
	// DurationMS is the wall-clock time from the transcript being created to
	// its completion (how long the whole request took at the gateway). Nil
	// while a request is still in flight.
	DurationMS *int64
}

// RequestFilter narrows a request query. It reuses UsageFilter's timestamp
// and key constraints, plus windowing, sorting and per-column filters.
type RequestFilter struct {
	UsageFilter
	Limit  int
	Offset int
	Sort   string
	Dir    string
	Filter map[string]FilterSpec
}

// requestKeyExpr matches the label the admin UI shows for requests of deleted
// keys, so the per-column key filter and sort agree with what is displayed.
const requestKeyExpr = "COALESCE(vk.name, '(deleted key)')"

// requestFilterCols and requestSortCols map the admin column ids to SQL
// expressions for the requests table.
var (
	requestFilterCols = map[string]string{
		"key":        requestKeyExpr,
		"model":      "t.gateway_model",
		"status":     "COALESCE(t.status, 0)",
		"prompt":     "COALESCE(t.prompt_tokens, 0)",
		"cached":     "COALESCE(t.cached_tokens, 0)",
		"completion": "COALESCE(t.completion_tokens, 0)",
		"cost":       "t.cost_usd",
	}
	requestSortCols = map[string]string{
		"id":         "t.id",
		"time":       "t.created_at",
		"key":        requestKeyExpr,
		"model":      "t.gateway_model",
		"status":     "COALESCE(t.status, 0)",
		"prompt":     "COALESCE(t.prompt_tokens, 0)",
		"cached":     "COALESCE(t.cached_tokens, 0)",
		"completion": "COALESCE(t.completion_tokens, 0)",
		"cost":       "t.cost_usd",
		"duration":   "CAST((julianday(t.completed_at) - julianday(t.created_at)) * 86400000 AS INTEGER)",
	}
)

// Requests returns requests matching f along with the total number of matches
// before windowing. It sorts by the requested column (newest first by default)
// and applies the per-column filters.
func (s *Store) Requests(ctx context.Context, f RequestFilter) ([]RequestRow, int, error) {
	conds, args := f.where("t.created_at", "vk.name")
	fconds, fargs, err := buildFilters(f.Filter, requestFilterCols)
	if err != nil {
		return nil, 0, err
	}
	conds = append(conds, fconds...)
	args = append(args, fargs...)
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM transcripts t
		JOIN conversations c ON c.id = t.conversation_id
		LEFT JOIN virtual_keys vk ON vk.id = c.key_id
		`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order, err := buildOrder(ListParams{Sort: f.Sort, Dir: f.Dir}, requestSortCols, "id", "desc", "t.id")
	if err != nil {
		return nil, 0, err
	}
	query := `
		SELECT t.id, t.conversation_id, COALESCE(vk.name, '(deleted key)'),
		       t.gateway_model, COALESCE(t.status, 0),
		       COALESCE(t.prompt_tokens,0), COALESCE(t.completion_tokens,0), COALESCE(t.cached_tokens,0),
		       t.cost_usd, t.created_at,
		       CAST((julianday(t.completed_at) - julianday(t.created_at)) * 86400000 AS INTEGER)
		FROM transcripts t
		JOIN conversations c ON c.id = t.conversation_id
		LEFT JOIN virtual_keys vk ON vk.id = c.key_id
		` + where + order

	limit := f.Limit
	if limit == 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	if limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, limit, f.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]RequestRow, 0)
	for rows.Next() {
		var r RequestRow
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.KeyName, &r.GatewayModel,
			&r.Status, &r.PromptTokens, &r.CompletionToken, &r.CachedTokens,
			&r.CostUSD, &r.CreatedAt, &r.DurationMS); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// transcriptJSON marshals v for storage, falling back to a JSON string on
// marshal failure (so odd payloads are still inspectable).
func transcriptJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(fmt.Sprint(v))
	}
	return string(b)
}
