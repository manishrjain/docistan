# docovia, with everything it shells out to.
#
# This image exists because the pipeline's *output* depends on the versions of
# seven external tools. ocrmypdf decides whether --redo-ocr keeps a text layer,
# Ghostscript decides whether a JPEG survives re-encoding, ImageMagick decides
# whether an image becomes a PDF at all. Unpinned, restoring a backup onto
# another machine and re-running OCR rebuilds a different archive — which is
# exactly what the disaster-recovery story assumes it does not.
#
# Typesense is in here too. It keeps nothing: the index is rebuilt from the JSON
# sidecars on every boot, so it has no volume, no migrations and no state to
# outlive the process. Bumping it is the ARG below and a rebuild.

ARG TYPESENSE_VERSION=30.2

# --- typesense ---------------------------------------------------------------
# Named only so the binary can be copied out of it. It is a single executable
# against glibc and nothing else, so this is a copy rather than an install.
FROM typesense/typesense:${TYPESENSE_VERSION} AS typesense

# --- build -------------------------------------------------------------------
FROM golang:1.25 AS build
WORKDIR /src

# Cached separately from the source: the dependency tree changes far less often
# than the code, and a one-line edit should not re-download it.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static, so the runtime stage needs no Go toolchain and no matching libc. This
# program has no cgo in it, which is what makes that free.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/docovia .

# --- runtime -----------------------------------------------------------------
# Trixie rather than bookworm, and not for freshness. Bookworm ships ImageMagick
# 6, where the `magick` command does not exist — every image would fail at
# normalize. It also carries ocrmypdf 14 against trixie's 16, and --redo-ocr is
# the whole of how a scan with a page-number text layer gets read.
FROM debian:trixie-slim

# tesseract-ocr-eng is separate from tesseract-ocr: the engine without a
# language pack silently reads nothing.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ocrmypdf \
      tesseract-ocr \
      tesseract-ocr-eng \
      ghostscript \
      poppler-utils \
      qpdf \
      imagemagick \
      libreoffice-writer \
      libreoffice-calc \
      libreoffice-impress \
      fonts-liberation \
      fonts-crosextra-carlito \
      fonts-crosextra-caladea \
      ca-certificates \
      tzdata \
 && rm -rf /var/lib/apt/lists/*

# Debian has shipped an ImageMagick policy that refuses the PDF coder outright,
# to contain old Ghostscript vulnerabilities. Trixie does not, today. If that
# ever comes back this image would build cleanly and then fail on the first
# JPEG someone uploaded, months later and far from here — so the conversion the
# pipeline actually performs is run at build time and the build fails instead.
RUN magick -size 200x80 canvas:white /tmp/probe.jpg \
 && magick /tmp/probe.jpg -auto-orient -strip -quality 92 /tmp/probe.pdf \
 && test -s /tmp/probe.pdf \
 && rm -f /tmp/probe.jpg /tmp/probe.pdf \
 || (echo "ImageMagick refuses to write PDFs in this base image — check /etc/ImageMagick-*/policy.xml" >&2; exit 1)

# The same argument for LibreOffice, which has more ways to be installed and
# still not work: a missing import filter, a profile it cannot write, a Java
# dependency it wanted after all. RTF and SYLK are the fixtures because they are
# the two office formats that can be written here as plain text, with no archive
# to build and no binary blob to keep in the repository.
#
# Both, because the modules install separately and a document only ever reaches
# the one that reads its format. An image with Writer and no Calc passes a
# Writer-only probe and then fails on the first spreadsheet somebody uploads —
# which is not hypothetical: it is exactly what a .xlsx did on a development
# machine that had libreoffice-writer and nothing else, months after the image
# was built and far from here.
RUN printf '%s' '{\rtf1\ansi Probe of the office conversion path.\par}' > /tmp/probe.rtf \
 && printf 'ID;PSCALC3\nC;X1;Y1;K"Probe of the spreadsheet path"\nE\n' > /tmp/calcprobe.slk \
 && soffice -env:UserInstallation=file:///tmp/lo-probe --headless --norestore \
      --convert-to pdf --outdir /tmp /tmp/probe.rtf >/dev/null 2>&1 \
 && soffice -env:UserInstallation=file:///tmp/lo-calc --headless --norestore \
      --convert-to pdf --outdir /tmp /tmp/calcprobe.slk >/dev/null 2>&1 \
 && test -s /tmp/probe.pdf && test -s /tmp/calcprobe.pdf \
 && pdftotext /tmp/probe.pdf - | grep -q "office conversion path" \
 && pdftotext /tmp/calcprobe.pdf - | grep -q "spreadsheet path" \
 && rm -rf /tmp/probe.rtf /tmp/probe.pdf /tmp/calcprobe.slk /tmp/calcprobe.pdf /tmp/lo-probe /tmp/lo-calc \
 || (echo "LibreOffice cannot convert office documents to PDF in this image — check that libreoffice-writer and libreoffice-calc are both installed" >&2; exit 1)

# The metric-compatible substitutes are load-bearing rather than cosmetic. A
# .doc naming Calibri or Cambria on a machine without them gets a fallback of a
# different width, so lines re-wrap and the page count changes — and the derived
# PDF stops being a faithful copy of what arrived, which is the only reason it
# is kept. Carlito and Caladea match those two by metrics; Liberation matches
# Arial, Times New Roman and Courier New.
RUN fc-list | grep -qi carlito && fc-list | grep -qi caladea \
 || (echo "metric-compatible substitute fonts are missing" >&2; exit 1)

COPY --from=typesense /opt/typesense-server /usr/local/bin/typesense-server
COPY --from=build /out/docovia /usr/local/bin/docovia
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint

# Matching the host user that owns the archive, because the data directory is a
# bind mount and the passwords file inside it is 0600. A mismatch here does not
# fail loudly — it fails as an encrypted document that will not open.
ARG UID=1000
ARG GID=1000
RUN groupadd -g "${GID}" docovia \
 && useradd -u "${UID}" -g "${GID}" -m -s /usr/sbin/nologin docovia \
 && mkdir -p /data /var/lib/typesense /etc/docovia \
 && chown -R "${UID}:${GID}" /data /var/lib/typesense \
 && chmod +x /usr/local/bin/docker-entrypoint

USER docovia
WORKDIR /data

# The archive. Everything durable is in here and nothing durable is anywhere
# else, which is what makes the backup one directory.
VOLUME ["/data"]

# The OpenAI key is looked for here when OPENAI_API_KEY is unset, so mounting it
# is the whole configuration:
#
#   -v ~/.openai.secret:/etc/docovia/openai.secret:ro
#
# A read-only mount rather than an environment variable, because a variable set
# at `docker run` is written into the container's config on disk and stays
# visible to `docker inspect` for as long as the container exists.

# 0.0.0.0 because a container's boundary is its published port, not its listen
# address — publish it to 127.0.0.1 on the host and authenticate in front.
EXPOSE 8080

# Typesense answers only on loopback inside the container, so the only way in is
# through docovia.
ENV TYPESENSE_DATA=/var/lib/typesense

# Over bash's /dev/tcp, the same trick the compose file uses on the Typesense
# image and for the same reason: a slim base ships no curl, no wget and no nc,
# and installing one to ask a single question is a package to keep patched
# forever. /healthz reports Typesense as well, so this covers both processes.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/8080 && printf 'GET /healthz HTTP/1.0\\r\\n\\r\\n' >&3 && head -1 <&3 | grep -q ' 200 '"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint"]
CMD ["-data", "/data", "-listen", "0.0.0.0:8080"]
