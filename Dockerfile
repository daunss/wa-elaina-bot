# ---------- Stage build ----------
FROM golang:1.22 AS builder
WORKDIR /app
COPY . .
# Biarkan go mod di-generate otomatis saat build; akan fetch deps dari imports
RUN CGO_ENABLED=0 go build -o app .

# ---------- Stage runtime ----------
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
ENV GEMINI_API_KEY=
ENV BOT_NAME=Elaina
# Untuk Spaces free: gunakan session lokal; untuk Spaces dengan storage upgrade: /data/session.db
ENV SESSION_PATH=session.db
COPY --from=builder /app/app /app/app
EXPOSE 7860
USER nobody
ENTRYPOINT ["/app/app"]
