FROM golang:1.24.4-alpine AS builder

WORKDIR /app

# Копируем файлы модулей для кэширования зависимостей
COPY go.mod go.sum ./
# Загружаем зависимости
RUN go mod download

# Копируем весь исходный код
COPY . .

# Компилируем статически скомпонованный бинарник без отладочной информации для уменьшения размера
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o observer_service .

# Используем Alpine, так как он очень маленький и содержит нужные пакеты
FROM alpine:3.22

WORKDIR /app

# Копируем только скомпилированный бинарник из сборщика
COPY --from=builder /app/observer_service .

# Устанавливаем команду по умолчанию
CMD ["/app/observer_service"]