FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o clonr-serverd ./cmd/clonr-serverd

FROM alpine:3.19
# ca-certificates: TLS; rsync/parted/sgdisk/e2fsprogs/xfsprogs/dosfstools: disk imaging utilities
RUN apk add --no-cache ca-certificates rsync parted sgdisk e2fsprogs xfsprogs dosfstools
COPY --from=builder /build/clonr-serverd /usr/local/bin/clonr-serverd
EXPOSE 8080 67/udp 69/udp
VOLUME ["/var/lib/clonr"]
ENTRYPOINT ["clonr-serverd"]
