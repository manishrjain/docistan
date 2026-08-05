# docovia

The Holy Land for Docs.

A self-hosted document archive: drop files in, get them OCR'd, titled, tagged,
dated and searchable. Go and Typesense, no database, and the JSON sidecars next
to your documents are the source of truth.

## Running it

The image carries everything the pipeline shells out to — ocrmypdf, Tesseract,
Ghostscript, poppler, qpdf, ImageMagick, LibreOffice — and runs Typesense
alongside docovia inside the same container. That is deliberate: Typesense keeps
no state here, because the index is rebuilt from the sidecars on every boot, so
there is nothing to migrate and bumping it is a rebuild.

```sh
docker build -t docovia .

docker run -d --init --name docovia --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v ~/docovia-data:/data \
  -v ~/.openai.secret:/etc/docovia/openai.secret:ro \
  docovia
```

Then http://localhost:8080, and drop files into `~/docovia-data/ingest/`.

Four details in that command are load-bearing:

**`-p 127.0.0.1:8080`** — docovia has no authentication of its own. Anything
that can reach the port can read, download, edit and delete every document, so
publish it to loopback and put an authenticating proxy in front. It logs this
warning at every start, on purpose.

**`--init`** — the pipeline spawns ocrmypdf and Ghostscript. If docovia dies
mid-OCR, those need a real PID 1 to reap them.

**`-v ~/docovia-data:/data`** — the whole archive: your originals, the derived
PDFs, thumbnails, the JSON sidecars, the journal and the PDF password file. Back
up this one directory and you have backed up everything.

**`-v ~/.openai.secret:...:ro`** — read-only, and a mount rather than
`-e OPENAI_API_KEY=`, because a variable set at `docker run` is written into the
container's config on disk and stays visible to `docker inspect` for as long as
the container exists. Without it everything still ingests and is searchable;
documents just keep filename-derived titles and no tags.

You do **not** need to pass `TYPESENSE_API_KEY`. The entrypoint generates one
from `/dev/urandom` at start and hands it to both processes through the
environment — never on the command line, because a container has no process
table of its own and `ps` on the host would print it in full.

### Checking it came up

```sh
docker logs docovia | head -20
```

```
5 PDF password(s) loaded from /data/passwords
metadata enrichment on, model gpt-5.6-luna (key from /etc/docovia/openai.secret)
indexed 1063 documents from /data/docs
listening on http://0.0.0.0:8080
```

If the middle line reads `no OpenAI key in …`, the key mount did not land. If
`indexed 0` is wrong for your archive, neither did the data mount.

### Flags worth knowing

Everything after the image name is passed to docovia, replacing the default
`-data /data -listen 0.0.0.0:8080`.

| flag | default | |
|---|---|---|
| `-workers` | half the cores, min 8 | Ingest workers. Each spawns ocrmypdf with `--jobs 2`, so the real thread count is about twice this. |
| `-enrich-workers` | 4 | Concurrent model calls. Unrelated to `-workers` on purpose: OCR is CPU-bound, a model call is latency. |
| `-llm` | `true` | `-llm=false` ingests without calling the model at all. |
| `-llm-model` | `gpt-5.6-luna` | |
| `-public-origin` | — | e.g. `https://docs.example.com`, for the cross-site check when behind a proxy. |
| `-pdf-passwords` | `<data>/passwords` | Candidates tried against encrypted PDFs, one per line. |

### The archive on disk

```
~/docovia-data/
  ingest/       drop files here; the watcher waits for the size to settle
  originals/    exactly what arrived — the preservation copy
  archive/      derived PDF/A with a text layer — reproducible by re-running OCR
  thumbs/       first-page JPEGs
  docs/         one JSON sidecar per document; the source of truth
  journal.jsonl what happened to what, and what it cost
  passwords     0600, one password per line
```

Back up `originals/` and `docs/`; `archive/` and `thumbs/` are derived and cost
only a re-OCR to rebuild. Restoring is `restic restore` and a start — the boot
replay rebuilds the index by itself.

## Developing

Typesense from compose, docovia natively, templates and static files read from
disk so an edit is a browser reload rather than a rebuild:

```sh
docker compose up -d           # Typesense on 127.0.0.1:8108
go build -o docovia . && ./docovia -dev
```

Go changes need the rebuild; HTML, CSS and JS do not. The defaults already point
at that Typesense, at `~/docovia-data`, and at `~/.openai.secret`.

Native development is only as faithful as the host's tools, and the image pins
them for a reason — a spreadsheet will fail to convert on a machine that has
`libreoffice-writer` and not `libreoffice-calc`, and Tesseract versions differ by
a few characters on the same scan. When something behaves oddly, the container is
the reference.

```sh
go test ./...
go test -race ./...
```
