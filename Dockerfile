# Build the operator manager, restart-relay sidecar, restricted
# credential-runner, and Desktop Gateway binaries as static CGO-free
# executables, then ship them in a distroless nonroot image.
#
# The Gateway ships in this same image rather than its own so that a sidecar,
# a relay, and a Gateway can never disagree about the contract version they
# implement: one digest carries the whole operator-owned data plane.

# noVNC is the browser-side VNC client the Desktop Console loads. It is baked
# in at a pinned, checksum-verified release instead of being fetched at runtime
# so the console has no network dependency and no drift between deployments.
FROM alpine:3.23 AS novnc

ARG NOVNC_VERSION=1.7.0
ARG NOVNC_SHA256=b1003a11b6e6e8d8f7f5e5586daae7f8ca651d8aee0aa155ff9ac841c48f52c6
RUN apk add --no-cache ca-certificates curl tar \
 && curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/novnc/noVNC/archive/refs/tags/v${NOVNC_VERSION}.tar.gz" \
      --output /tmp/novnc.tar.gz \
 && echo "${NOVNC_SHA256}  /tmp/novnc.tar.gz" | sha256sum -c - \
 && mkdir /opt/novnc \
 && tar --extract --gzip --file /tmp/novnc.tar.gz --strip-components=1 --directory /opt/novnc \
 && test -f /opt/novnc/core/rfb.js

FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# The guest and extension assets are compiled into the binaries through
# //go:embed, so they must be present in the build context.
COPY api api
COPY cmd cmd
COPY internal internal
COPY guest guest
COPY extensions extensions
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/manager ./cmd/manager \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/relay ./cmd/relay \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/credential-runner ./cmd/credential-runner \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/desktop-gateway ./cmd/desktop-gateway

FROM gcr.io/distroless/static-debian12:nonroot AS final

COPY --from=novnc /opt/novnc /opt/novnc
COPY --from=build /out/credential-runner /credential-runner
COPY --from=build /out/manager /manager
COPY --from=build /out/relay /relay
COPY --from=build /out/desktop-gateway /desktop-gateway

USER 65532:65532

ENTRYPOINT ["/manager"]
