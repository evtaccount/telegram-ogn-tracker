FROM golang:1.24.2-alpine AS builder

WORKDIR /app

COPY . .
RUN go mod download

# Кросс-компиляция под Linux x86_64
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -o tracker

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app
COPY --from=builder /app/tracker .

RUN mkdir -p /root/logs

CMD ["./tracker"]
