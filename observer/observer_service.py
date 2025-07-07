import os
import logging
from typing import List

import httpx
import redis.asyncio as redis
from fastapi import FastAPI, Request, HTTPException
from pydantic import BaseModel, Field

# --- Настройка логирования ---
logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')
logger = logging.getLogger(__name__)

# --- Конфигурация из переменных окружения ---
# Используем os.getenv для гибкости при запуске в Docker
REDIS_URL = os.getenv("REDIS_URL", "redis://localhost:6379/0")
MAX_IPS_PER_USER = int(os.getenv("MAX_IPS_PER_USER", 3))
ALERT_WEBHOOK_URL = os.getenv("ALERT_WEBHOOK_URL") # URL вашего ТГ-бота для алертов
# Время жизни записи об IP-адресах пользователя в секундах (24 часа)
USER_IP_TTL_SECONDS = int(os.getenv("USER_IP_TTL_SECONDS", 24 * 60 * 60))
# Время "остывания" для алертов, чтобы не спамить (1 час)
ALERT_COOLDOWN_SECONDS = int(os.getenv("ALERT_COOLDOWN_SECONDS", 60 * 60))

# --- Проверка конфигурации при старте ---
if not ALERT_WEBHOOK_URL:
    logger.warning("ALERT_WEBHOOK_URL не задан. Алерты не будут отправляться.")

# --- Модели данных (Pydantic) для валидации ---
class LogEntry(BaseModel):
    """Модель для входящей записи лога от Vector."""
    user_email: str = Field(..., alias="user_email")
    source_ip: str = Field(..., alias="source_ip")

class AlertPayload(BaseModel):
    """Модель для исходящего алерта в ТГ-бот."""
    user_identifier: str
    detected_ips_count: int
    limit: int
    violation_type: str = "ip_limit_exceeded"

# --- Инициализация приложения ---
app = FastAPI(
    title="Observer Service",
    description="Сервис для обнаружения превышения лимитов подключений.",
    version="1.0.0"
)

# Создаем глобальные клиенты для переиспользования соединений
# Это гораздо эффективнее, чем создавать их при каждом запросе
redis_client = redis.from_url(REDIS_URL, decode_responses=True)
http_client = httpx.AsyncClient()

@app.on_event("shutdown")
async def shutdown_event():
    """Корректно закрываем соединения при остановке приложения."""
    await http_client.aclose()
    await redis_client.aclose()
    logger.info("Соединения с HTTP и Redis успешно закрыты.")

# --- Основная логика ---
@app.post("/log-entry")
async def process_log_entries(entries: List[LogEntry]):
    """
    Главный эндпоинт. Принимает пачку логов, обрабатывает и проверяет лимиты.
    """
    for entry in entries:
        try:
            # Ключи в Redis
            user_ips_key = f"user_ips:{entry.user_email}"
            alert_sent_key = f"alert_sent:{entry.user_email}"

            # --- Оптимизация с Redis Pipeline ---
            # Объединяем несколько команд в один сетевой запрос к Redis
            async with redis_client.pipeline() as pipe:
                pipe.sadd(user_ips_key, entry.source_ip)  # 1. Добавить IP в набор
                pipe.expire(user_ips_key, USER_IP_TTL_SECONDS) # 2. Обновить TTL набора
                pipe.scard(user_ips_key) # 3. Получить количество уникальных IP
                pipe.exists(alert_sent_key) # 4. Проверить, был ли уже отправлен алерт
                
                # Выполняем все команды за один раз
                results = await pipe.execute()

            # Извлекаем результаты
            # result[0] - результат SADD (не используем)
            # result[1] - результат EXPIRE (не используем)
            current_ip_count = results[2]
            alert_was_sent = results[3]

            # Проверяем условие для отправки алерта
            if current_ip_count > MAX_IPS_PER_USER and not alert_was_sent:
                logger.warning(
                    f"ПРЕВЫШЕНИЕ ЛИМИТА! Пользователь: {entry.user_email}, "
                    f"IP-адресов: {current_ip_count}/{MAX_IPS_PER_USER}."
                )
                
                # Устанавливаем флаг "кулдауна" в Redis, чтобы не спамить
                await redis_client.setex(alert_sent_key, ALERT_COOLDOWN_SECONDS, "1")
                
                # Отправляем алерт, если URL задан
                if ALERT_WEBHOOK_URL:
                    alert_payload = AlertPayload(
                        user_identifier=entry.user_email,
                        detected_ips_count=current_ip_count,
                        limit=MAX_IPS_PER_USER,
                    )
                    try:
                        await http_client.post(ALERT_WEBHOOK_URL, json=alert_payload.dict(), timeout=10.0)
                        logger.info(f"Алерт для пользователя {entry.user_email} успешно отправлен.")
                    except httpx.RequestError as e:
                        logger.error(f"Ошибка отправки алерта для {entry.user_email}: {e}")

        except Exception as e:
            logger.error(f"Критическая ошибка при обработке записи для {entry.user_email}: {e}")
            # В реальном приложении можно добавить отправку ошибки в систему мониторинга (Sentry и т.п.)

    return {"status": "ok", "processed_entries": len(entries)}

@app.get("/health")
async def health_check():
    """Эндпоинт для проверки работоспособности сервиса."""
    try:
        await redis_client.ping()
        return {"status": "ok", "redis_connection": "ok"}
    except Exception as e:
        raise HTTPException(status_code=503, detail=f"Redis connection failed: {e}")

