FROM alpine:3.23 AS novnc

ARG NOVNC_VERSION=1.7.0
RUN apk add --no-cache ca-certificates curl tar \
    && curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/novnc/noVNC/archive/refs/tags/v${NOVNC_VERSION}.tar.gz" \
      --output /tmp/novnc.tar.gz \
    && mkdir /opt/novnc \
    && tar --extract --gzip --file /tmp/novnc.tar.gz --strip-components=1 --directory /opt/novnc \
    && test -f /opt/novnc/core/rfb.js

FROM golang:1.24.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/personal-desktop-gateway .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=novnc /opt/novnc /opt/novnc
COPY --from=build /out/personal-desktop-gateway /personal-desktop-gateway
ENV NOVNC_DIR=/opt/novnc
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/personal-desktop-gateway"]
