package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	Filename string
	Text     string
	// KnownTags is the vocabulary already in use across the archive, so the
	// model reaches for an existing tag instead of inventing a near-duplicate.
	KnownTags []string
	// CurrentTags is what this document already carries. Re-tagging replaces
	// the list outright, so without this a second pass silently drops
	// whatever the first pass — or a person — put there.
	CurrentTags []string
	// CurrentTitle is the title on the document now, for the same reason and
	// with the same danger: what comes back replaces it. Empty when the
	// document has never been titled by anything but its own filename, which
	// the prompt already knows about and should feel free to improve on.
	CurrentTitle string
	// Owner is who the archive belongs to, and the model cannot do without it:
	// from a single document there is no telling the owner's passport
	// application, where tagging the applicant is noise, from a spouse's, where
	// the applicant's name is exactly the tag that sets their documents apart.
	// Both look like "person X's application". Empty falls back to a heuristic
	// that gets the first case right and the second wrong.
	Owner string
}

// textCap bounds what we send. Dates and totals cluster at the start and end
// of a document, so a long one is sent head-and-tail rather than truncated.
const (
	textCap = 36000
	headCap = 32000
	tailCap = 4000
	// Twelve rather than five, now that the schema asks for a form number as
	// well as the kind of document, its subject and its other party. Tags are
	// the only classification here, so a W-2 that is genuinely a tax document,
	// an employer's and a form has use for all of those, and the form number
	// should not have to displace one to fit. A ceiling this high is an
	// invitation to pad, so the schema asks for only as many as apply.
	maxTags = 12
)

// capText applies that bound. It lives outside Enrich so the two cuts can be
// tested without a call to the model, which is the only part of Enrich that
// could go wrong here and the only part a test cannot reach.
//
// Both ends land on character boundaries: the caps are byte caps, but a byte cut
// through a multi-byte character reaches the model as a replacement character in
// the middle of a word, and the head cut is exactly where dates and reference
// numbers sit.
func capText(text string) string {
	if len(text) <= textCap {
		return text
	}
	return truncateBytes(text, headCap) + "\n\n…[middle omitted]…\n\n" + tailBytes(text, tailCap)
}

// modelPrices is USD per million tokens, from OpenAI's published price page
// (August 2026). OpenAI does not expose prices over the API and does not
// reprice existing models — new prices arrive as new model names — so a table
// in code, updated when the model is switched, is the honest source.
var modelPrices = map[string]struct{ In, Out float64 }{
	"gpt-5.6-sol":   {5.00, 30.00},
	"gpt-5.6-terra": {2.00, 12.00},
	"gpt-5.6-luna":  {0.20, 1.20},
	"gpt-5.4":       {2.50, 15.00},
	"gpt-5.4-mini":  {0.75, 4.50},
	"gpt-5.4-nano":  {0.20, 1.25},
}

// llmCents turns one call's tokens into US cents. A model the table has never
// heard of costs zero rather than a guess: wrong prices are worse than none,
// because the number still looks authoritative. Callers pair this with
// modelPriced so they can say nothing instead of saying zero.
func llmCents(model string, u Usage) float64 {
	p, ok := modelPrices[model]
	if !ok {
		return 0
	}
	usd := (float64(u.In)*p.In + float64(u.Out)*p.Out) / 1e6
	return usd * 100
}

// modelPriced reports whether costs for this model can be named at all.
func modelPriced(model string) bool {
	_, ok := modelPrices[model]
	return ok
}

// applyUsage records what a call cost on the document that caused it. Tokens
// are replaced because they describe the run that just happened; cents are
// added because the money really was spent each time. A call that never
// reached the model reports no tokens and must leave the previous run's
// figures alone rather than blanking them.
func applyUsage(doc *Doc, model string, used Usage) {
	if used.In == 0 && used.Out == 0 {
		return
	}
	doc.LLMIn, doc.LLMOut = used.In, used.Out
	doc.LLMCents += llmCents(model, used)
}

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

// Wait reports how long to hold off before another call is worth making, and
// why enrichment has stopped outright when it has. The reason comes back with
// the duration because a sentinel duration meaning "never" forced the caller
// to take the lock a second time to ask what this call already knew.
func (b *budget) Wait() (hold time.Duration, stopped string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped != "" {
		return 0, b.stopped
	}
	now := time.Now()
	if b.blocked.After(now) {
		return b.blocked.Sub(now), ""
	}
	// Zero remaining is the case worth catching: the next call is guaranteed
	// to be rejected, so waiting beats spending a request to discover that.
	if b.remaining == 0 && b.resetAt.After(now) {
		return b.resetAt.Sub(now), ""
	}
	return 0, ""
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

func (e *OpenAIEnricher) Spend() (calls, in, out int64) {
	return e.calls.Load(), e.inTokens.Load(), e.outTokens.Load()
}

func (e *OpenAIEnricher) Budget() (remaining int, resetIn time.Duration, stopped string) {
	return e.budget.status()
}

func metaSchema(maxTags int) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"title", "summary", "tags", "created_date"},
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "Short human-readable title naming who it is from and what it is, e.g. 'Northwind Electricity Statement'. When it is one of a recurring series — a monthly statement, an annual tax form — include the period ('Northwind Electricity Statement March 2026'), or twelve of them end up sharing one name. No file extension.",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "One or two plain sentences saying what this document is and what it actually says — the parties, amounts, dates, reference numbers, and the decision or obligation it records. The summary is searched, so specifics beat categories: 'renews the Foster St lease for twelve months at $2,400' can be found again, 'a lease renewal document' cannot. No preamble like 'This document is'.",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": fmt.Sprintf("1-%d short lowercase tags: as many as genuinely apply and no more, so a one-page receipt comes back with two or three rather than filling the allowance. Cover what the document is, what it is about, and the other party to it where there is one — never the archive's owner, and never a place that is only part of an address. A document printed on a numbered form also carries that form's own designation, lowercased and closed up with no hyphens or spaces however the form prints it — w2, 1099int, form16 — whoever issues it, tax authority or not.", maxTags),
			},
			"created_date": map[string]any{
				"type":        "string",
				"description": "The month the document itself is dated, as YYYY-MM. This is the statement or issue date, not a due date, a billing-period end, or a printed-on date. A document that names no month but belongs unambiguously to one year — a tax year, a form year, 'your 2022 forms are enclosed' — is dated December of that year, YYYY-12, which files it at the end of the year it belongs to; do not guess a likelier month, since inventing precision is worse than December. The year is the one the document is about, not the one it was sent in: a 2022 W-2 issued in January 2023 is 2022-12. Empty string only when there is no year either.",
			},
		},
	}
}

// Usage is what one call actually cost, returned alongside the result so the
// tokens can be attributed to the document that caused them rather than only
// to a process-wide total.
type Usage struct{ In, Out int64 }

// enrichPrompt is the system prompt for one document, apart from Enrich so its
// decisions are testable without a call to the model.
//
// The shape it teaches: a tag earns its place by narrowing a search. The two
// failures the archive actually produced were both violations of that — the
// owner's own name on a third of the documents, which narrows nothing in an
// archive where everything is theirs, and "sydney" on a citizenship
// application because the address block mentions it, when the document is
// about citizenship and a country. Every rule below is one of those failures
// stated the other way round.
func enrichPrompt(in EnrichInput) string {
	var sb strings.Builder
	sb.WriteString("You file documents for one person's private archive. Extract metadata from the document text.\n")
	sb.WriteString("Tags are the only classification in this system, and a tag earns its place by narrowing ")
	sb.WriteString("a search: the owner looking for this document years from now, among thousands that are ")
	sb.WriteString("all theirs. Three kinds of tag do that — what the document is (statement, application, ")
	sb.WriteString("receipt, tax), what it is about (citizenship, insurance, car, medical), and the other ")
	sb.WriteString("party to it (northwind, ato, irs). Prefer a tag that could group several documents over ")
	sb.WriteString("one so specific it will only ever match this one.\n")
	if in.Owner != "" {
		fmt.Fprintf(&sb, "The archive belongs to %s. Never tag the owner: their name, in any form or ", in.Owner)
		sb.WriteString("spelling, is on nearly every document here, so it finds everything and distinguishes ")
		sb.WriteString("nothing. A document that is really someone else's — a spouse's application, a child's ")
		sb.WriteString("report — takes that person's name, which is exactly what sets their documents apart ")
		sb.WriteString("in this archive.\n")
	} else {
		sb.WriteString("Never tag the archive's owner: the account holder, applicant or addressee is on ")
		sb.WriteString("nearly every document here, so their name finds everything and distinguishes nothing.\n")
	}
	sb.WriteString("A place is a tag when the document is about the place — a trip, an event held there, a ")
	sb.WriteString("property — never because it appears in an address. A citizenship application is ")
	sb.WriteString("citizenship and the country applied to, not the city the applicant happens to live in.\n")
	if len(in.KnownTags) > 0 {
		sb.WriteString("Reuse these existing tags whenever one fits, rather than inventing a near-duplicate: ")
		sb.WriteString(strings.Join(in.KnownTags, ", "))
		sb.WriteString(".\n")
	}
	// Your answer replaces the tag list wholesale, so anything left out is
	// removed. Say so plainly, and give the asymmetry: a tag someone chose is
	// worth more than the tidiness of dropping it. "Breaks a rule above" is the
	// named escape hatch — without it, the model reads this paragraph as
	// protecting the very tags the rules exist to remove.
	if len(in.CurrentTags) > 0 {
		sb.WriteString("This document is already tagged: ")
		sb.WriteString(strings.Join(in.CurrentTags, ", "))
		sb.WriteString(".\nReturn those tags again unless one is clearly wrong for this document or breaks ")
		sb.WriteString("a rule above, and add any that are missing. Your list replaces the current one, so ")
		sb.WriteString("a tag you leave out is deleted; dropping a tag someone chose is worse than keeping ")
		sb.WriteString("one that is merely imprecise.\n")
	}
	// Same danger as the tags, and it went unsaid for longer: the title comes
	// back replaced whether or not there was anything wrong with it. Re-running
	// the model on unchanged text produced "Ticket Receipt" one time and
	// "Admission Tickets" the next — both fine, and the second one silently
	// overwrote a title its owner may have chosen deliberately.
	//
	// Only when it is a real title. A document nobody has titled carries its own
	// filename, and offering that back as something to preserve would defend the
	// very thing this step exists to improve on.
	if in.CurrentTitle != "" {
		fmt.Fprintf(&sb, "This document is currently titled %q.\n", in.CurrentTitle)
		sb.WriteString("Your answer replaces that title, so return it unchanged unless it is wrong ")
		sb.WriteString("or genuinely uninformative — someone may have written it, and a title that ")
		sb.WriteString("keeps changing is worse than one that is merely not how you would have put it. ")
		sb.WriteString("Rewrite it when it names the wrong document, and leave it alone for the sake ")
		sb.WriteString("of phrasing.\n")
	}
	fmt.Fprintf(&sb, "The original filename is %q; use it only if the text is uninformative.\n", in.Filename)
	return sb.String()
}

// Enrich returns usage on every path where the request reached the model,
// including one whose answer we then fail to parse — that response was
// billed, so the document should carry the cost.
func (e *OpenAIEnricher) Enrich(ctx context.Context, in EnrichInput) (Meta, Usage, error) {
	var meta Meta
	var used Usage

	if hold, stopped := e.budget.Wait(); hold != 0 || stopped != "" {
		return meta, used, ErrRateLimited
	}

	text := strings.TrimSpace(in.Text)
	if text == "" {
		return meta, used, errors.New("no text to work from")
	}
	text = capText(text)

	// Rare now the cap is twelve, but a document that arrived with more — hand
	// tagged, or tagged when the ceiling was lower — would otherwise lose one to
	// the cap however careful the model was, so the ceiling rises to fit.
	limit := maxTags
	if n := len(in.CurrentTags); n > limit {
		limit = n
	}

	resp, err := e.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:           e.model,
		ReasoningEffort: shared.ReasoningEffortHigh,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(enrichPrompt(in)),
			openai.UserMessage(text),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "document_metadata",
					Schema: metaSchema(limit),
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
				return meta, used, ErrRateLimited
			case 401, 403:
				// A bad key will not fix itself. Stop, rather than walk the
				// whole queue producing identical failures.
				e.budget.stop(fmt.Sprintf("authentication failed (HTTP %d)", apiErr.StatusCode))
				return meta, used, err
			}
		}
		return meta, used, err
	}

	used = Usage{In: resp.Usage.PromptTokens, Out: resp.Usage.CompletionTokens}
	e.calls.Add(1)
	e.inTokens.Add(used.In)
	e.outTokens.Add(used.Out)

	if len(resp.Choices) == 0 {
		return meta, used, errors.New("model returned no choices")
	}
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &meta); err != nil {
		return meta, used, fmt.Errorf("parsing model output: %w", err)
	}
	return cleanMeta(meta, limit), used, nil
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

func cleanMeta(m Meta, maxTags int) Meta {
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

func (q *EnrichQueue) Add(id int) { q.add(id, false) }

func (q *EnrichQueue) add(id int, front bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued[id] {
		return
	}
	q.queued[id] = true
	if front {
		q.pending = append([]int{id}, q.pending...)
		return
	}
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
func (q *EnrichQueue) requeue(id int) { q.add(id, true) }

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

		hold, stopped := q.app.enricher.budget.Wait()
		if stopped != "" {
			logf("enrichment stopped: %s", stopped)
			return
		}
		if hold > 0 {
			nap := hold + time.Second
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
	// A document in the trash is queued for deletion, so paying the model to
	// title and tag it is money spent on something nobody will read. It is
	// dropped rather than requeued; if it comes back out of the trash, the
	// document page's Re-tag asks for this again, and so does the next restart.
	if doc.Trashed() {
		return
	}

	known, _ := q.app.search.Vocabulary(ctx, "tags", 50)
	// The vocabulary is a facet count of the index, which now carries the
	// reserved names as well — so the list of tags already in use has to be
	// filtered before it becomes the list of tags on offer. A model shown
	// "trash" as an existing tag will eventually use it — and the same goes
	// for the owner's name, which past tagging left all over the vocabulary:
	// offering it back as an established tag argues against the prompt's own
	// rule.
	owner := ownerTags(q.app.cfg.Owner)
	meta, used, err := q.app.enricher.Enrich(ctx, EnrichInput{
		Filename: doc.OriginalName, Text: doc.Content,
		KnownTags: withoutTags(withoutReserved(known), owner...),
		// needs-review is ours, not a description of the document; offering it
		// back would invite the model to keep it forever. The owner's name is
		// filtered for the opposite reason: shown as an existing tag, it reads
		// as one somebody chose and the prompt's preservation rule defends it.
		CurrentTags:  withoutTags(doc.Tags, append([]string{TagNeedsReview}, owner...)...),
		CurrentTitle: realTitle(doc),
		Owner:        q.app.cfg.Owner,
	})
	// Whatever the outcome, tokens the model actually billed belong to this
	// document — a failed parse still cost money.
	applyUsage(doc, q.app.cfg.LLMModel, used)

	if errors.Is(err, ErrRateLimited) {
		q.requeue(id)
		return
	}
	if err != nil {
		doc.Tags = appendTag(doc.Tags, TagNeedsReview)
		q.save(ctx, doc)
		// "enrichment" leads for the same reason the pipeline's failures lead
		// with their stage: both events are called failed, and what failed is
		// the thing worth knowing first.
		q.app.record("failed", id, "", "enrichment: "+err.Error())
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
	}
	// The model is never shown a reserved name or the owner's, but what it
	// returns is not a promise. Filtered before the length is tested, so a
	// reply made entirely of names it may not use leaves the tags it had
	// rather than emptying them.
	if tags := withoutTags(withoutReserved(meta.Tags), owner...); len(tags) > 0 {
		doc.Tags = tags
	}
	doc.Enriched = true
	doc.EnrichedTS = time.Now().Unix()
	q.save(ctx, doc)

	q.mu.Lock()
	q.done++
	q.mu.Unlock()
	q.app.record("enriched", id, "", enrichDetail(doc, used, llmCents(q.app.cfg.LLMModel, used)))
}

// enrichDetail says what the model decided and what it cost. What it decided is
// the substance of the event — a title, a set of tags and a date, which are
// exactly the fields a person would otherwise open the document to check — and
// the tokens are how anyone judges whether that was worth paying for. It used
// to be that only the log carried the decisions and only the journal kept the
// cost, so neither could answer "what did this model actually do to my archive"
// on its own.
//
// The cost is named only when the price table knows this model, so an unpriced
// run says nothing rather than claiming it was free.
func enrichDetail(doc *Doc, used Usage, cents float64) string {
	parts := []string{fmt.Sprintf("%q", doc.Title)}
	if len(doc.Tags) > 0 {
		parts = append(parts, "["+strings.Join(doc.Tags, " ")+"]")
	}
	if doc.CreatedDate != "" {
		parts = append(parts, doc.CreatedDate)
	}
	parts = append(parts, fmt.Sprintf("%s in · %s out", commaNum(used.In), commaNum(used.Out)))
	if s := centsStr(cents); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

// save stores the metadata the model produced. persist holds until the write
// and the index both accept it, so the only error left is shutdown — the
// tokens are already spent either way, and the document stays unenriched on
// disk, which puts it back in this queue on the next start.
func (q *EnrichQueue) save(ctx context.Context, doc *Doc) {
	if err := q.app.persist(ctx, doc); err != nil && !errors.Is(err, context.Canceled) {
		logf("doc %d: after enrichment: %v", doc.ID, err)
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

// ownerTags is the owner's name as the tags a model would make of it, so the
// two obvious spellings can be stripped mechanically wherever tags flow: the
// vocabulary on offer, the current tags shown back, and the answer. The prompt
// handles other forms — initials, first name alone — where matching in code
// would start guessing; this handles the certainty that the exact ones never
// get through, whatever the model was thinking that day.
func ownerTags(owner string) []string {
	fields := strings.Fields(strings.ToLower(owner))
	if len(fields) == 0 {
		return nil
	}
	if len(fields) == 1 {
		return fields
	}
	return []string{strings.Join(fields, "-"), strings.Join(fields, "")}
}

// realTitle is the document's title if it has one, and empty if what it is
// carrying is only the name of the file it arrived as.
//
// The pipeline titles an untitled document after its own filename so that
// nothing shows up blank, which means "has a title" and "has been titled" are
// different questions. Only the second is worth defending: handing the model
// "Scan 2026-05-02 11.14.38.pdf" and asking it to keep that would preserve
// exactly what it was called in to fix.
func realTitle(doc *Doc) string {
	if doc.Title == doc.OriginalName {
		return ""
	}
	return doc.Title
}

func appendTag(tags []string, t string) []string {
	if slices.Contains(tags, t) {
		return tags
	}
	return append(tags, t)
}
