import os
import logging
from typing import List, Set

import httpx
import redis.asyncio as redis
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

# --- Настройка логирования ---
logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')
logger = logging.getLogger(__name__)

# --- Конфигурация из переменных окружения ---
REDIS_URL = os.getenv("REDIS_URL", "redis://localhost:6379/0")
MAX_IPS_PER_USER = int(os.getenv("MAX_IPS_PER_USER", 3))
ALERT_WEBHOOK_URL = os.getenv("ALERT_WEBHOOK_URL")
USER_IP_TTL_SECONDS = int(os.getenv("USER_IP_TTL_SECONDS", 24 * 60 * 60))
ALERT_COOLDOWN_SECONDS = int(os.getenv("ALERT_COOLDOWN_SECONDS", 60 * 60))

excluded_users_str = os.getenv("EXCLUDED_USERS", "")
EXCLUDED_USERS: Set[str] = {email.strip() for email in excluded_users_str.split(',') if email.strip()}

# --- Проверка конфигурации при старте ---
if not ALERT_WEBHOOK_URL:
    logger.warning("ALERT_WEBHOOK_URL не задан. Алерты не будут отправляться.")
if EXCLUDED_USERS:
    logger.info(f"Загружен список исключений. {len(EXCLUDED_USERS)} пользователей не будут проверяться: {', '.join(EXCLUDED_USERS)}")

# --- Модели данных (Pydantic) для валидации ---
class LogEntry(BaseModel):
    user_email: str = Field(..., alias="user_email")
    source_ip: str = Field(..., alias="source_ip")

class AlertPayload(BaseModel):
    user_identifier: str
    detected_ips_count: int
    limit: int
    violation_type: str = "ip_limit_exceeded"

# --- Инициализация приложения ---
app = FastAPI(
    title="Observer Service",
    description="Сервис для обнаружения превышения лимитов подключений.",
    version="1.1.0"
)

redis_client = redis.from_url(REDIS_URL, decode_responses=True)
http_client = httpx.AsyncClient()

@app.on_event("shutdown")
async def shutdown_event():
    await http_client.aclose()
    await redis_client.aclose()
    logger.info("Соединения с HTTP и Redis успешно закрыты.")

# --- Основная логика ---
@app.post("/log-entry")
async def process_log_entries(entries: List[LogEntry]):
    for entry in entries:
        try:
            if entry.user_email in EXCLUDED_USERS:
                logger.info(f"Пользователь {entry.user_email} находится в списке исключений. Проверка пропущена.")
                continue

            # Ключи в Redis
            user_ips_key = f"user_ips:{entry.user_email}"
            alert_sent_key = f"alert_sent:{entry.user_email}"

            # Оптимизация с Redis Pipeline
            async with redis_client.pipeline() as pipe:
                pipe.sadd(user_ips_key, entry.source_ip)
                pipe.expire(user_ips_key, USER_IP_TTL_SECONDS)
                pipe.scard(user_ips_key)
                pipe.exists(alert_sent_key)
                results = await pipe.execute()

            current_ip_count = results[2]
            alert_was_sent = bool(results[3])

            logger.info(
                f"Проверка пользователя: {entry.user_email}. "
                f"IP-адресов: {current_ip_count}. "
                f"Лимит: {MAX_IPS_PER_USER}. "
                f"Алерт в кулдауне: {alert_was_sent}."
            )

            # Проверяем условие для отправки алерта
            if current_ip_count > MAX_IPS_PER_USER and not alert_was_sent:
                logger.warning(
                    f"ПРЕВЫШЕНИЕ ЛИМИТА! Пользователь: {entry.user_email}, "
                    f"IP-адресов: {current_ip_count}/{MAX_IPS_PER_USER}."
                )
                
                await redis_client.setex(alert_sent_key, ALERT_COOLDOWN_SECONDS, "1")
                
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

    return {"status": "ok", "processed_entries": len(entries)}

@app.get("/health")
async def health_check():
    try:
        await redis_client.ping()
        return {"status": "ok", "redis_connection": "ok"}
    except Exception as e:
        raise HTTPException(status_code=503, detail=f"Redis connection failed: {e}")

