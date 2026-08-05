# Docovia - The Land For Docs

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
  -v ~/.config/docovia/config:/etc/docovia/config:ro \
  docovia
```

Then http://localhost:8080, and drop files into `~/docovia-data/ingest/`.

Four details in that command are load-bearing:

**`-p 127.0.0.1:8080`** — without `oidc_issuer` in the config, docovia has no
authentication: anything that can reach the port can read, download, edit and
delete every document, so publish it to loopback and put an authenticating
proxy in front. It logs this warning at every start, on purpose. With OIDC
configured (below), every request requires login and publishing wide is the
point.

**`--init`** — the pipeline spawns ocrmypdf and Ghostscript. If docovia dies
mid-OCR, those need a real PID 1 to reap them.

**`-v ~/docovia-data:/data`** — the whole archive: your originals, the derived
PDFs, thumbnails, the JSON sidecars, the journal and the PDF password file. Back
up this one directory and you have backed up everything.

**`-v ~/.config/docovia/config:...:ro`** — the config file, which is the whole
configuration: the OpenAI key, the OIDC settings, everything. Read-only, and a
mount rather than `-e` variables, because a variable set at `docker run` is
written into the container's config on disk and stays visible to
`docker inspect` for as long as the container exists. Without a config
everything still ingests and is searchable; documents just keep
filename-derived titles and no tags.

You do **not** need to pass `TYPESENSE_API_KEY`. The entrypoint generates one
from `/dev/urandom` at start and hands it to both processes through the
environment — never on the command line, because a container has no process
table of its own and `ps` on the host would print it in full.

### The config file

Flat `key = value`, `#` comments. Looked for at `~/.config/docovia/config`,
then `/etc/docovia/config` (which is where the mount above lands), or wherever
`-config` points. Every flag is also a config key — dashes become underscores —
and a flag given on the command line beats the config. A key the program does
not know refuses to start, naming the line, so a typo cannot silently mean
nothing.

```
# ~/.config/docovia/config          chmod 600 — it holds keys
openai_key = sk-...

oidc_issuer = https://id.example.com
oidc_client_id = docovia
# oidc_client_secret = ...          only for a confidential registration
```

`openai_key` and `oidc_client_secret` are also flags for uniformity's sake,
but put them here: argv is public — `ps` prints it to every user on the
machine, and `docker inspect` keeps it for the life of the container.

### Login, via OIDC

Set `oidc_issuer` and `oidc_client_id` and every request requires login; set
neither and behavior is exactly the pre-OIDC arrangement, loopback warning
included. With a provider like [Pocket ID](https://pocket-id.org):

1. Create an OIDC client there. **Public client** with PKCE — no secret needed.
2. Register the callback URL: `http://your-host:8080/auth/callback` (one per
   origin you browse from; the scheme and host are taken from the request, so
   it works on a LAN address and behind a TLS proxy alike).
3. Put the issuer URL and client id in the config.

Who may log in is the issuer's decision — Pocket ID's allowed-groups per app —
not docovia's; there is no user table here. Sessions are HMAC-signed cookies,
30 days, renewed by use; signing out clears only docovia's session, so with
the issuer's own session still alive, signing back in is one passkey tap.
Deleting `<data>/session.key` signs everyone out at once.

### Checking it came up

```sh
docker logs docovia | head -20
```

```
config loaded from /etc/docovia/config
5 PDF password(s) loaded from /data/passwords
metadata enrichment on, model gpt-5.6-luna
every request requires login via https://id.example.com (public client with PKCE)
indexed 1063 documents from /data/docs
listening on http://0.0.0.0:8080
```

If a line reads `no openai_key in the config`, the config mount did not land.
If `indexed 0` is wrong for your archive, neither did the data mount.

### Settings worth knowing

Each is a flag and a config key; the flag spelling is shown. Everything after
the image name is passed to docovia, replacing the default
`-data /data -listen 0.0.0.0:8080`.

| flag | default | |
|---|---|---|
| `-config` | `~/.config/docovia/config`, then `/etc/docovia/config` | Flag only, for obvious reasons. |
| `-workers` | half the cores, min 8 | Ingest workers. Each spawns ocrmypdf with `--jobs 2`, so the real thread count is about twice this. |
| `-enrich-workers` | half the cores, min 8 | Concurrent model calls. The same figure as `-workers` for an unrelated reason: OCR is CPU-bound, a model call is latency held down by the API's request allowance. |
| `-llm` | `true` | `-llm=false` ingests without calling the model at all. |
| `-llm-model` | `gpt-5.6-luna` | |
| `-oidc-issuer`, `-oidc-client-id` | — | Both or neither; see Login above. |
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
  session.key   0600, signs login cookies; delete it to sign everyone out
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

Go changes need the rebuild; HTML, CSS and JS do not. The defaults already
point at that Typesense and at `~/docovia-data`, and the same
`~/.config/docovia/config` the container mounts is read natively — so one
config serves both, with dev flags beating it where they differ.

Native development is only as faithful as the host's tools, and the image pins
them for a reason — a spreadsheet will fail to convert on a machine that has
`libreoffice-writer` and not `libreoffice-calc`, and Tesseract versions differ by
a few characters on the same scan. When something behaves oddly, the container is
the reference.

```sh
go test ./...
go test -race ./...
```
