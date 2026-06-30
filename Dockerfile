# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shelly-fritz-proxy ./cmd/shelly-fritz-proxy

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/shelly-fritz-proxy /usr/local/bin/shelly-fritz-proxy
EXPOSE 80/tcp
EXPOSE 5353/udp
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/shelly-fritz-proxy"]
