# scaleplex-orchestrator image — phase 3.
# Static Go binary on distroless. No ffmpeg, no NFS, no GPU here — the
# orchestrator just routes task envelopes to workers.

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY orchestrator/go.mod ./
RUN go mod download
COPY orchestrator/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/scaleplex-orchestrator ./...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/scaleplex-orchestrator /usr/local/bin/scaleplex-orchestrator
EXPOSE 3500
USER nonroot
ENTRYPOINT ["/usr/local/bin/scaleplex-orchestrator"]
