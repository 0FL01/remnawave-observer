import os
import json
import logging
from typing import List, Set
import httpx
import aio_pika
import redis.asyncio as redis
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

# Настройка логирования
logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')
logger = logging.getLogger(__name__)

# Загрузка конфигурации из переменных окружения
REDIS_URL = os.getenv("REDIS_URL", "redis://localhost:6379/0")
RABBITMQ_URL = os.getenv("RABBITMQ_URL", "amqp://guest:guest@localhost/")
MAX_IPS_PER_USER = int(os.getenv("MAX_IPS_PER_USER", 3))
ALERT_WEBHOOK_URL = os.getenv("ALERT_WEBHOOK_URL")
USER_IP_TTL_SECONDS = int(os.getenv("USER_IP_TTL_SECONDS", 24 * 60 * 60))
ALERT_COOLDOWN_SECONDS = int(os.getenv("ALERT_COOLDOWN_SECONDS", 60 * 60))
BLOCK_DURATION = os.getenv("BLOCK_DURATION", "5m")
BLOCKING_EXCHANGE_NAME = "blocking_exchange"

# Обработка списка исключений
excluded_users_str = os.getenv("EXCLUDED_USERS", "")
EXCLUDED_USERS: Set[str] = {email.strip() for email in excluded_users_str.split(',') if email.strip()}

if EXCLUDED_USERS:
    logger.info(f"Загружен список исключений: {len(EXCLUDED_USERS)} пользователей.")

# Глобальные переменные для соединений
app = FastAPI(title="Observer Service", version="1.2.1")
redis_client = redis.from_url(REDIS_URL, decode_responses=True)
http_client = httpx.AsyncClient()
rabbitmq_connection = None
blocking_exchange = None

@app.on_event("startup")
async def startup_event():
    """Подключение к RabbitMQ при старте приложения."""
    global rabbitmq_connection, blocking_exchange
    try:
        rabbitmq_connection = await aio_pika.connect_robust(RABBITMQ_URL)
        channel = await rabbitmq_connection.channel()
        blocking_exchange = await channel.declare_exchange(
            BLOCKING_EXCHANGE_NAME, aio_pika.ExchangeType.FANOUT, durable=True
        )
        logger.info("Успешное подключение к RabbitMQ.")
    except Exception as e:
        logger.error(f"Не удалось подключиться к RabbitMQ: {e}")
        rabbitmq_connection = None

@app.on_event("shutdown")
async def shutdown_event():
    """Корректное закрытие соединений."""
    if rabbitmq_connection:
        await rabbitmq_connection.close()
    await http_client.aclose()
    await redis_client.aclose()
    logger.info("Все соединения успешно закрыты.")

# Модели данных
class LogEntry(BaseModel):
    user_email: str = Field(..., alias="user_email")
    source_ip: str = Field(..., alias="source_ip")

class AlertPayload(BaseModel):
    user_identifier: str
    detected_ips_count: int
    limit: int
    violation_type: str = "ip_limit_exceeded"

@app.post("/log-entry")
async def process_log_entries(entries: List[LogEntry]):
    """Основной эндпоинт для обработки логов."""
    for entry in entries:
        try:
            # Проверка, находится ли пользователь в списке исключений
            if entry.user_email in EXCLUDED_USERS:
                continue

            user_ips_key = f"user_ips:{entry.user_email}"
            alert_sent_key = f"alert_sent:{entry.user_email}"

            async with redis_client.pipeline() as pipe:
                pipe.sadd(user_ips_key, entry.source_ip)
                pipe.expire(user_ips_key, USER_IP_TTL_SECONDS)
                pipe.scard(user_ips_key)
                pipe.exists(alert_sent_key)
                results = await pipe.execute()

            current_ip_count = results[2]
            alert_was_sent = bool(results[3])

            # Проверка на превышение лимита и отсутствие кулдауна
            if current_ip_count > MAX_IPS_PER_USER and not alert_was_sent:
                logger.warning(f"ПРЕВЫШЕНИЕ ЛИМИТА: Пользователь {entry.user_email}, IP-адресов: {current_ip_count}/{MAX_IPS_PER_USER}.")
                
                all_user_ips = await redis_client.smembers(user_ips_key)
                
                # Отправка команды на блокировку через RabbitMQ
                if blocking_exchange and all_user_ips:
                    block_message_body = {"ips": list(all_user_ips), "duration": BLOCK_DURATION}
                    message = aio_pika.Message(
                        body=json.dumps(block_message_body).encode(),
                        delivery_mode=aio_pika.DeliveryMode.PERSISTENT
                    )
                    await blocking_exchange.publish(message, routing_key="")
                    logger.info(f"Сообщение о блокировке для {entry.user_email} отправлено.")
                
                # Установка кулдауна на алерты и отправка уведомления
                await redis_client.setex(alert_sent_key, ALERT_COOLDOWN_SECONDS, "1")
                if ALERT_WEBHOOK_URL:
                    alert_payload = AlertPayload(
                        user_identifier=entry.user_email,
                        detected_ips_count=current_ip_count,
                        limit=MAX_IPS_PER_USER,
                    )
                    try:
                        await http_client.post(ALERT_WEBHOOK_URL, json=alert_payload.dict(), timeout=10.0)
                    except httpx.RequestError as e:
                        logger.error(f"Ошибка отправки алерта для {entry.user_email}: {e}")

        except Exception as e:
            logger.error(f"Критическая ошибка при обработке записи для {entry.user_email}: {e}")

    return {"status": "ok", "processed_entries": len(entries)}

@app.get("/health")
async def health_check():
    """Эндпоинт для проверки работоспособности сервиса."""
    try:
        await redis_client.ping()
        return {"status": "ok", "redis_connection": "ok"}
    except Exception as e:
        raise HTTPException(status_code=503, detail=f"Redis connection failed: {e}")