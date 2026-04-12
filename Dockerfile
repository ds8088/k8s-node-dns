FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /opt/build

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath \
    -ldflags="-s -w" \
    -o k8s-node-dns \
    .

FROM alpine:3.23

RUN addgroup -S -g 10001 app && adduser -S -G app -u 10001 app

COPY --from=builder /opt/build/k8s-node-dns /opt/k8s-node-dns

USER 10001

EXPOSE 53/udp 53/tcp

ENTRYPOINT ["/opt/k8s-node-dns"]
