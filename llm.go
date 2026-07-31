package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Meta is what the model is asked to produce for a document.
type Meta struct {
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	CreatedDate string   `json:"created_date"`
}

// Enricher produces document metadata. Behind an interface because model
// pricing is moving fast enough that swapping providers should be contained.
type Enricher interface {
	Enrich(ctx context.Context, in EnrichInput) (Meta, error)
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

// ErrRateLimited means the account's request budget is exhausted. Enrichment
// backs off as a whole rather than per document: once the budget is gone,
// every further call would just burn more of it on a guaranteed failure.
var ErrRateLimited = errors.New("rate limited")

type OpenAIEnricher struct {
	client openai.Client
	model  string

	// Usage counters, so real spend is observable instead of estimated.
	inTokens  atomic.Int64
	outTokens atomic.Int64
	calls     atomic.Int64

	// Unix seconds until which calls are skipped outright.
	pausedUntil atomic.Int64
}

func NewOpenAIEnricher(model, apiKey string) *OpenAIEnricher {
	opts := []option.RequestOption{
		// The SDK retries 429s on its own. Combined with a caller-side retry
		// that turns one document into six requests against a budget counted
		// in requests, so keep it to a single attempt and let the circuit
		// breaker below handle backoff.
		option.WithMaxRetries(0),
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return &OpenAIEnricher{client: openai.NewClient(opts...), model: model}
}

// PausedFor reports how long enrichment is backed off, for the status page.
func (e *OpenAIEnricher) PausedFor() time.Duration {
	until := e.pausedUntil.Load()
	if until == 0 {
		return 0
	}
	d := time.Until(time.Unix(until, 0))
	if d < 0 {
		return 0
	}
	return d
}

// pause backs off until the rate limit resets, preferring the server's own
// Retry-After when it offers one.
func (e *OpenAIEnricher) pause(err error) {
	wait := 60 * time.Second
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.Response != nil {
		if v := apiErr.Response.Header.Get("Retry-After"); v != "" {
			if secs, perr := strconv.Atoi(v); perr == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
		}
		if v := apiErr.Response.Header.Get("x-ratelimit-reset-requests"); v != "" {
			if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
				wait = d
			}
		}
	}
	e.pausedUntil.Store(time.Now().Add(wait).Unix())
	logf("model rate limit reached; pausing enrichment for %s", wait.Round(time.Second))
}

// Spend reports cumulative usage. Prices are Luna's published rates; they are
// only used for the running estimate shown on the status page.
func (e *OpenAIEnricher) Spend() (calls int64, in int64, out int64, usd float64) {
	calls, in, out = e.calls.Load(), e.inTokens.Load(), e.outTokens.Load()
	usd = float64(in)*0.20/1e6 + float64(out)*1.20/1e6
	return
}

func metaSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"title", "tags", "created_date"},
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "Short human-readable title, e.g. 'Northwind Electricity Statement'. No file extension, no date unless it is part of the document's own name.",
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

	if e.PausedFor() > 0 {
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

	schemaParam := shared.ResponseFormatJSONSchemaJSONSchemaParam{
		Name:   "document_metadata",
		Schema: metaSchema(),
		Strict: openai.Bool(true),
	}

	resp, err := e.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: e.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(sb.String()),
			openai.UserMessage(text),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{JSONSchema: schemaParam},
		},
	})
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 429 {
			e.pause(err)
			return meta, ErrRateLimited
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

// cleanMeta normalizes whatever came back so downstream code never has to.
func cleanMeta(m Meta) Meta {
	m.Title = strings.TrimSpace(m.Title)
	m.CreatedDate = normalizeMonth(m.CreatedDate)

	seen := map[string]bool{}
	var tags []string
	for _, t := range m.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		t = strings.Trim(t, ".,;:")
		if t == "" || seen[t] || len(tags) >= maxTags {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	m.Tags = tags
	return m
}

// maybeEnrich fills in metadata using the model. It runs once per document —
// a reprocess or retry will not spend again — and never blocks ingestion: on
// any failure the document still completes with a filename-derived title.
func (p *Pipeline) maybeEnrich(ctx context.Context, doc *Doc) {
	if p.app.enricher == nil || doc.Enriched {
		return
	}
	if strings.TrimSpace(doc.Content) == "" {
		return
	}

	known, err := p.app.search.Vocabulary(ctx, "tags", 50)
	if err != nil {
		logf("doc %d: reading tag vocabulary: %v", doc.ID, err)
	}

	in := EnrichInput{Filename: doc.OriginalName, Text: doc.Content, KnownTags: known}

	meta, err := p.app.enricher.Enrich(ctx, in)
	if err != nil {
		// A rate limit is not this document's fault and will not improve by
		// trying again now, so it is tagged for a later sweep rather than
		// retried into an already-empty budget.
		if !errors.Is(err, ErrRateLimited) {
			logf("doc %d: enrichment failed: %v", doc.ID, err)
		}
		doc.Tags = appendTag(doc.Tags, "needs-review")
		return
	}

	if meta.Title != "" {
		doc.Title = meta.Title
	}
	if meta.CreatedDate != "" {
		doc.CreatedDate = meta.CreatedDate
		doc.CreatedTS = parseDateTS(meta.CreatedDate)
	}
	if len(meta.Tags) > 0 {
		doc.Tags = meta.Tags
	}
	doc.Enriched = true
}

func appendTag(tags []string, t string) []string {
	for _, existing := range tags {
		if existing == t {
			return tags
		}
	}
	return append(tags, t)
}
