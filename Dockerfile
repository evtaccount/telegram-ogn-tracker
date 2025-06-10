# Builder stage
# Use a Go version matching go.mod requirement
FROM golang:1.23.10-alpine AS builder

WORKDIR /app

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .

# Cross-compile for Linux amd64
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -o bot ./cmd/bot

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/
COPY --from=builder /app/bot .

RUN mkdir -p /root/logs

CMD ["./bot"]
