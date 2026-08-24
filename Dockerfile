FROM golang:1.23 AS build

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/plex-strm-proxy ./cmd/plex-strm-proxy

FROM alpine:3.21

RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=build /out/plex-strm-proxy /usr/local/bin/plex-strm-proxy

EXPOSE 3001
ENTRYPOINT ["/usr/local/bin/plex-strm-proxy"]
