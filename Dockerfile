FROM alpine:latest

RUN apk add --no-cache ca-certificates ffmpeg tzdata

WORKDIR /app

COPY dist/m3u8-downloader /app/m3u8-downloader

RUN chmod 0755 /app/m3u8-downloader \
    && mkdir -p /app/cache /app/data /app/downloads

ENV ADDR=0.0.0.0:8080
ENV FFMPEG_PATH=/usr/bin/ffmpeg

VOLUME ["/app/cache", "/app/data", "/app/downloads"]

EXPOSE 8080

ENTRYPOINT ["/app/m3u8-downloader"]
