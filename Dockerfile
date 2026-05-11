FROM golang:1.24.3-alpine3.21 AS builder

LABEL org.opencontainers.image.source=https://github.com/amgno/glance

WORKDIR /app
COPY . /app
RUN CGO_ENABLED=0 go build .

FROM alpine:3.21

WORKDIR /app
COPY --from=builder /app/glance .

EXPOSE 8080/tcp
ENTRYPOINT ["/app/glance", "--config", "/app/config/glance.yml"]
