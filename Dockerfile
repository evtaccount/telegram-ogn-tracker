FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o tracker

FROM alpine:latest
WORKDIR /app
COPY --from=build /src/tracker ./tracker
CMD ["./tracker"]
