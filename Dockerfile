# build container
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.su* ./
RUN if [ -f go.sum ]; then go mod download; fi
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /video-optimizer-daemon .

# run container
FROM debian:bookworm-slim

# install multimedia tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    mediainfo \
    handbrake-cli \
    mkvtoolnix \
    && rm -rf /var/lib/apt/lists/*
ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8

# setup permissions
RUN groupadd -g 1000 mygroup && \
    useradd -u 1000 -g mygroup -m myuser

WORKDIR /app
RUN mkdir storage && chown myuser:mygroup storage

USER myuser
COPY --from=builder --chown=myuser:mygroup /video-optimizer-daemon .

CMD ["./video-optimizer-daemon"]
