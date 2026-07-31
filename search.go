package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/typesense/typesense-go/v3/typesense"
	"github.com/typesense/typesense-go/v3/typesense/api"
	"github.com/typesense/typesense-go/v3/typesense/api/pointer"
)

const collectionName = "documents"

// contentIndexLimit caps how much text is handed to Typesense. The sidecar
// always keeps the full text; this only bounds the index.
const contentIndexLimit = 400 << 10

type Search struct {
	client *typesense.Client
}

func NewSearch(url, key string) *Search {
	return &Search{client: typesense.NewClient(
		typesense.WithServer(url),
		typesense.WithAPIKey(key),
		typesense.WithConnectionTimeout(10*time.Second),
	)}
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

func schema() *api.CollectionSchema {
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
		Name: collectionName,
		Fields: []api.Field{
			f("title", "string"),
			f("content", "string"),
			f("original_name", "string"),
			f("tags", "string[]", facet),
			f("correspondent", "string", facet),
			f("doc_type", "string", facet),
			f("status", "string", facet),
			f("sha256", "string"),
			f("created_ts", "int64", sortable),
			f("added_ts", "int64", sortable),
			f("confidence", "int32", sortable),
			f("created_date", "string", stored),
			f("page_count", "int32", stored),
			f("file_size", "int64", stored),
			f("ocr_source", "string", stored),
			f("signed", "bool", stored),
			f("enriched", "bool", stored),
		},
		DefaultSortingField: pointer.String("added_ts"),
	}
}

// EnsureFreshCollection drops any existing collection and recreates it. The
// index is rebuilt from sidecars on every boot, so starting clean guarantees
// the live schema always matches this code and makes schema changes free.
func (s *Search) EnsureFreshCollection(ctx context.Context) error {
	if _, err := s.client.Collection(collectionName).Delete(ctx); err != nil {
		// A missing collection is the normal case on a cold start.
		var apiErr *typesense.HTTPError
		if !errors.As(err, &apiErr) || apiErr.Status != 404 {
			logf("dropping existing collection: %v", err)
		}
	}
	_, err := s.client.Collections().Create(ctx, schema())
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
		"content":       content,
		"original_name": d.OriginalName,
		"tags":          tags,
		"correspondent": d.Correspondent,
		"doc_type":      d.DocType,
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
	}
}

func (s *Search) Upsert(ctx context.Context, d *Doc) error {
	_, err := s.client.Collection(collectionName).Documents().Upsert(ctx, tsDoc(d), &api.DocumentIndexParameters{})
	return err
}

func (s *Search) Delete(ctx context.Context, id int) error {
	_, err := s.client.Collection(collectionName).Document(strconv.Itoa(id)).Delete(ctx)
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
	results, err := s.client.Collection(collectionName).Documents().Import(ctx, batch,
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

// FindByHash returns the id of an existing document with this content hash.
func (s *Search) FindByHash(ctx context.Context, sha string) (int, bool, error) {
	res, err := s.client.Collection(collectionName).Documents().Search(ctx, &api.SearchCollectionParams{
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
