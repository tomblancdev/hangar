# Le Hangar — one static binary and the certificate authorities it verifies
# https with, nothing else in the image. Its built-in plugins are the same
# binary, started by it as separate processes. The nodes' hook rides along,
# for the operator to copy out (podman cp <container>:/hangar-hook .): it
# runs on a node, never here.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/hangar ./cmd/hangar && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/hangar-hook ./cmd/hangar-hook

FROM scratch
# the brain verifies its identity provider's certificate (and a wake's, over
# https): a scratch image carries no trust store, and without one every
# certificate is "signed by unknown authority" (tools/image-test.sh)
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
LABEL org.opencontainers.image.source="https://github.com/tomblancdev/hangar" \
      org.opencontainers.image.description="Le Hangar — a small cloud's control plane: ask for a machine, get one within your limits" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/hangar /out/hangar-hook /
VOLUME ["/data"]
USER 65532:65532
EXPOSE 8080
ENV HANGAR_CONFIG=/etc/hangar/hangar.yaml HANGAR_DATA_DIR=/data
ENTRYPOINT ["/hangar"]
CMD ["serve"]
