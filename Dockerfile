FROM golang:1.26-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
    -o runner ./cmd/runner

FROM alpine:3
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/runner /usr/local/bin/
ENTRYPOINT ["runner", "run"]
