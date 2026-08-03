package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// JournalEvent is one line of journal.jsonl: something that happened to a
// document, written when it happened. The sidecars say what is true now; the
// journal says what was done.
type JournalEvent struct {
	TS     int64  `json:"ts"`
	Event  string `json:"event"`
	DocID  int    `json:"doc_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// journal appends one event. It is the writer, not the way to state an event:
// record below is the only caller, so that no site can record something in one
// place and not the other. Every caller is doing something else that matters
// more — ingesting a document, answering a request — so this is best effort in
// the strongest sense: a journal that cannot be written is a journal that is
// missing a line, never a document that failed to process.
//
// The lock covers the whole open-write-close, because two workers appending at
// once is the ordinary case here and a line interleaved with another is a line
// nothing can parse. Opening per event rather than holding a file handle keeps
// the journal to something that can be rotated, truncated or deleted from
// underneath a running server without wedging it.
func (a *App) journal(event string, docID int, name, detail string) {
	b, err := json.Marshal(JournalEvent{
		TS: time.Now().Unix(), Event: event, DocID: docID, Name: name, Detail: detail,
	})
	if err != nil {
		logf("journal %s: %v", event, err)
		return
	}

	a.journalMu.Lock()
	defer a.journalMu.Unlock()
	f, err := os.OpenFile(a.store.JournalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logf("journal %s: %v", event, err)
		return
	}
	defer f.Close()
	// One Write for the line and its newline together, so a short write cannot
	// split the record itself.
	if _, err := f.Write(append(b, '\n')); err != nil {
		logf("journal %s: %v", event, err)
	}
}

// record states one document-lifecycle event: the journal line that outlives
// the process, and the log line someone watching a terminal during an import
// reads while it is happening. Both are derived from the same four values,
// which is the entire point — every site below used to write the two by hand,
// in two formats, and nothing but care kept them saying the same thing.
//
// The journal is the durable record and the log is a view of it, so anything
// worth telling the terminal belongs in detail rather than in a second call
// beside this one. logf stays for diagnostics that are not events: a retry, a
// fallback, a rate-limit pause. Those have no document behind them and would
// bury the journal, which is meant to stay about two lines per document.
func (a *App) record(event string, docID int, name, detail string) {
	a.journal(event, docID, name, detail)
	// The line is already rendered, so it is passed as data rather than as a
	// format: a title or an error message containing a % must not be read as a
	// verb.
	logf("%s", recordLine(event, docID, name, detail))
}

// recordLine renders one event for the log: what it happened to, what happened,
// and what there is to say about it.
//
// The subject is the document when there is one and the file's name when there
// is not — a rejected file never becomes a document, and its name is the only
// handle anyone will ever have on it. When there is both, the name follows the
// event ("DOC-12 duplicate scan_001.pdf"), because the id says which document
// this is about and the name says which file arrived. An event with nothing to
// add is a bare line rather than one ending in a colon with nothing after it.
func recordLine(event string, docID int, name, detail string) string {
	subject := name
	if docID > 0 {
		subject = docCode(docID)
	}
	line := strings.TrimSpace(subject + " " + event)
	if docID > 0 && name != "" {
		line += " " + name
	}
	if detail != "" {
		line += ": " + detail
	}
	return line
}

// journalTailBytes is how much of the journal the status page will ever read.
// The page polls every few seconds while anything is processing, so the cost of
// showing recent history must not grow with the length of that history.
const journalTailBytes = 64 << 10

// readJournalTail returns the last n events, newest first, reading at most
// maxBytes from the end of the file. Seeking into the middle of a line is the
// normal outcome of that seek, so the first line read is dropped whenever the
// read did not start at the beginning of the file.
//
// A missing or unreadable journal is an empty history rather than an error:
// nothing on the status page is worth failing a page render over, and the file
// does not exist at all until the first event.
func readJournalTail(path string, maxBytes int64, n int) []JournalEvent {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("reading journal: %v", err)
		}
		return nil
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		logf("reading journal: %v", err)
		return nil
	}
	start := fi.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		logf("reading journal: %v", err)
		return nil
	}
	b, err := io.ReadAll(f)
	if err != nil {
		logf("reading journal: %v", err)
		return nil
	}

	lines := bytes.Split(b, []byte{'\n'})
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}

	out := make([]JournalEvent, 0, min(len(lines), n))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev JournalEvent
		// A line we cannot parse is skipped rather than reported. The journal is
		// append-only and its writer is one line per Write, so the only lines
		// that can be malformed are ones a crash left half-written.
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	slices.Reverse(out)
	return out
}

// editDetail says what one save actually changed, in one line:
//
//	title: "scan_001" → "Northwind Statement"; tags: +car −medical; date: 2026-01 → 2026-02
//
// An empty string means nothing changed, and the caller writes no event at all.
// The document page autosaves, so a field that was focused and left alone posts
// a save like any other; recording those would bury the real edits.
func editDetail(before, after *Doc) string {
	var parts []string
	if before.Title != after.Title {
		parts = append(parts, fmt.Sprintf("title: %q → %q", before.Title, after.Title))
	}
	if d := tagDiff(before.Tags, after.Tags); d != "" {
		parts = append(parts, "tags: "+d)
	}
	if before.CreatedDate != after.CreatedDate {
		parts = append(parts, fmt.Sprintf("date: %s → %s",
			dateOrNone(before.CreatedDate), dateOrNone(after.CreatedDate)))
	}
	return strings.Join(parts, "; ")
}

// tagDiff names what was added and what was taken away rather than printing
// both lists. The two lists are nearly always the same list with one entry
// different, and that entry is the whole content of the event.
func tagDiff(before, after []string) string {
	var out []string
	for _, t := range after {
		if !slices.Contains(before, t) {
			out = append(out, "+"+t)
		}
	}
	for _, t := range before {
		if !slices.Contains(after, t) {
			// U+2212 MINUS SIGN rather than a hyphen, so a removal cannot be
			// read as part of the tag name beside it.
			out = append(out, "−"+t)
		}
	}
	return strings.Join(out, " ")
}

// dateOrNone names an absent date. "date:  → 2026-02" reads as a rendering
// fault rather than as a document being dated for the first time.
func dateOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
