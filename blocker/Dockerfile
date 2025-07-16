FROM golang:1.24.4-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o blocker-worker .

FROM alpine:3.22

RUN apk --no-cache add nftables

WORKDIR /app

COPY --from=builder /app/blocker-worker .

CMD ["/app/blocker-worker"]