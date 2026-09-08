# Multi-stage build: compile with the full Go toolchain, ship a static
# binary in a minimal image. The engine has zero third-party dependencies,
# so the final image needs nothing but libc-free Go binary and CA certs.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 strata
COPY --from=build /out/server /usr/local/bin/server
RUN mkdir -p /data && chown strata:strata /data
USER strata
VOLUME ["/data"]
ENV STRATA_DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
