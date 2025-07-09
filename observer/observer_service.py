import os
import json
import logging
import asyncio
from typing import List, Set, Dict
from datetime import datetime
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
MONITORING_INTERVAL = int(os.getenv("MONITORING_INTERVAL", 300))  # 5 минут по умолчанию

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
monitoring_task = None

async def monitor_user_ip_pools():
    """Периодический мониторинг IP-пулов пользователей."""
    while True:
        try:
            await asyncio.sleep(MONITORING_INTERVAL)
            
            # Получаем все ключи пользователей
            pattern = "user_ips:*"
            user_keys = await redis_client.keys(pattern)
            
            if not user_keys:
                print(f"[{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}] === IP POOLS MONITORING === НЕТ АКТИВНЫХ ПОЛЬЗОВАТЕЛЕЙ")
                continue
            
            print(f"\n[{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}] === IP POOLS MONITORING START ===")
            
            total_users = 0
            users_near_limit = 0
            users_over_limit = 0
            
            # Собираем информацию о каждом пользователе
            user_stats: List[Dict] = []
            
            for key in user_keys:
                try:
                    user_email = key.split(":", 1)[1]
                    
                    # Получаем количество IP и TTL
                    async with redis_client.pipeline() as pipe:
                        pipe.scard(key)
                        pipe.ttl(key)
                        pipe.smembers(key)
                        results = await pipe.execute()
                    
                    ip_count = results[0]
                    ttl = results[1]
                    ips = results[2]
                    
                    if ip_count > 0:
                        total_users += 1
                        
                        # Определяем статус пользователя
                        status = "NORMAL"
                        if ip_count >= MAX_IPS_PER_USER * 0.8:  # 80% от лимита
                            status = "NEAR_LIMIT"
                            users_near_limit += 1
                        if ip_count > MAX_IPS_PER_USER:
                            status = "OVER_LIMIT"
                            users_over_limit += 1
                        
                        # Проверяем, есть ли активный кулдаун на алерты
                        alert_cooldown_key = f"alert_sent:{user_email}"
                        has_alert_cooldown = await redis_client.exists(alert_cooldown_key)
                        
                        user_stats.append({
                            'email': user_email,
                            'ip_count': ip_count,
                            'ips': sorted(list(ips)),
                            'ttl_hours': round(ttl / 3600, 1) if ttl > 0 else 0,
                            'status': status,
                            'has_alert_cooldown': bool(has_alert_cooldown),
                            'excluded': user_email in EXCLUDED_USERS
                        })
                
                except Exception as e:
                    logger.error(f"Ошибка при обработке ключа {key}: {e}")
            
            # Сортируем по количеству IP (по убыванию)
            user_stats.sort(key=lambda x: x['ip_count'], reverse=True)
            
            # Выводим общую статистику
            print(f"📊 ОБЩАЯ СТАТИСТИКА:")
            print(f"   👥 Всего активных пользователей: {total_users}")
            print(f"   ⚠️  Близко к лимиту ({MAX_IPS_PER_USER}): {users_near_limit}")
            print(f"   🚨 Превышение лимита: {users_over_limit}")
            print(f"   🛡️  Исключенных пользователей: {len([u for u in user_stats if u['excluded']])}")
            
            # Выводим топ-10 пользователей с наибольшим количеством IP
            print(f"\n📈 ТОП ПОЛЬЗОВАТЕЛИ ПО КОЛИЧЕСТВУ IP:")
            for i, user in enumerate(user_stats[:10], 1):
                status_emoji = {
                    'NORMAL': '✅',
                    'NEAR_LIMIT': '⚠️',
                    'OVER_LIMIT': '🚨'
                }.get(user['status'], '❓')
                
                excluded_marker = ' [EXCLUDED]' if user['excluded'] else ''
                cooldown_marker = ' [ALERT_COOLDOWN]' if user['has_alert_cooldown'] else ''
                
                print(f"   {i:2d}. {status_emoji} {user['email']}{excluded_marker}{cooldown_marker}")
                print(f"       IP: {user['ip_count']}/{MAX_IPS_PER_USER} | TTL: {user['ttl_hours']}h")
                print(f"       IPs: {', '.join(user['ips'])}")
            
            # Отдельно выводим всех пользователей с превышением лимита
            over_limit_users = [u for u in user_stats if u['status'] == 'OVER_LIMIT']
            if over_limit_users:
                print(f"\n🚨 ПОЛЬЗОВАТЕЛИ С ПРЕВЫШЕНИЕМ ЛИМИТА:")
                for user in over_limit_users:
                    excluded_marker = ' [EXCLUDED - НЕ БЛОКИРУЕТСЯ]' if user['excluded'] else ''
                    cooldown_marker = ' [ALERT_COOLDOWN]' if user['has_alert_cooldown'] else ''
                    print(f"   • {user['email']}{excluded_marker}{cooldown_marker}")
                    print(f"     IP: {user['ip_count']}/{MAX_IPS_PER_USER} | TTL: {user['ttl_hours']}h")
                    print(f"     IPs: {', '.join(user['ips'])}")
            
            print(f"[{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}] === IP POOLS MONITORING END ===\n")
            
        except Exception as e:
            logger.error(f"Критическая ошибка в мониторинге IP-пулов: {e}")
            await asyncio.sleep(30)  # Короткая пауза перед повторной попыткой

@app.on_event("startup")
async def startup_event():
    """Подключение к RabbitMQ и запуск мониторинга при старте приложения."""
    global rabbitmq_connection, blocking_exchange, monitoring_task
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
    
    # Запуск задачи мониторинга IP-пулов
    monitoring_task = asyncio.create_task(monitor_user_ip_pools())
    logger.info(f"Мониторинг IP-пулов запущен с интервалом {MONITORING_INTERVAL} секунд.")

@app.on_event("shutdown")
async def shutdown_event():
    """Корректное закрытие соединений."""
    global monitoring_task
    
    # Остановка задачи мониторинга
    if monitoring_task:
        monitoring_task.cancel()
        try:
            await monitoring_task
        except asyncio.CancelledError:
            pass
        logger.info("Мониторинг IP-пулов остановлен.")
    
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

@app.get("/user-ip-stats")
async def get_user_ip_stats():
    """Эндпоинт для получения статистики IP-пулов пользователей в реальном времени."""
    try:
        pattern = "user_ips:*"
        user_keys = await redis_client.keys(pattern)
        
        if not user_keys:
            return {"total_users": 0, "users": []}
        
        user_stats = []
        
        for key in user_keys:
            try:
                user_email = key.split(":", 1)[1]
                
                async with redis_client.pipeline() as pipe:
                    pipe.scard(key)
                    pipe.ttl(key)
                    pipe.smembers(key)
                    results = await pipe.execute()
                
                ip_count = results[0]
                ttl = results[1]
                ips = results[2]
                
                if ip_count > 0:
                    alert_cooldown_key = f"alert_sent:{user_email}"
                    has_alert_cooldown = await redis_client.exists(alert_cooldown_key)
                    
                    user_stats.append({
                        'email': user_email,
                        'ip_count': ip_count,
                        'ips': sorted(list(ips)),
                        'ttl_seconds': ttl if ttl > 0 else 0,
                        'over_limit': ip_count > MAX_IPS_PER_USER,
                        'has_alert_cooldown': bool(has_alert_cooldown),
                        'excluded': user_email in EXCLUDED_USERS
                    })
            
            except Exception as e:
                logger.error(f"Ошибка при обработке ключа {key}: {e}")
        
        user_stats.sort(key=lambda x: x['ip_count'], reverse=True)
        
        return {
            "total_users": len(user_stats),
            "users_over_limit": len([u for u in user_stats if u['over_limit']]),
            "monitoring_interval": MONITORING_INTERVAL,
            "max_ips_per_user": MAX_IPS_PER_USER,
            "users": user_stats
        }
    
    except Exception as e:
        logger.error(f"Ошибка при получении статистики IP-пулов: {e}")
        raise HTTPException(status_code=500, detail=f"Failed to get IP stats: {e}")