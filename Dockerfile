# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# build
# ---------------------------------------------------------------------------
FROM golang:1.27 AS builder

# ca-certificates: the root store copied into the final image (and used to fetch
#   modules here).
# tzdata: the zone files TZ points at. Asking for it explicitly matters: if the
#   builder ever lacks it, the COPY below fails the build loudly instead of shipping
#   an image whose logs silently say UTC. Go falls back to UTC without complaining
#   when a zone name cannot be resolved.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /dyndns

# go.mod before the sources, so `go mod download` stays cached across code-only
# rebuilds; the previous COPY of the whole tree re-downloaded on every edit.
# If the project ever gains dependencies, copy go.sum here too.
COPY ./src/go.mod ./
RUN go mod download

COPY ./src .

# CGO_ENABLED=0  a static binary using Go's own DNS resolver. Measured: no ELF
#                interpreter, and it resolves both /etc/hosts entries and DNS. This is
#                what makes the scratch stage below possible at all.
# -s -w          9.7 MiB -> 6.6 MiB of stripped symbol and DWARF tables.
# -trimpath      no /dyndns paths baked into panics; reproducible build.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /updater .

# ---------------------------------------------------------------------------
# run
# ---------------------------------------------------------------------------
# scratch: no shell, no package manager, nothing to patch and no CVE list to chase.
# The runtime is the binary plus the two data files Go needs, which is everything a
# static HTTPS client requires:
#
#   ca-certificates.crt  verifies api.zone.eu and the IP service (~220 KiB)
#   zoneinfo             what TZ=Europe/Tallinn reads (~1 MiB; drop it and logs are UTC)
#
# There is no /bin/sh, so debugging is `LOG_LEVEL=debug docker compose up -d` followed
# by `docker compose logs -f` - which logs at debug what it is doing, to stdout, with
# the URL and the response status.
FROM scratch

COPY --from=builder /updater /zone-dyndns/updater
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

WORKDIR /zone-dyndns

# Never root, whoever runs the image. docker-compose.yml overrides this with the host
# user, so files written into ./conf belong to whoever edits records.conf by hand; the
# default here is what protects a plain `docker run`. Numeric, because scratch has no
# /etc/passwd to look a name up in.
USER 65534:65534

ENTRYPOINT ["/zone-dyndns/updater"]
