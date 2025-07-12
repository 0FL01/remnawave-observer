FROM golang:1.21-alpine AS builder

WORKDIR /app

# Копируем файлы модулей для кэширования зависимостей
COPY go.mod go.sum ./
# Загружаем зависимости
RUN go mod download

# Копируем весь исходный код
COPY . .

# Компилируем статически скомпонованный бинарник без отладочной информации для уменьшения размера
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o blocker-worker .

# Используем Alpine, так как он очень маленький и содержит нужные пакеты
FROM alpine:3.22

# Устанавливаем только nftables и больше ничего лишнего
RUN apk --no-cache add nftables

WORKDIR /app

# Копируем только скомпилированный бинарник из сборщика
COPY --from=builder /app/blocker-worker .

# Устанавливаем команду по умолчанию
CMD ["/app/blocker-worker"]