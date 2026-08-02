package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

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
			f("created_ts", "int64", sortable),
			f("added_ts", "int64", sortable),
			f("confidence", "int32", sortable),
			// Faceted purely for the stats Typesense returns with a facet, which
			// is what lets the archive total outlive the process.
			f("llm_in", "int64", facet),
			f("llm_out", "int64", facet),
			f("created_date", "string", stored),
			f("page_count", "int32", stored),
			f("file_size", "int64", stored),
			f("ocr_source", "string", stored),
			f("signed", "bool", stored),
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
	content := d.Content
	if len(content) > contentIndexLimit {
		content = content[:contentIndexLimit]
	}
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
		"created_ts":    d.CreatedTS,
		"added_ts":      d.AddedTS,
		"confidence":    d.Confidence,
		"created_date":  d.CreatedDate,
		"page_count":    d.PageCount,
		"file_size":     d.FileSize,
		"ocr_source":    d.OCRSource,
		"signed":        d.Signed,
		"enriched":      d.Enriched,
		"native_text":   d.NativeText,
		"llm_in":        d.LLMIn,
		"llm_out":       d.LLMOut,
		"text_ts":       d.TextTS,
		"enriched_ts":   d.EnrichedTS,
	}
}

func (s *Search) Upsert(ctx context.Context, d *Doc) error {
	_, err := s.client.Collection(s.collectionName).Documents().Upsert(ctx, tsDoc(d), &api.DocumentIndexParameters{})
	return err
}

func (s *Search) Delete(ctx context.Context, id int) error {
	_, err := s.client.Collection(s.collectionName).Document(strconv.Itoa(id)).Delete(ctx)
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

// Query is a parsed search request from the URL.
type Query struct {
	Q      string
	Tags   []string // every tag must match, so picking more narrows
	Status string
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

// Result is everything a results page needs, already shaped for templates.
type Result struct {
	Hits    []Hit
	Found   int
	Page    int
	Pages   int
	PerPage int
	Facets  map[string][]FacetValue
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
	hlStart = "@@docistan-hl@@"
	hlEnd   = "@@/docistan-hl@@"
)

func escapeFilterValue(v string) string {
	return strings.ReplaceAll(v, `"`, `\"`)
}

func (s *Search) Query(ctx context.Context, q Query) (*Result, error) {
	res, err := s.query(ctx, q, perPage)
	if err != nil {
		return nil, err
	}
	out := &Result{Page: max(q.Page, 1), PerPage: perPage, Facets: map[string][]FacetValue{}}
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
// copy of how a filter is spelled.
func (s *Search) query(ctx context.Context, q Query, perPage int) (*api.SearchResult, error) {
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
		Q:                       pointer.String(text),
		QueryBy:                 pointer.String("title,summary,content,tags,original_name"),
		QueryByWeights:          pointer.String("10,6,4,8,2"),
		FacetBy:                 pointer.String("tags,status"),
		MaxFacetValues:          pointer.Int(200),
		HighlightFields:         pointer.String("title,summary,content"),
		HighlightStartTag:       pointer.String(hlStart),
		HighlightEndTag:         pointer.String(hlEnd),
		HighlightAffixNumTokens: pointer.Int(8),
		PerPage:                 pointer.Int(perPage),
		Page:                    pointer.Int(q.Page),
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

// AllIDs walks every page of a query and returns the ids in order. Used by
// the bulk download, where "all matching" has to mean all of them and not
// just the two dozen on screen.
func (s *Search) AllIDs(ctx context.Context, q Query, limit int) ([]int, error) {
	const batch = 250
	var out []int
	for page := 1; ; page++ {
		q.Page = page
		res, err := s.query(ctx, q, batch)
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
			if raw, ok := (*h.Document)["id"].(string); ok {
				if id, err := strconv.Atoi(raw); err == nil {
					out = append(out, id)
				}
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

// Count returns how many documents are indexed, for the search placeholder.
// Asking Typesense for zero results makes this a metadata lookup rather than
// a fetch, so it costs almost nothing on every index render.
func (s *Search) Count(ctx context.Context) (int, error) {
	res, err := s.client.Collection(s.collectionName).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:       pointer.String("*"),
		QueryBy: pointer.String("title"),
		PerPage: pointer.Int(0),
	})
	if err != nil {
		return 0, err
	}
	if res.Found == nil {
		return 0, nil
	}
	return *res.Found, nil
}

// TokenTotals sums model usage across every indexed document. Typesense
// returns sum/avg alongside a numeric facet, so this is one cheap query rather
// than a scan — and because it reads the documents rather than a counter in
// memory, the figure survives a restart and counts work done by earlier runs.
func (s *Search) TokenTotals(ctx context.Context) (in, out int64, docs int, err error) {
	res, err := s.client.Collection(s.collectionName).Documents().Search(ctx, &api.SearchCollectionParams{
		Q:              pointer.String("*"),
		QueryBy:        pointer.String("title"),
		FilterBy:       pointer.String("llm_in:>0"),
		FacetBy:        pointer.String("llm_in,llm_out"),
		MaxFacetValues: pointer.Int(1),
		PerPage:        pointer.Int(0),
	})
	if err != nil {
		return 0, 0, 0, err
	}
	if res.Found != nil {
		docs = *res.Found
	}
	if res.FacetCounts == nil {
		return 0, 0, docs, nil
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
		}
	}
	return in, out, docs, nil
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
	return out, nil
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
	boolean := func(k string) bool {
		v, _ := m[k].(bool)
		return v
	}

	d := &Doc{
		OriginalName: str("original_name"),
		Title:        str("title"),
		Summary:      str("summary"),
		Status:       str("status"),
		SHA256:       str("sha256"),
		CreatedDate:  str("created_date"),
		OCRSource:    str("ocr_source"),
		Content:      str("content"),
		CreatedTS:    num("created_ts"),
		AddedTS:      num("added_ts"),
		LLMIn:        num("llm_in"),
		LLMOut:       num("llm_out"),
		TextTS:       num("text_ts"),
		EnrichedTS:   num("enriched_ts"),
		FileSize:     num("file_size"),
		PageCount:    int(num("page_count")),
		Confidence:   int(num("confidence")),
		Signed:       boolean("signed"),
		Enriched:     boolean("enriched"),
		NativeText:   boolean("native_text"),
	}
	d.ID, _ = strconv.Atoi(str("id"))
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

// FindByHash returns the id of an existing document with this content hash.
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
	raw, ok := (*hit.Document)["id"].(string)
	if !ok {
		return 0, false, nil
	}
	id, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, nil
	}
	return id, true, nil
}
