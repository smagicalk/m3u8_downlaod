FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install --no-install-recommends -y ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY dist/m3u8-downloader /app/m3u8-downloader

RUN chmod 0755 /app/m3u8-downloader \
    && mkdir -p /app/cache /app/data /app/downloads

ENV ADDR=0.0.0.0:8080
ENV FFMPEG_PATH=/usr/bin/ffmpeg

VOLUME ["/app/cache", "/app/data", "/app/downloads"]

EXPOSE 8080

ENTRYPOINT ["/app/m3u8-downloader"]
