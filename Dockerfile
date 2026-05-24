# syntax=docker/dockerfile:1.7

FROM golang:1.26.3-trixie AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/ddnsync .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/ddnsync /usr/local/bin/ddnsync
EXPOSE 8245
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ddnsync"]