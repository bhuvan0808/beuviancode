# Beuvian backend container.
#
# Build from the REPOSITORY ROOT, not from this directory, because the build needs
# both backend/ and shared/:
#
#   docker build -f docker/backend.Dockerfile -t beuvian-backend:dev .
#
# PROJECT.md specifies Docker for the backend only. The Desktop Agent is a native
# binary on a user's machine — containerising it would defeat its purpose, since it
# must see the host's PATH, the user's repositories, and the host power APIs.

# ---------- Stage 1: build ----------
# Pinned to a specific minor version. A floating `golang:alpine` tag would change
# the compiler under us between builds, which is the opposite of reproducible.
FROM golang:1.26-alpine AS builder

# git is needed for VCS stamping in the binary; ca-certificates for module fetches.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Copy manifests first so `go mod download` is cached independently of source
# changes. Without this split, editing one .go file re-downloads every dependency.
COPY shared/go.mod ./shared/
COPY backend/go.mod backend/go.sum ./backend/

# No go.work is copied on purpose. The workspace is a developer convenience; the
# container relies on the `replace` directive in backend/go.mod to resolve the
# shared module from ../shared. Copying go.work would make the build fail on the
# absent agent/ module. See docs/adr/0002-go-workspace-multi-module.md.
WORKDIR /src/backend
RUN go mod download

# Now the sources.
WORKDIR /src
COPY shared/ ./shared/
COPY backend/ ./backend/

# Build metadata, supplied by CI. Defaults keep a plain `docker build` working.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

WORKDIR /src/backend
# CGO_ENABLED=0 produces a static binary, which is what lets the final stage be
# distroless/static with no libc at all.
# -trimpath strips local filesystem paths, so the binary does not leak build-host
# directory names and is byte-identical across machines.
# -w -s drop DWARF and the symbol table: a smaller image and less to disclose.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-w -s \
      -X github.com/bhuvan0808/beuviancode/shared/version.Version=${VERSION} \
      -X github.com/bhuvan0808/beuviancode/shared/version.Commit=${COMMIT} \
      -X github.com/bhuvan0808/beuviancode/shared/version.Date=${BUILD_DATE}" \
    -o /out/beuvian-backend ./cmd/server

# Fail the build if the binary cannot even report its own version. Catching a
# broken build here is far cheaper than discovering it in a crash loop on Railway.
RUN /out/beuvian-backend -version

# ---------- Stage 2: runtime ----------
# distroless/static rather than alpine: no shell, no package manager, no busybox.
# That removes essentially the entire post-exploitation toolkit from the image, and
# the attack surface of a WebSocket gateway exposed to the internet is worth
# minimising. The tradeoff is that `docker exec` debugging is impossible — accepted
# deliberately, since diagnosis is via structured logs, not a shell in production.
FROM gcr.io/distroless/static-debian12:nonroot

# Outbound TLS to Supabase, Upstash, and GitHub needs the trust store.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=builder /out/beuvian-backend /usr/local/bin/beuvian-backend

# The nonroot user (uid 65532) ships with the base image. Running as root inside a
# container is an unnecessary privilege even with no shell present.
USER nonroot:nonroot

# Documentation only; the actual port comes from configuration. Railway injects
# PORT, which backend/internal/config adopts into the normal precedence chain.
EXPOSE 8080

# No HEALTHCHECK instruction: distroless has no shell or curl to run one, and
# Railway performs its own HTTP health probe. Adding a Go-based health binary
# purely to satisfy Docker's healthcheck would be weight for no benefit.

ENTRYPOINT ["/usr/local/bin/beuvian-backend"]
