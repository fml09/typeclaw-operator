# Build both the operator manager and the restart-relay sidecar binary as
# static CGO-free executables, then ship them in a distroless nonroot image.
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY api api
COPY cmd cmd
COPY internal internal

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/manager ./cmd/manager \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/relay ./cmd/relay

FROM gcr.io/distroless/static-debian12:nonroot AS final

COPY --from=build /out/manager /manager
COPY --from=build /out/relay /relay

USER 65532:65532

ENTRYPOINT ["/manager"]
