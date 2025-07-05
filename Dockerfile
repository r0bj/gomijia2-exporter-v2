FROM golang:1.24.4 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY *.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o gomijia2-exporter-v2 .


FROM scratch

COPY --from=builder /workspace/gomijia2-exporter-v2 /

ENTRYPOINT ["/gomijia2-exporter-v2"]
