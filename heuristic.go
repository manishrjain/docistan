package main

import "context"

// applyHeuristics fills in metadata without calling an LLM and scores how
// confident we are in the result. Implemented in M3; the zero score means the
// enrich gate currently treats every document as a candidate.
func applyHeuristics(ctx context.Context, app *App, doc *Doc, archive string) {
	if doc.Title == "" {
		doc.Title = doc.OriginalName
	}
	if doc.Tags == nil {
		doc.Tags = []string{}
	}
}
