# ---------- Builder (vulnerabilities di sini tidak ikut ke final) ----------
FROM golang:1.22-bookworm AS builder

WORKDIR /src
# Cache deps lebih efektif
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy seluruh kode
COPY . .

# Build static binary (CGO off) agar cocok untuk distroless:static
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
# Tambah -trimpath & strip untuk lebih kecil
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/app ./...

# ---------- Final ----------
# Distroless static dengan CA certs + user nonroot
FROM gcr.io/distroless/static-debian12:nonroot

# Direktori data untuk WA sqlite store (session.db)
WORKDIR /data
# Buat file dummy agar owner = nonroot
COPY --chown=nonroot:nonroot --from=builder /dev/null /data/.keep

# Binary ke /bin/app
COPY --from=builder /out/app /bin/app

# Konfigurasi default (bisa override saat run)
ENV PORT=7860 \
    SESSION_PATH=/data/session.db \
    MODE=MANUAL \
    TRIGGER=elaina

EXPOSE 7860
USER nonroot:nonroot
ENTRYPOINT ["/bin/app"]
