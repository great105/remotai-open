FROM golang:1.25-alpine AS build
WORKDIR /src
COPY tgcontrol-relay/go.mod tgcontrol-relay/go.sum ./
RUN go mod download
COPY tgcontrol-relay/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X tgcontrol-relay/internal/server.Version=self-hosted' -o /out/relay ./cmd/relay

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S relay && adduser -S -G relay relay && \
    mkdir -p /data && chown relay:relay /data
COPY --from=build /out/relay /usr/local/bin/relay
USER relay
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/relay"]
