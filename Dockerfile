FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/nightcache .

FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg python3 python3-pip unzip \
    && rm -rf /var/lib/apt/lists/*
# yt-dlp recommends FFmpeg plus a JS runtime for full modern YouTube support.
RUN python3 -m pip install --break-system-packages --no-cache-dir -U --pre "yt-dlp[default]" \
    && curl -fsSL https://deno.land/install.sh | sh \
    && mv /root/.deno/bin/deno /usr/local/bin/deno \
    && chmod +x /usr/local/bin/deno
WORKDIR /app
COPY --from=build /out/nightcache /app/nightcache
COPY index.html /app/index.html
ENV PORT=10000
EXPOSE 10000
CMD ["/app/nightcache"]
