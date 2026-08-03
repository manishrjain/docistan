package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/typesense/typesense-go/v3/typesense"
	"github.com/typesense/typesense-go/v3/typesense/api"
	"github.com/typesense/typesense-go/v3/typesense/api/pointer"
)

// contentIndexLimit caps how much text is handed to Typesense. The sidecar
// always keeps the full text; this only bounds the index.
const contentIndexLimit = 400 << 10

type Search struct {
	client *typesense.Client
	// collectionName is configurable because every boot drops and recreates
	// the collection. Two instances sharing one Typesense would otherwise
	// silently wipe each other's index on startup.
	collectionName string

	vocabMu sync.Mutex
	vocab   map[string]vocabEntry
}

// vocabTTL is how long a facet sweep of the whole archive is reused. The
// vocabulary is asked for on every document page view and again for every
// document enriched, so at eight thousand documents an uncached call is a
// full-corpus facet count per page view and per backfilled document. A tag
// added now appears in autocomplete within this window, which is a better
// trade than counting the archive again to shave seconds off that.
const vocabTTL = 30 * time.Second

type vocabEntry struct {
	values []string
	at     time.Time
}

func NewSearch(url, key, collection string) *Search {
	if collection == "" {
		collection = "documents"
	}
	return &Search{
		collectionName: collection,
		client: typesense.NewClient(
			typesense.WithServer(url),
			typesense.WithAPIKey(key),
			typesense.WithConnectionTimeout(10*time.Second),
		),
	}
}

func (s *Search) Health(ctx context.Context) error {
	ok, err := s.client.Health(ctx, 5*time.Second)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("typesense reports unhealthy")
	}
	return nil
}

func (s *Search) schema() *api.CollectionSchema {
	f := func(name, typ string, opts ...func(*api.Field)) api.Field {
		fl := api.Field{Name: name, Type: typ}
		for _, o := range opts {
			o(&fl)
		}
		return fl
	}
	facet := func(fl *api.Field) { fl.Facet = pointer.True() }
	sortable := func(fl *api.Field) { fl.Sort = pointer.True() }
	stored := func(fl *api.Field) { fl.Index = pointer.False(); fl.Optional = pointer.True() }

	return &api.CollectionSchema{
		Name: s.collectionName,
		Fields: []api.Field{
			f("title", "string"),
			f("summary", "string"),
			f("content", "string"),
			f("original_name", "string"),
			f("tags", "string[]", facet),
			f("status", "string", facet),
			f("sha256", "string"),
			// Both pages that show a failure read their Doc out of the index,
			// so leaving these out of the schema rendered the reason for the
			// failure as an empty cell.
			f("error", "string", stored),
			f("failed_stage", "string", stored),
			f("created_ts", "int64", sortable),
			f("added_ts", "int64", sortable),
			// Indexed rather than `stored`: every query filters on this, and
			// `stored` sets index:false, which makes a field unfilterable.
			f("delete_after_ts", "int64"),
			// Faceted purely for the stats Typesense returns with a facet, which
			// is what lets the archive total outlive the process.
			f("llm_in", "int64", facet),
			f("llm_out", "int64", facet),
			f("llm_cents", "float", facet),
			f("created_date", "string", stored),
			f("page_count", "int32", stored),
			f("file_size", "int64", stored),
			f("ocr_source", "string", stored),
			f("signed", "bool", stored),
			f("encrypted", "bool", stored),
			f("native_text", "bool", stored),
			f("enriched", "bool", stored),
			f("text_ts", "int64", stored),
			f("enriched_ts", "int64", stored),
		},
		DefaultSortingField: pointer.String("added_ts"),
	}
}

// EnsureFreshCollection drops any existing collection and recreates it. The
// index is rebuilt from sidecars on every boot, so starting clean guarantees
// the live schema always matches this code and makes schema changes free.
func (s *Search) EnsureFreshCollection(ctx context.Context) error {
	if _, err := s.client.Collection(s.collectionName).Delete(ctx); err != nil {
		// A missing collection is the normal case on a cold start.
		var apiErr *typesense.HTTPError
		if !errors.As(err, &apiErr) || apiErr.Status != 404 {
			logf("dropping existing collection: %v", err)
		}
	}
	_, err := s.client.Collections().Create(ctx, s.schema())
	return err
}

// tsDoc converts a Doc into the map Typesense stores. Typesense ids are
// strings, so the integer id is rendered in base 10.
func tsDoc(d *Doc) map[string]any {
	content := truncateBytes(d.Content, contentIndexLimit)
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"id":            strconv.Itoa(d.ID),
		"title":         d.Title,
		"summary":       d.Summary,
		"content":       content,
		"original_name": d.OriginalName,
		"tags":          tags,
		"status":        d.Status,
		"sha256":        d.SHA256,
		"error":         d.Error,
		"failed_stage":  d.FailedStage,
		// Derived here, at index-write time, rather than kept on the document.
		// The sidecar then stores only the date it was told, and the number the
		// index sorts and filters on cannot drift away from it.
		"created_ts": parseDateTS(d.CreatedDate),
		"added_ts":   d.AddedTS,
		// Zero for everything that is not in the trash, so the default view can
		// be a filter on this field rather than an absence of one.
		"delete_after_ts": d.DeleteAfterTS,
		"created_date":    d.CreatedDate,
		"page_count":      d.PageCount,
		"file_size":       d.FileSize,
		"ocr_source":      d.OCRSource,
		"signed":          d.Signed,
		"encrypted":       d.Encrypted,
		"enriched":        d.Enriched,
		"native_text":     d.NativeText,
		"llm_in":          d.LLMIn,
		"llm_out":         d.LLMOut,
		"llm_cents":       d.LLMCents,
		"text_ts":         d.TextTS,
		"enriched_ts":     d.EnrichedTS,
	}
}

func (s *Search) Upsert(ctx context.Context, d *Doc) error {
	_, err := s.client.Collection(s.collectionName).Documents().Upsert(ctx, tsDoc(d), &api.DocumentIndexParameters{})
	return err
}

// Delete takes one id out of the index. An id that was not in it is the state
// the caller asked for rather than a failure, so it is reported as success:
// every caller retries this until it succeeds, and Typesense's answer for "no
// such document" is a 404, which no amount of retrying will turn into anything
// else. Persisting a tombstone lands here for an id that is normally already
// gone, so this is the ordinary path, not a rare one.
func (s *Search) Delete(ctx context.Context, id int) error {
	_, err := s.client.Collection(s.collectionName).Document(strconv.Itoa(id)).Delete(ctx)
	var httpErr *typesense.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == 404 {
		return nil
	}
	return err
}

// Import bulk-upserts a batch. Typesense reports per-document failures in the
// response rather than as an error, so both are checked.
func (s *Search) Import(ctx context.Context, docs []*Doc) error {
	if len(docs) == 0 {
		return nil
	}
	batch := make([]any, 0, len(docs))
	for _, d := range docs {
		batch = append(batch, tsDoc(d))
	}
	action := "upsert"
	results, err := s.client.Collection(s.collectionName).Documents().Import(ctx, batch,
		&api.ImportDocumentsParams{Action: (*api.IndexAction)(&action)})
	if err != nil {
		return err
	}
	var failed int
	for _, r := range results {
		if r != nil && !r.Success {
			if failed == 0 {
				logf("import error: %s", r.Error)
			}
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d documents failed to index", failed, len(docs))
	}
	return nil
}

// The three views the archive offers. They partition it: everything is in
// exactly one of "not trashed" and "trashed", and Failed is the slice of the
// first that did not process. Failed documents therefore appear under All as
// well — All means the whole archive, not the part of it that worked.
const (
	ViewAll    = ""
	ViewFailed = "failed"
	ViewTrash  = "trash"
)

// Query is a parsed search request from the URL.
type Query struct {
	Q      string
	Tags   []string // every tag must match, so picking more narrows
	Status string
	// View selects which of the three slices is being looked at. It is a mode
	// rather than a filter — it decides what the archive is, and the filters
	// then narrow that — which is why it is not part of HasFilters and is not
	// cleared by "Clear filters".
	View string
	// Sort names the field: "" for relevance, "added" for when the document
	// arrived, "created" for the date on the document itself. Direction is
	// separate, because which field to order by and which end to start from
	// are two different questions.
	Sort string
	Dir  string // "asc" for oldest first; anything else is newest first
	// Range selects a document-date window: "", "month", "quarter", "year" or
	// "custom". Empty means all time, which is the right default for an
	// archive — a date filter that hides most of the corpus should be asked
	// for, not assumed.
	Range string
	From  string // YYYY-MM, custom range only
	To    string // YYYY-MM, custom range only
	Page  int
}

// Bounds turns the selected range into inclusive created_ts limits. Zero means
// unbounded on that side. Document dates are month-precision timestamps, so a
// bound at the start of the To month covers the whole of it.
func (q Query) Bounds(now time.Time) (lo, hi int64) {
	month := func(t time.Time) int64 {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	}
	switch q.Range {
	case "month":
		return month(now.AddDate(0, -1, 0)), 0
	case "quarter":
		return month(now.AddDate(0, -3, 0)), 0
	case "year":
		return month(now.AddDate(-1, 0, 0)), 0
	case "custom":
		return parseDateTS(q.From), parseDateTS(q.To)
	}
	return 0, 0
}

// SortField resolves what the results are actually ordered by, which is not
// always what the URL says: with no field chosen, a search orders by relevance
// and a bare listing orders by arrival.
func (q Query) SortField() string {
	switch q.Sort {
	case "added", "created":
		return q.Sort
	}
	if strings.TrimSpace(q.Q) != "" {
		return "relevance"
	}
	return "added"
}

func (q Query) Descending() bool { return q.Dir != "asc" }

// HasDateFilter reports whether the range actually constrains anything, which
// is what the UI uses to decide if "Clear" is worth offering.
func (q Query) HasDateFilter() bool {
	lo, hi := q.Bounds(time.Now())
	return lo > 0 || hi > 0
}

// HasFilters reports whether anything narrows the archive other than the
// search term itself. Both the "Clear filters" affordance and the decision to
// look up the archive-wide count hang on this, so it is answered in one place:
// a filter dimension added here is added everywhere.
func (q Query) HasFilters() bool {
	return len(q.Tags) > 0 || q.Status != "" || q.HasDateFilter()
}

// Result is everything a results page needs, already shaped for templates.
type Result struct {
	Hits   []Hit
	Found  int
	Page   int
	Pages  int
	Facets map[string][]FacetValue
}

// Hit carries per-field highlights. A match can land in the title, the
// summary or the body, and the card should show where it actually landed
// rather than always excerpting the body.
type Hit struct {
	Doc     *Doc
	Title   template.HTML // the title, marked up if the match was there
	Summary template.HTML // summary, marked up if matched, plain otherwise
	Snippet template.HTML // body excerpt, only when the body matched
}

type FacetValue struct {
	Value string
	Count int
}

const perPage = 24

// highlight sentinels. Typesense injects its markers into raw document text
// without escaping the surroundings, so we ask for markers that cannot occur
// in real text, escape everything, and only then turn them into real tags.
const (
	hlStart = "@@docovia-hl@@"
	hlEnd   = "@@/docovia-hl@@"
)

func escapeFilterValue(v string) string {
	return strings.ReplaceAll(v, `"`, `\"`)
}

func (s *Search) Query(ctx context.Context, q Query) (*Result, error) {
	res, err := s.query(ctx, q, perPage, false)
	if err != nil {
		return nil, err
	}
	out := &Result{Page: max(q.Page, 1), Facets: map[string][]FacetValue{}}
	if res.Found != nil {
		out.Found = *res.Found
		out.Pages = (out.Found + perPage - 1) / perPage
	}
	if res.Hits != nil {
		for _, h := range *res.Hits {
			if h.Document == nil {
				continue
			}
			out.Hits = append(out.Hits, newHit(docFromMap(*h.Document), h))
		}
	}
	if res.FacetCounts != nil {
		for _, fc := range *res.FacetCounts {
			if fc.FieldName == nil || fc.Counts == nil {
				continue
			}
			var vals []FacetValue
			for _, c := range *fc.Counts {
				if c.Value == nil || c.Count == nil || *c.Value == "" {
					continue
				}
				vals = append(vals, FacetValue{Value: *c.Value, Count: *c.Count})
			}
			out.Facets[*fc.FieldName] = vals
		}
	}
	return out, nil
}

// query builds and runs the search. Separated from Query so the bulk download
// can page through the same filters at its own size without a second, drifting
// copy of how a filter is spelled. idsOnly strips the request down to what
// that caller actually reads.
func (s *Search) query(ctx context.Context, q Query, perPage int, idsOnly bool) (*api.SearchResult, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	text := strings.TrimSpace(q.Q)
	if text == "" {
		text = "*"
	}

	var filters []string
	add := func(field, value string) {
		if value != "" {
			filters = append(filters, fmt.Sprintf(`%s:="%s"`, field, escapeFilterValue(value)))
		}
	}
	// The view goes on first, and every query gets one whether it asked or not.
	// That is what makes the trash actually hidden: the index, the status page's
	// failed table and the bulk download all build their requests through Query,
	// so none of them can forget to exclude a document that is on its way out.
	if q.View == ViewTrash {
		filters = append(filters, "delete_after_ts:>0")
	} else {
		filters = append(filters, "delete_after_ts:=0")
		if q.View == ViewFailed {
			filters = append(filters, "status:="+StatusFailed)
		}
	}

	// Chained rather than combined into one list filter, because chaining is
	// what gives AND: each selected tag narrows further.
	for _, t := range q.Tags {
		add("tags", t)
	}
	add("status", q.Status)

	// A document with no date has created_ts 0, so any lower bound excludes
	// undated documents. That is the honest reading of "documents from the
	// last year" — but it is why all-time stays the default.
	if lo, hi := q.Bounds(time.Now()); lo > 0 || hi > 0 {
		if lo > 0 {
			filters = append(filters, fmt.Sprintf("created_ts:>=%d", lo))
		}
		if hi > 0 {
			filters = append(filters, fmt.Sprintf("created_ts:<=%d", hi))
		}
	}

	dir := "desc"
	if !q.Descending() {
		dir = "asc"
	}
	// Relevance still falls back to arrival for ties, so equally-good matches
	// come out newest first rather than in whatever order the index holds.
	sortBy := "_text_match:desc,added_ts:desc"
	switch q.SortField() {
	case "added":
		sortBy = "added_ts:" + dir
	case "created":
		sortBy = "created_ts:" + dir
	}

	params := &api.SearchCollectionParams{
		Q:              pointer.String(text),
		QueryBy:        pointer.String("title,summary,content,tags,original_name"),
		QueryByWeights: pointer.String("10,6,4,8,2"),
		PerPage:        pointer.Int(perPage),
		Page:           pointer.Int(q.Page),
	}
	if idsOnly {
		// The bulk download reads nothing but the id. Asking for whole
		// documents and a full facet count to build a []int meant dragging up
		// to two thousand document bodies across the wire to throw them away.
		params.IncludeFields = pointer.String("id")
	} else {
		// The body is matched against but never rendered from a list: the card
		// shows the highlighted snippet, which Typesense returns separately. So
		// every hit was shipping its whole indexed text to be decoded and
		// dropped, on a page that reloads itself every few seconds.
		params.ExcludeFields = pointer.String("content")
		params.FacetBy = pointer.String("tags,status")
		params.MaxFacetValues = pointer.Int(200)
		params.HighlightFields = pointer.String("title,summary,content")
		params.HighlightStartTag = pointer.String(hlStart)
		params.HighlightEndTag = pointer.String(hlEnd)
		params.HighlightAffixNumTokens = pointer.Int(8)
	}
	if len(filters) > 0 {
		params.FilterBy = pointer.String(strings.Join(filters, " && "))
	}
	params.SortBy = pointer.String(sortBy)

	return s.client.Collection(s.collectionName).Documents().Search(ctx, params)
}

func newHit(doc *Doc, h api.SearchResultHit) Hit {
	hit := Hit{Doc: doc}

	marked := map[string]template.HTML{}
	if h.Highlights != nil {
		for _, hl := range *h.Highlights {
			if hl.Field == nil || hl.Snippet == nil || *hl.Snippet == "" {
				continue
			}
			marked[*hl.Field] = renderMarks(*hl.Snippet)
		}
	}

	hit.Title = marked["title"]
	if hit.Title == "" {
		hit.Title = template.HTML(template.HTMLEscapeString(doc.Title))
	}
	hit.Summary = marked["summary"]
	if hit.Summary == "" {
		hit.Summary = template.HTML(template.HTMLEscapeString(clip(doc.Summary, 240)))
	}
	hit.Snippet = marked["content"]
	return hit
}

// renderMarks turns a Typesense snippet into safe HTML. Everything is escaped
// first and only the sentinel markers become tags, so document text can never
// inject markup into the page.
func renderMarks(snippet string) template.HTML {
	safe := template.HTMLEscapeString(snippet)
	safe = strings.ReplaceAll(safe, template.HTMLEscapeString(hlStart), "<mark>")
	safe = strings.ReplaceAll(safe, template.HTMLEscapeString(hlEnd), "</mark>")
	return template.HTML(safe)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// truncateBytes cuts s to at most max bytes without splitting a character. The
// limit stays a byte limit — these are payload caps, and a document in a script
// that spends three bytes a character really does fit fewer of them — but a cut
// that lands inside a rune leaves a string that is not valid UTF-8, and the JSON
// encoder downstream rewrites the stray bytes to U+FFFD instead of complaining.
// So the cut walks back to the last boundary at or before the limit.
//
// Anything already within the limit comes back untouched, which is the common
// case and worth not allocating for.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	s = s[:max]
	// DecodeLastRuneInString reports (RuneError, 1) for a byte that cannot end a
	// valid encoding. U+FFFD is also a rune real text carries — this app counts
	// them on purpose in garbageRatio — so it is the size, not the rune, that
	// says the cut landed mid-character.
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// tailBytes is truncateBytes taken from the other end: at most max bytes off the
// end of s, still landing on a character boundary. It cannot be the same walk,
// because keeping a suffix means the damaged rune is at the *start* of what is
// kept, so the fix is to move the start forward to the next boundary rather than
// backward — dropping the continuation bytes whose leading byte was cut away.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	s = s[len(s)-max:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// AllIDs walks every page of a query and returns the ids in order. Used by
// the bulk download, where "all matching" has to mean all of them and not
// just the two dozen on screen.
func (s *Search) AllIDs(ctx context.Context, q Query, limit int) ([]int, error) {
	const batch = 250
	var out []int
	for page := 1; ; page++ {
		q.Page = page
		res, err := s.query(ctx, q, batch, true)
		if err != nil {
			return out, err
		}
		if res.Hits == nil || len(*res.Hits) == 0 {
			return out, nil
		}
		for _, h := range *res.Hits {
			if h.Document == nil {
				continue
			}
			if id, ok := hitID(*h.Document); ok {
				out = append(out, id)
			}
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
		if len(*res.Hits) < batch {
			return out, nil
		}
	}
}

// ViewCounts is how many documents each of the three views holds. Archive-wide
// rather than narrowed by whatever is currently searched or filtered: the
// control says how big each view is, so that a search finding nothing in Failed
// still shows there is nothing there rather than reporting the search.
//
// Three metadata lookups, each asking for zero results so Typesense counts
// rather than fetches, which is cheap enough to pay on every index render. The
// All figure doubles as the size of the searchable archive, which is what the
// search placeholder used to make a fourth lookup for. A multi_search could
// fold the three into one round trip later; it is not worth the extra shape
// yet.
func (s *Search) ViewCounts(ctx context.Context) (all, failed, trash int, err error) {
	count := func(filter string) (int, error) {
		res, err := s.client.Collection(s.collectionName).Documents().Search(ctx, &api.SearchCollectionParams{
			Q:        pointer.String("*"),
			QueryBy:  pointer.String("title"),
			FilterBy: pointer.String(filter),
			PerPage:  pointer.Int(0),
		})
		if err != nil {
			return 0, err
		}
		if res.Found == nil {
			return 0, nil
		}
		return *res.Found, nil
	}
	if all, err = count("delete_after_ts:=0"); err != nil {
		return 0, 0, 0, err
	}
	if failed, err = count("delete_after_ts:=0 && status:=" + StatusFailed); err != nil {
		return 0, 0, 0, err
	}
	if trash, err = count("delete_after_ts:>0"); err != nil {
		return 0, 0, 0, err
	}
	return all, failed, trash, nil
}

// ExpiredTrashIDs lists documents whose purge deadline has passed, for the
// sweeper. Paged the way AllIDs is, and asking for nothing but the id, because
// a sweep after a large bulk trash can match thousands of documents and none of
// their contents are wanted here. The whole list is read before anything is
// deleted, so the pages are walking a set that is not changing underneath them.
func (s *Search) ExpiredTrashIDs(ctx context.Context, now int64) ([]int, error) {
	const batch = 250
	var out []int
	for page := 1; ; page++ {
		res, err := s.client.Collection(s.collectionName).Documents().Search(ctx, &api.SearchCollectionParams{
			Q:             pointer.String("*"),
			QueryBy:       pointer.String("title"),
			FilterBy:      pointer.String(fmt.Sprintf("delete_after_ts:>0 && delete_after_ts:<=%d", now)),
			IncludeFields: pointer.String("id"),
			PerPage:       pointer.Int(batch),
			Page:          pointer.Int(page),
		})
		if err != nil {
			return out, err
		}
		if res.Hits == nil || len(*res.Hits) == 0 {
			return out, nil
		}
		for _, h := range *res.Hits {
			if h.Document == nil {
				continue
			}
			if id, ok := hitID(*h.Document); ok {
				out = append(out, id)
			}
		}
		if len(*res.Hits) < batch {
			return out, nil
		}
	}
}

// TokenTotals sums model usage across every indexed document. Typesense
// returns sum/avg alongside a numeric facet, so this is one cheap query rather
// than a scan — and because it reads the documents rather than a counter in
// memory, the figure survives a restart and counts work done by earlier runs.
//
// The cents are what each document recorded at the time it was tagged, so the
// total is spend that actually happened rather than today's prices applied to
// yesterday's tokens: it stays correct even after the price table changes.
func (s *Search) TokenTotals(ctx context.Context) (in, out int64, cents float64, docs int, err error) {
	res, err := s.client.Collection(s.collectionName).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:              pointer.String("*"),
		QueryBy:        pointer.String("title"),
		FilterBy:       pointer.String("llm_in:>0"),
		FacetBy:        pointer.String("llm_in,llm_out,llm_cents"),
		MaxFacetValues: pointer.Int(1),
		PerPage:        pointer.Int(0),
	})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if res.Found != nil {
		docs = *res.Found
	}
	if res.FacetCounts == nil {
		return 0, 0, 0, docs, nil
	}
	for _, fc := range *res.FacetCounts {
		if fc.FieldName == nil || fc.Stats == nil || fc.Stats.Sum == nil {
			continue
		}
		switch *fc.FieldName {
		case "llm_in":
			in = int64(*fc.Stats.Sum)
		case "llm_out":
			out = int64(*fc.Stats.Sum)
		case "llm_cents":
			cents = *fc.Stats.Sum
		}
	}
	return in, out, cents, docs, nil
}

// Get fetches one document by id. This is a direct lookup, not a search.
func (s *Search) Get(ctx context.Context, id int) (*Doc, error) {
	raw, err := s.client.Collection(s.collectionName).Document(strconv.Itoa(id)).Retrieve(ctx)
	if err != nil {
		return nil, err
	}
	return docFromMap(raw), nil
}

// Vocabulary returns the most common values of a faceted field, used to keep
// the heuristics and the LLM prompt anchored to terms already in use.
func (s *Search) Vocabulary(ctx context.Context, field string, limit int) ([]string, error) {
	key := field + "/" + strconv.Itoa(limit)
	if v, ok := s.cachedVocab(key); ok {
		return v, nil
	}
	res, err := s.client.Collection(s.collectionName).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:              pointer.String("*"),
		QueryBy:        pointer.String("title"),
		FacetBy:        pointer.String(field),
		MaxFacetValues: pointer.Int(limit),
		PerPage:        pointer.Int(0),
	})
	if err != nil {
		return nil, err
	}
	var out []string
	if res.FacetCounts == nil {
		return out, nil
	}
	for _, fc := range *res.FacetCounts {
		if fc.FieldName == nil || *fc.FieldName != field || fc.Counts == nil {
			continue
		}
		for _, c := range *fc.Counts {
			if c.Value != nil && *c.Value != "" {
				out = append(out, *c.Value)
			}
		}
	}
	s.storeVocab(key, out)
	return out, nil
}

// cachedVocab hands back a copy: the list goes on to a template and into a
// prompt, and a cache that shares its slice is one careless append away from
// rewriting itself.
func (s *Search) cachedVocab(key string) ([]string, bool) {
	s.vocabMu.Lock()
	defer s.vocabMu.Unlock()
	e, ok := s.vocab[key]
	if !ok || time.Since(e.at) >= vocabTTL {
		return nil, false
	}
	return slices.Clone(e.values), true
}

func (s *Search) storeVocab(key string, values []string) {
	s.vocabMu.Lock()
	defer s.vocabMu.Unlock()
	if s.vocab == nil {
		s.vocab = map[string]vocabEntry{}
	}
	s.vocab[key] = vocabEntry{values: slices.Clone(values), at: time.Now()}
}

// hitID recovers our integer id from a Typesense hit, which stores ids as
// strings. Written once because getting it wrong fails quietly: the id comes
// back zero and the document is skipped rather than reported.
func hitID(m map[string]any) (int, bool) {
	raw, ok := m["id"].(string)
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(raw)
	return id, err == nil
}

// docFromMap rebuilds a Doc from a Typesense hit. Every display field is
// stored in the index, so pages render without touching the disk.
func docFromMap(m map[string]any) *Doc {
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	num := func(k string) int64 {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		}
		return 0
	}
	// Separate from num because that one truncates to int64, which would round
	// every sub-cent document down to nothing.
	flt := func(k string) float64 {
		v, _ := m[k].(float64)
		return v
	}
	boolean := func(k string) bool {
		v, _ := m[k].(bool)
		return v
	}

	d := &Doc{
		OriginalName:  str("original_name"),
		Title:         str("title"),
		Summary:       str("summary"),
		Status:        str("status"),
		Error:         str("error"),
		FailedStage:   str("failed_stage"),
		SHA256:        str("sha256"),
		CreatedDate:   str("created_date"),
		OCRSource:     str("ocr_source"),
		Content:       str("content"),
		AddedTS:       num("added_ts"),
		LLMIn:         num("llm_in"),
		LLMOut:        num("llm_out"),
		LLMCents:      flt("llm_cents"),
		TextTS:        num("text_ts"),
		EnrichedTS:    num("enriched_ts"),
		DeleteAfterTS: num("delete_after_ts"),
		FileSize:      num("file_size"),
		PageCount:     int(num("page_count")),
		Signed:        boolean("signed"),
		Encrypted:     boolean("encrypted"),
		Enriched:      boolean("enriched"),
		NativeText:    boolean("native_text"),
	}
	d.ID, _ = hitID(m)
	if tags, ok := m["tags"].([]any); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok {
				d.Tags = append(d.Tags, s)
			}
		}
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}
	return d
}

// FindByHashWait is FindByHash with the answer required. A lookup that errors
// is not evidence of absence, and treating it as such is how an outage turned a
// duplicate into a second document: with Typesense unreachable, dropping the
// same bytes twice logged two lookup failures and ingested both files, leaving
// two documents sharing one sha256. So the lookup is retried until the index
// answers, on the same contract as persist — Typesense is required
// infrastructure here, and ingestion stalling visibly while it is down is the
// intended behaviour. Dedup runs once per ingested file, never in a loop, so
// waiting costs one file rather than a hot path.
//
// The only outcome other than an answer is ctx.Err(), which is a canceled
// upload or shutdown. Callers must branch on the error before reading found:
// the returned tuple on that path is the zero one, and it means "not asked",
// not "not present".
//
// what names the file in the log rather than the sha, because "dedup lookup for
// scan-2026-07.pdf: connection refused" is the line that tells whoever is
// reading it which document is stuck; a hash prefix would need a second lookup
// to mean anything, and the caller has the name in hand at both sites.
func (s *Search) FindByHashWait(ctx context.Context, sha, what string) (int, bool, error) {
	return findByHashWait(ctx, retryInitial, retryMax, fmt.Sprintf("dedup lookup for %s", what),
		func() (int, bool, error) { return s.FindByHash(ctx, sha) })
}

// findByHashWait is the decision behind FindByHashWait with the lookup and the
// backoff passed in: what has to be true is that a failure never becomes "not a
// duplicate", which is a property of this loop rather than of Typesense, and a
// test can only pin it here.
func findByHashWait(ctx context.Context, initial, max time.Duration, what string, lookup func() (int, bool, error)) (int, bool, error) {
	var (
		id    int
		found bool
	)
	err := retryUntil(ctx, initial, max, what, func() error {
		var err error
		id, found, err = lookup()
		return err
	})
	if err != nil {
		return 0, false, err
	}
	return id, found, nil
}

// FindByHash returns the id of an existing document with this content hash.
// Callers deciding whether to ingest want FindByHashWait: this one reports a
// lookup failure as an error, and an error here is not an absence.
func (s *Search) FindByHash(ctx context.Context, sha string) (int, bool, error) {
	res, err := s.client.Collection(s.collectionName).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:        pointer.String("*"),
		FilterBy: pointer.String("sha256:=" + sha),
		PerPage:  pointer.Int(1),
	})
	if err != nil {
		return 0, false, err
	}
	if res.Hits == nil || len(*res.Hits) == 0 {
		return 0, false, nil
	}
	hit := (*res.Hits)[0]
	if hit.Document == nil {
		return 0, false, nil
	}
	id, ok := hitID(*hit.Document)
	return id, ok, nil
}
