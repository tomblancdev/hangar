# Le Hangar — one static binary, nothing else in the image. Its built-in
# plugins are the same binary, started by it as separate processes.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/hangar ./cmd/hangar

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/tomblancdev/hangar" \
      org.opencontainers.image.description="Le Hangar — a small cloud's control plane: ask for a machine, get one within your limits" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/hangar /hangar
VOLUME ["/data"]
USER 65532:65532
EXPOSE 8080
ENV HANGAR_CONFIG=/etc/hangar/hangar.yaml HANGAR_DATA_DIR=/data
ENTRYPOINT ["/hangar"]
CMD ["serve"]
