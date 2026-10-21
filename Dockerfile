FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tgbridge ./cmd/tgbridge && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tgbridge /tgbridge
COPY --from=build --chown=65532:65532 /out/data /data
ENV BRIDGE_DATA_DIR=/data
VOLUME ["/data"]
USER 65532:65532
ENTRYPOINT ["/tgbridge"]
