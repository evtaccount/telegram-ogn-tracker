# Build stage
FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o ogn-tracker main.go

# Final image
FROM alpine:3.18
WORKDIR /app
COPY --from=build /src/ogn-tracker /app/
ENV TELEGRAM_BOT_TOKEN=""
CMD ["/app/ogn-tracker"]
