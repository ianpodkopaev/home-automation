# Этап сборки
FROM golang:1.24 AS builder

WORKDIR /app
COPY . .

RUN go mod tidy
RUN go build -o goskript .

FROM debian:bookworm-slim

WORKDIR /app
COPY --from=builder /app/goskript .

ENTRYPOINT ["./goskript"]
