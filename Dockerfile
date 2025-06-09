# Builder stage
FROM golang:1.24.2-alpine AS builder

WORKDIR /app

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .

# Cross-compile for Linux amd64
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -o bot main.go

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/
COPY --from=builder /app/bot .

RUN mkdir -p /root/logs

CMD ["./bot"]
