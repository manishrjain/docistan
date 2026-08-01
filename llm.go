package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Meta is what the model is asked to produce for a document.
type Meta struct {
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	Tags        []string `json:"tags"`
	CreatedDate string   `json:"created_date"`
}

type EnrichInput struct {
	Filename  string
	Text      string
	KnownTags []string
}

// textCap bounds what we send. Dates and totals cluster at the start and end
// of a document, so a long one is sent head-and-tail rather than truncated.
const (
	textCap = 36000
	headCap = 32000
	tailCap = 4000
	maxTags = 5
)

// ErrRateLimited means the request budget is spent. Nothing is wrong with the
// document; it should simply be tried again after the reset.
var ErrRateLimited = errors.New("rate limited")

// budget tracks what the API reports about the remaining request allowance so
// that calls certain to be rejected are never made. Every response carries the
// current numbers, so this stays accurate without any polling of its own.
type budget struct {
	mu        sync.Mutex
	remaining int // -1 until a response tells us
	resetAt   time.Time
	blocked   time.Time // hard stop after a 429
	stopped   string    // non-empty disables enrichment entirely
}

func (b *budget) observe(h http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v := h.Get("x-ratelimit-remaining-requests"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			b.remaining = n
		}
	}
	if v := h.Get("x-ratelimit-reset-requests"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			b.resetAt = time.Now().Add(d)
		}
	}
}

// Wait reports how long to hold off before another call is worth making.
// A negative result means never.
func (b *budget) Wait() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped != "" {
		return -1
	}
	now := time.Now()
	if b.blocked.After(now) {
		return b.blocked.Sub(now)
	}
	// Zero remaining is the case worth catching: the next call is guaranteed
	// to be rejected, so waiting beats spending a request to discover that.
	if b.remaining == 0 && b.resetAt.After(now) {
		return b.resetAt.Sub(now)
	}
	return 0
}

func (b *budget) block(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blocked = time.Now().Add(d)
	b.remaining = 0
}

func (b *budget) stop(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = reason
}

func (b *budget) status() (remaining int, resetIn time.Duration, stopped string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.resetAt.After(time.Now()) {
		resetIn = time.Until(b.resetAt)
	}
	return b.remaining, resetIn, b.stopped
}

type OpenAIEnricher struct {
	client openai.Client
	model  string
	budget budget

	inTokens  atomic.Int64
	outTokens atomic.Int64
	calls     atomic.Int64
}

func NewOpenAIEnricher(model, apiKey string) *OpenAIEnricher {
	e := &OpenAIEnricher{model: model}
	e.budget.remaining = -1

	opts := []option.RequestOption{
		// The SDK retries 429s by itself, which against a budget counted in
		// requests burns the allowance several times faster than the document
		// count implies. Back off as a whole instead.
		option.WithMaxRetries(0),
		option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			resp, err := next(req)
			if resp != nil {
				e.budget.observe(resp.Header)
			}
			return resp, err
		}),
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	e.client = openai.NewClient(opts...)
	return e
}

func (e *OpenAIEnricher) Spend() (calls, in, out int64, usd float64) {
	calls, in, out = e.calls.Load(), e.inTokens.Load(), e.outTokens.Load()
	usd = float64(in)*0.20/1e6 + float64(out)*1.20/1e6
	return
}

func (e *OpenAIEnricher) Budget() (remaining int, resetIn time.Duration, stopped string) {
	return e.budget.status()
}

func metaSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"title", "summary", "tags", "created_date"},
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "Short human-readable title, e.g. 'Northwind Electricity Statement'. No file extension.",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "One or two plain sentences saying what this document is and what it actually says — amounts, dates, parties, the decision or obligation it records. No preamble like 'This document is'.",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "1-5 short lowercase tags. Tags are the only classification, so include both the kind of document (invoice, statement, tax) and who or what it concerns (northwind, car, medical).",
			},
			"created_date": map[string]any{
				"type":        "string",
				"description": "The month the document itself is dated, as YYYY-MM. This is the statement or issue date, not a due date, a billing-period end, or a printed-on date. Empty string if the document carries no date.",
			},
		},
	}
}

func (e *OpenAIEnricher) Enrich(ctx context.Context, in EnrichInput) (Meta, error) {
	var meta Meta

	if wait := e.budget.Wait(); wait != 0 {
		return meta, ErrRateLimited
	}

	text := strings.TrimSpace(in.Text)
	if text == "" {
		return meta, errors.New("no text to work from")
	}
	if len(text) > textCap {
		text = text[:headCap] + "\n\n…[middle omitted]…\n\n" + text[len(text)-tailCap:]
	}

	var sb strings.Builder
	sb.WriteString("You file documents for a personal archive. Extract metadata from the document text.\n")
	sb.WriteString("Tags are the only classification in this system, so choose them carefully.\n")
	if len(in.KnownTags) > 0 {
		sb.WriteString("Reuse these existing tags whenever one fits, rather than inventing a near-duplicate: ")
		sb.WriteString(strings.Join(in.KnownTags, ", "))
		sb.WriteString(".\n")
	}
	fmt.Fprintf(&sb, "The original filename is %q; use it only if the text is uninformative.\n", in.Filename)

	resp, err := e.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: e.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(sb.String()),
			openai.UserMessage(text),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "document_metadata",
					Schema: metaSchema(),
					Strict: openai.Bool(true),
				},
			},
		},
	})
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case 429:
				e.budget.block(e.resetHint(apiErr))
				return meta, ErrRateLimited
			case 401, 403:
				// A bad key will not fix itself. Stop, rather than walk the
				// whole queue producing identical failures.
				e.budget.stop(fmt.Sprintf("authentication failed (HTTP %d)", apiErr.StatusCode))
				return meta, err
			}
		}
		return meta, err
	}

	e.calls.Add(1)
	e.inTokens.Add(resp.Usage.PromptTokens)
	e.outTokens.Add(resp.Usage.CompletionTokens)

	if len(resp.Choices) == 0 {
		return meta, errors.New("model returned no choices")
	}
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &meta); err != nil {
		return meta, fmt.Errorf("parsing model output: %w", err)
	}
	return cleanMeta(meta), nil
}

func (e *OpenAIEnricher) resetHint(apiErr *openai.Error) time.Duration {
	wait := 60 * time.Second
	if apiErr.Response == nil {
		return wait
	}
	if v := apiErr.Response.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
	}
	if v := apiErr.Response.Header.Get("x-ratelimit-reset-requests"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			wait = d
		}
	}
	return wait
}

func cleanMeta(m Meta) Meta {
	m.Title = strings.TrimSpace(m.Title)
	m.Summary = strings.TrimSpace(m.Summary)
	m.CreatedDate = normalizeMonth(m.CreatedDate)

	seen := map[string]bool{}
	var tags []string
	for _, t := range m.Tags {
		t = strings.Trim(strings.ToLower(strings.TrimSpace(t)), ".,;:")
		if t == "" || seen[t] || len(tags) >= maxTags {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	m.Tags = tags
	return m
}

// EnrichQueue holds documents waiting on the model. Ingestion never blocks on
// it: a document is fully usable — searchable, readable, thumbnailed — as soon
// as the local tools finish, and metadata arrives whenever the budget allows.
type EnrichQueue struct {
	app *App

	mu      sync.Mutex
	pending []int
	queued  map[int]bool
	active  int // id currently being enriched, 0 when idle
	done    int
	failed  int
}

func NewEnrichQueue(app *App) *EnrichQueue {
	return &EnrichQueue{app: app, queued: map[int]bool{}}
}

func (q *EnrichQueue) Add(id int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued[id] {
		return
	}
	q.queued[id] = true
	q.pending = append(q.pending, id)
}

func (q *EnrichQueue) next() (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return 0, false
	}
	id := q.pending[0]
	q.pending = q.pending[1:]
	delete(q.queued, id)
	// Stays claimed while in flight: a caller polling for the result must not
	// see a gap between "waiting" and "done" and read it as failure.
	q.active = id
	return id, true
}

func (q *EnrichQueue) clearActive() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.active = 0
}

// requeue puts a document back at the front, for when nothing was wrong with
// it and the budget simply ran out.
func (q *EnrichQueue) requeue(id int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued[id] {
		return
	}
	q.queued[id] = true
	q.pending = append([]int{id}, q.pending...)
}

// Has reports whether a document is waiting or currently being enriched, so
// its page can say "in progress" rather than leaving the user guessing.
func (q *EnrichQueue) Has(id int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queued[id] || q.active == id
}

func (q *EnrichQueue) Stats() (pending, done, failed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending), q.done, q.failed
}

// Run drains the queue for the life of the process, pausing whenever the
// budget says a call would be rejected rather than spending one to find out.
func (q *EnrichQueue) Run(ctx context.Context) {
	if q.app.enricher == nil {
		return
	}
	const (
		idle   = 5 * time.Second
		pace   = 250 * time.Millisecond
		maxNap = 5 * time.Minute
	)

	for {
		if ctx.Err() != nil {
			return
		}

		if wait := q.app.enricher.budget.Wait(); wait != 0 {
			if wait < 0 {
				_, _, stopped := q.app.enricher.Budget()
				logf("enrichment stopped: %s", stopped)
				return
			}
			nap := wait + time.Second
			if nap > maxNap {
				nap = maxNap
			}
			pending, _, _ := q.Stats()
			logf("enrichment paused %s for rate limit (%d document(s) waiting)",
				nap.Round(time.Second), pending)
			if !sleepCtx(ctx, nap) {
				return
			}
			continue
		}

		id, ok := q.next()
		if !ok {
			if !sleepCtx(ctx, idle) {
				return
			}
			continue
		}
		q.enrichOne(ctx, id)
		q.clearActive()
		if !sleepCtx(ctx, pace) {
			return
		}
	}
}

func (q *EnrichQueue) enrichOne(ctx context.Context, id int) {
	doc, err := q.app.store.Load(id)
	if err != nil {
		return
	}
	if doc.Status != StatusReady || doc.Enriched || strings.TrimSpace(doc.Content) == "" {
		return
	}

	known, _ := q.app.search.Vocabulary(ctx, "tags", 50)
	meta, err := q.app.enricher.Enrich(ctx, EnrichInput{
		Filename: doc.OriginalName, Text: doc.Content, KnownTags: known,
	})
	if errors.Is(err, ErrRateLimited) {
		q.requeue(id)
		return
	}
	if err != nil {
		logf("doc %d: enrichment failed: %v", id, err)
		doc.Tags = appendTag(doc.Tags, "needs-review")
		q.save(ctx, doc)
		q.mu.Lock()
		q.failed++
		q.mu.Unlock()
		return
	}

	if meta.Title != "" {
		doc.Title = meta.Title
	}
	if meta.Summary != "" {
		doc.Summary = meta.Summary
	}
	if meta.CreatedDate != "" {
		doc.CreatedDate = meta.CreatedDate
		doc.CreatedTS = parseDateTS(meta.CreatedDate)
	}
	if len(meta.Tags) > 0 {
		doc.Tags = meta.Tags
	}
	doc.Enriched = true
	doc.EnrichedTS = time.Now().Unix()
	q.save(ctx, doc)

	q.mu.Lock()
	q.done++
	q.mu.Unlock()
	logf("doc %d tagged: %q %v %s", doc.ID, doc.Title, doc.Tags, doc.CreatedDate)
}

func (q *EnrichQueue) save(ctx context.Context, doc *Doc) {
	if err := q.app.store.Save(doc); err != nil {
		logf("doc %d: saving after enrichment: %v", doc.ID, err)
		return
	}
	if err := q.app.search.Upsert(ctx, doc); err != nil {
		logf("doc %d: indexing after enrichment: %v", doc.ID, err)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func appendTag(tags []string, t string) []string {
	for _, existing := range tags {
		if existing == t {
			return tags
		}
	}
	return append(tags, t)
}
