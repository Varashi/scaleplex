# scaleplex-clientfix — content-aware reverse proxy for PMS's transcode
# decision endpoint. Fixes per-client Plex profile bugs (currently the
# Apple-TV-8.45 mkv-in-HLS re-encode bug, Varashi/scaleplex#122) by
# inspecting PMS's copy-vs-transcode decision and rewriting the container
# only for the broken re-encode case. See cmd/clientfix/main.go.
#
# Deployed as a small Service the gateway routes the matched client's
# decision request to; everything else bypasses it straight to PMS.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY clientfix/go.mod ./
RUN go mod download
COPY clientfix/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-clientfix ./cmd/clientfix

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source=https://github.com/Varashi/scaleplex
LABEL org.opencontainers.image.description="scaleplex client-fix proxy — content-aware PMS decision rewriter for per-client Plex profile bugs"
LABEL org.opencontainers.image.licenses=MIT
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/scaleplex-clientfix"]
COPY --from=build /out/scaleplex-clientfix /scaleplex-clientfix
