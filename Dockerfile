FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o runscaler ./cmd/runscaler

FROM alpine:3
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/runscaler /usr/local/bin/
ENTRYPOINT ["runscaler"]
