package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Document status values.
const (
	StatusProcessing = "processing"
	StatusReady      = "ready"
	StatusFailed     = "failed"
	// StatusDeleted marks a tombstone. The sidecar is kept after deletion so
	// the id it consumed can never be handed out to a different document.
	StatusDeleted = "deleted"
)

// trashRetention is how long a document sits in the trash before the sweeper
// destroys it for good. Long enough that a mistake is noticed and undone, short
// enough that the trash is not a second archive.
const trashRetention = 30 * 24 * time.Hour

// failReasonLimit bounds the failure text a result row carries. A wrapped tool
// error runs to a couple of hundred characters and carries an absolute path;
// the row has one line for it and the document page has the whole thing.
const failReasonLimit = 160

// TagNeedsReview is ours, not a description of the document: it marks a
// document the model failed to describe. Named once because three places act
// on it — the prompt withholds it, the failure path adds it, and the timeline
// reads it to explain itself.
const TagNeedsReview = "needs-review"

// The reserved tags. They filter, facet and toggle exactly like the tags a
// person writes, which is the whole point — one query dimension, not two — but
// nobody outside this file may allocate them: they are derived from the
// document's own state at index-write time, so they cannot drift from the
// status and the purge deadline the way a stored copy would.
const (
	TagLocked = "locked"
	TagFailed = "failed"
	TagTrash  = "trash"
)

// stageDecrypt is the pipeline stage whose failure means "nobody here has the
// password" rather than "something went wrong". Named because that distinction
// is what separates TagLocked from TagFailed, and the two files that have to
// agree about it are this one and the pipeline that sets the stage.
const stageDecrypt = "decrypt"

// Locked reports whether the document is waiting for a password rather than
// broken. The unlock form exists only for these, and the failure box changes
// what it says for them, so the question is answered once.
func (d *Doc) Locked() bool {
	return d.Status == StatusFailed && d.FailedStage == stageDecrypt
}

// reservedTags is what a document's own state says about it, in tag form. The
// index carries these alongside the real tags; the sidecar never does.
//
// Locked and failed partition the failures rather than nesting: a
// password-protected document carries locked and not failed, because the two
// pills lead to two different piles of work — one is a password away from
// being readable, the other wants somebody to look at it. Trash is orthogonal
// and rides along with either, which costs nothing: the default query excludes
// trashed documents, so the locked and failed counts never include them unless
// the reader is looking in the trash.
func reservedTags(d *Doc) []string {
	var out []string
	if d.Trashed() {
		out = append(out, TagTrash)
	}
	switch {
	case d.Locked():
		out = append(out, TagLocked)
	case d.Status == StatusFailed:
		out = append(out, TagFailed)
	}
	return out
}

func isReserved(tag string) bool {
	return tag == TagLocked || tag == TagFailed || tag == TagTrash
}

// withoutReserved strips the names a user or the model may not self-allocate.
// Every tag list arriving from outside goes through it — what the model
// returns, what the tag box posts, and what a sidecar written before these
// existed happens to hold — so the only way one of these names reaches the
// index is by being derived there.
func withoutReserved(tags []string) []string {
	return slices.DeleteFunc(slices.Clone(tags), isReserved)
}

// withoutTags copies the list minus the named entries, leaving the caller's
// slice alone — a document's own tags must not change just because we described
// them to the model.
func withoutTags(tags []string, drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return slices.Contains(drop, t) })
}

// OCR source values, recorded so the UI can explain where text came from.
const (
	OCRNone          = "none"           // the PDF already had a text layer
	OCRTesseract     = "tesseract"      // ocrmypdf produced the text layer
	OCRLLM           = "llm"            // vision rescue transcribed it
	OCRSkippedSigned = "skipped-signed" // signed PDF, left untouched
)

// Doc is one document. The JSON form of this struct is the sidecar written to
// docs/<id>.json, which is the source of truth for the whole system.
type Doc struct {
	ID           int      `json:"id"`
	SHA256       string   `json:"sha256"`
	OriginalName string   `json:"original_name"`
	OriginalExt  string   `json:"original_ext"`
	Status       string   `json:"status"`
	Error        string   `json:"error,omitempty"`
	FailedStage  string   `json:"failed_stage,omitempty"`
	AddedTS      int64    `json:"added_ts"`
	FileSize     int64    `json:"file_size"`
	PageCount    int      `json:"page_count"`
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Tags         []string `json:"tags"`
	CreatedDate  string   `json:"created_date"`
	DeletedTS    int64    `json:"deleted_ts,omitempty"`
	// DeleteAfterTS records when a trashed document may be purged, rather than
	// merely that it was trashed. The deadline is the state: it survives
	// restarts without a timer to rebuild, and anyone reading the sidecar sees
	// the date itself instead of having to know the retention period and do the
	// arithmetic. Zero means the document is not in the trash.
	DeleteAfterTS int64  `json:"delete_after_ts,omitempty"`
	OCRSource     string `json:"ocr_source"`
	// NativeText records that the source PDF had its own text layer. That text
	// is exact, so it must never be replaced by a transcription of an image.
	NativeText bool `json:"native_text"`
	// NeedsRescue marks a scanned document whose OCR output still looks poor,
	// making it a candidate for the model to re-read.
	NeedsRescue bool `json:"needs_rescue,omitempty"`
	// Encrypted records that the document arrived password-protected. It says
	// that and only that: the original is normally replaced by the decrypted
	// copy as soon as we can open it, so nothing on disk still carries the
	// encryption and this flag is the only record that a password was ever
	// needed. It stays set for the life of the document — a reprocess reading
	// the now-plain original must not clear it — and the document page's banner
	// is keyed to it. A signed document is the exception that keeps its
	// encrypted original, which is why that banner asks about Signed too.
	Encrypted bool   `json:"encrypted"`
	Signed    bool   `json:"signed"`
	Enriched  bool   `json:"enriched"`
	Content   string `json:"content"`
	// What the model cost for this one document. The tokens are the most
	// recent call's, so they describe the run whose title and tags are on
	// display; the cents are cumulative across every call ever made for this
	// document, because a re-tag really did spend the money a second time.
	// Tokens describe the last run; cents remember every run.
	LLMIn    int64   `json:"llm_in,omitempty"`
	LLMOut   int64   `json:"llm_out,omitempty"`
	LLMCents float64 `json:"llm_cents,omitempty"`
	// When each visible step finished, so the document page can say when it
	// was read and when the model described it rather than only when it
	// arrived. Zero on documents ingested before these were recorded; the
	// timeline simply omits the time in that case.
	TextTS     int64 `json:"text_ts,omitempty"`
	EnrichedTS int64 `json:"enriched_ts,omitempty"`
}

// Gone reports whether this is a tombstone rather than a document: the sidecar
// outlived the files it described, and stays only to keep the id it consumed
// from ever being handed out again.
//
// Four places have to agree about that, so it is written once. The replay that
// rebuilds the index at boot skips these; persist removes them from the index
// instead of writing them to it; permanent deletion refuses to run a second
// time; and every handler that loads a document in order to change it answers
// 404 — the same answer the document page already gives, because that page
// reads from the index and tombstones are not in it.
func (d *Doc) Gone() bool { return d.Status == StatusDeleted }

// Trashed reports whether the document is in the trash. Every view and the
// enrichment queue ask this question, so it is answered in one place rather
// than by each of them remembering which way the comparison goes.
//
// This is not Gone: a trashed document still has all of its files and is still
// in the index, which is what the trash view lists.
func (d *Doc) Trashed() bool { return d.DeleteAfterTS > 0 }

// Trash moves the document to the trash by setting its purge deadline. Nothing
// on disk is touched: the original, the archival copy, the thumbnail and the
// extracted text all stay exactly where they are, which is what makes Restore
// lossless rather than a best effort.
//
// now is a parameter so the transition can be exercised without waiting a
// month for it.
func (d *Doc) Trash(now time.Time) { d.DeleteAfterTS = now.Add(trashRetention).Unix() }

// Restore takes it back out. Clearing the deadline is the whole operation —
// there is nothing to put back.
func (d *Doc) Restore() { d.DeleteAfterTS = 0 }

// duePurge is the sweeper's candidate test: in the trash at all, and past its
// deadline. It is a function rather than an expression written twice because
// the index filter and the sidecar re-check that follows it have to agree about
// what "due" means, and only one of those two can be tested directly.
func duePurge(deleteAfterTS, now int64) bool {
	return deleteAfterTS > 0 && deleteAfterTS <= now
}

// FailReason is the one-line explanation a failed document carries on a result
// row. The pipeline's error is a wrapped tool message and can run long, so it
// is bounded here; the document page shows it whole.
func (d *Doc) FailReason() string {
	if d.Status != StatusFailed {
		return ""
	}
	if msg := strings.TrimSpace(d.Error); msg != "" {
		return clip(msg, failReasonLimit)
	}
	// A failure with no message still has to say something, or the row would
	// carry a badge and no reason for it.
	if d.FailedStage != "" {
		return "failed at " + d.FailedStage
	}
	return "processing failed"
}

// Store owns the data directory. Sidecars are the durable state; the only
// in-memory state is the id counter and the set of hashes being ingested.
type Store struct {
	dir string

	mu       sync.Mutex
	nextID   int
	inflight map[string]bool
}

func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir, inflight: map[string]bool{}}
	for _, sub := range []string{"ingest", "docs", "originals", "archive", "thumbs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	if err := s.initNextID(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.dir}, parts...)...)
}

func (s *Store) DocPath(id int) string     { return s.path("docs", strconv.Itoa(id)+".json") }
func (s *Store) ArchivePath(id int) string { return s.path("archive", strconv.Itoa(id)+".pdf") }
func (s *Store) ThumbPath(id int) string   { return s.path("thumbs", strconv.Itoa(id)+".jpg") }
func (s *Store) IngestDir() string         { return s.path("ingest") }
func (s *Store) ArchiveDir() string        { return s.path("archive") }
func (s *Store) JournalPath() string       { return s.path("journal.jsonl") }

// PasswordsPath is where the PDF passwords live by default. In the data
// directory rather than the home directory because they belong to the documents
// they open: whoever backs up the archive backs these up with it, and an archive
// restored onto another machine can still read its own encrypted documents. The
// -pdf-passwords flag still overrides it for anyone who keeps them elsewhere.
func (s *Store) PasswordsPath() string { return s.path("passwords") }
func (s *Store) OriginalPath(id int, ext string) string {
	return s.path("originals", strconv.Itoa(id)+ext)
}

// OriginalGlob finds the original regardless of extension, for the paths that
// know an id but not the file type.
func (s *Store) OriginalGlob(id int) (string, error) {
	matches, err := filepath.Glob(s.path("originals", strconv.Itoa(id)+".*"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no original for doc %d", id)
	}
	return matches[0], nil
}

// initNextID derives the counter from the sidecars on disk. There is no
// counter file: deletion leaves a tombstone sidecar behind, so the highest id
// present is a complete record of what has ever been issued.
func (s *Store) initNextID() error {
	ids, err := s.SidecarIDs()
	if err != nil {
		return err
	}
	s.nextID = 1
	for _, id := range ids {
		if id >= s.nextID {
			s.nextID = id + 1
		}
	}
	return nil
}

// AllocID hands out the next id from memory.
func (s *Store) AllocID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	return id
}

// ClaimHash reserves a content hash for ingestion. It returns false when
// another worker is already ingesting identical bytes, which closes the race
// that a Typesense-only dedup check would leave open.
//
// The release is returned rather than exposed as a second method, so it cannot
// be forgotten or deferred in the wrong scope — a claim that is never released
// wedges those bytes for the life of the process.
func (s *Store) ClaimHash(sha string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[sha] {
		return nil, false
	}
	s.inflight[sha] = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.inflight, sha)
	}, true
}

func (s *Store) Save(d *Doc) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.DocPath(d.ID), append(b, '\n'))
}

func (s *Store) Load(id int) (*Doc, error) {
	b, err := os.ReadFile(s.DocPath(id))
	if err != nil {
		return nil, err
	}
	var d Doc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("sidecar %d: %w", id, err)
	}
	return &d, nil
}

// LoadMeta reads only what is needed to name and label a document. The
// extracted text is the bulk of a sidecar, and json.Unmarshal skips keys the
// target struct does not have rather than allocating them — which is the
// difference between reading two thousand titles and reading two thousand
// full documents when a bulk download builds its filenames.
func (s *Store) LoadMeta(id int) (*Doc, error) {
	b, err := os.ReadFile(s.DocPath(id))
	if err != nil {
		return nil, err
	}
	var m struct {
		ID           int    `json:"id"`
		Title        string `json:"title"`
		OriginalName string `json:"original_name"`
		OriginalExt  string `json:"original_ext"`
		Status       string `json:"status"`
		AddedTS      int64  `json:"added_ts"`
		// The purge deadline rides along because the sweeper checks it against
		// the sidecar before destroying anything, and it would otherwise have to
		// read every candidate's full text to ask a one-field question.
		DeleteAfterTS int64 `json:"delete_after_ts"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("sidecar %d: %w", id, err)
	}
	return &Doc{
		ID: m.ID, Title: m.Title, OriginalName: m.OriginalName,
		OriginalExt: m.OriginalExt, Status: m.Status, AddedTS: m.AddedTS,
		DeleteAfterTS: m.DeleteAfterTS,
	}, nil
}

// Delete removes a document's files but replaces its sidecar with a tombstone
// rather than erasing it. That keeps the id permanently spoken for — ids get
// written on paper — and leaves a record of what used to be there.
func (s *Store) Delete(id int) error {
	prev, err := s.Load(id)
	if err != nil {
		return err
	}

	paths := []string{s.ArchivePath(id), s.ThumbPath(id)}
	if orig, err := s.OriginalGlob(id); err == nil {
		paths = append(paths, orig)
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	// Keep identity and timestamps, drop the bulk.
	tomb := &Doc{
		ID:           prev.ID,
		SHA256:       prev.SHA256,
		OriginalName: prev.OriginalName,
		OriginalExt:  prev.OriginalExt,
		Status:       StatusDeleted,
		AddedTS:      prev.AddedTS,
		DeletedTS:    time.Now().Unix(),
		Title:        prev.Title,
		Tags:         []string{},
	}
	return s.Save(tomb)
}

// ClearDerived removes everything regenerable from the original, so the next
// pass rebuilds it. Stage skipping makes crash recovery cheap but also makes a
// user-initiated reprocess a no-op; this is what gives that button meaning.
func (s *Store) ClearDerived(id int) error {
	for _, p := range []string{s.ArchivePath(id), s.ThumbPath(id)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// SidecarIDs lists document ids present on disk, ascending.
func (s *Store) SidecarIDs() ([]int, error) {
	matches, err := filepath.Glob(s.path("docs", "*.json"))
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(matches))
	for _, m := range matches {
		base := strings.TrimSuffix(filepath.Base(m), ".json")
		if id, err := strconv.Atoi(base); err == nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// Each walks every sidecar in id order. A single unreadable sidecar is
// reported and skipped rather than aborting the whole replay, so one bad file
// can't stop the server from starting.
func (s *Store) Each(fn func(*Doc) error) error {
	ids, err := s.SidecarIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		d, err := s.Load(id)
		if err != nil {
			logf("skipping unreadable sidecar %d: %v", id, err)
			continue
		}
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic writes via a temp file in the same directory and renames, so
// readers never observe a partially written file.
func writeFileAtomic(path string, b []byte) error {
	return writeFileAtomicFrom(path, bytes.NewReader(b))
}

// writeFileAtomicFrom is the streaming form, for content that should never be
// held in memory in one piece.
func writeFileAtomicFrom(path string, r io.Reader) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
