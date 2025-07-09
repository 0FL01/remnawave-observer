import asyncio
import json
import os
import logging
import aio_pika

# --- Настройка логирования ---
# Настраиваем логирование для вывода информации о работе воркера
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - [BlockerWorker] - %(message)s'
)

# --- Загрузка конфигурации из переменных окружения ---
# Получаем URL для подключения к RabbitMQ из .env файла
# Если переменная не задана, используется значение по умолчанию для локального тестирования
RABBITMQ_URL = os.getenv("RABBITMQ_URL", "amqp://guest:guest@localhost/")
# Имя exchange, должно совпадать с тем, что используется в observer'е
BLOCKING_EXCHANGE_NAME = "blocking_exchange"

async def run_command(command: str):
    """
    Асинхронно и безопасно выполняет системную команду.
    Используется для взаимодействия с nftables.
    """
    # Создаем подпроцесс для выполнения команды в shell
    proc = await asyncio.create_subprocess_shell(
        command,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE)

    # Ожидаем завершения команды и получаем ее вывод
    stdout, stderr = await proc.communicate()

    # Проверяем код возврата. Если не 0, значит произошла ошибка.
    if proc.returncode != 0:
        logging.error(f"Ошибка выполнения команды '{command}': {stderr.decode().strip()}")
    else:
        logging.info(f"Команда '{command}' выполнена успешно.")

async def main():
    """
    Основная функция, которая запускает воркер.
    """
    logging.info("Запуск Blocker Worker...")
    # Используем connect_robust для автоматического переподключения в случае сбоев сети
    connection = await aio_pika.connect_robust(RABBITMQ_URL)

    async with connection:
        # Создаем канал для общения с RabbitMQ
        channel = await connection.channel()
        # Ограничиваем воркер обработкой одного сообщения за раз для стабильности
        await channel.set_qos(prefetch_count=1)

        # Объявляем fanout exchange. Он должен уже существовать, но эта команда
        # является идемпотентной и не вызовет ошибки, если exchange уже есть.
        exchange = await channel.declare_exchange(
            BLOCKING_EXCHANGE_NAME, aio_pika.ExchangeType.FANOUT, durable=True
        )

        # Объявляем эксклюзивную очередь. У каждого воркера (на каждой ноде) будет своя
        # уникальная очередь, которая автоматически удалится при его отключении.
        queue = await channel.declare_queue(exclusive=True)
        # Привязываем нашу очередь к exchange, чтобы получать от него сообщения
        await queue.bind(exchange)

        logging.info(" [*] Воркер готов. Ожидание сообщений о блокировке...")
        
        # Запускаем бесконечный цикл прослушивания очереди
        async with queue.iterator() as queue_iter:
            async for message in queue_iter:
                # Контекстный менеджер автоматически подтверждает (ack) успешную обработку
                # или отклоняет (nack) сообщение в случае ошибки.
                async with message.process():
                    try:
                        # Декодируем тело сообщения и парсим JSON
                        payload = json.loads(message.body.decode())
                        ips_to_block = payload.get("ips", [])
                        duration = payload.get("duration", "5m")

                        if ips_to_block:
                            # Формируем строку со списком IP для команды nftables
                            ip_list_str = ", ".join(ips_to_block)
                            # Собираем полную команду для выполнения
                            command = f"nft add element inet firewall user_blacklist {{ {ip_list_str} timeout {duration} }}"
                            await run_command(command)
                            
                    except json.JSONDecodeError:
                        logging.error(f"Не удалось декодировать JSON из сообщения: {message.body.decode()}")
                    except Exception as e:
                        logging.error(f"Произошла непредвиденная ошибка при обработке сообщения: {e}")

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        logging.info("Воркер остановлен вручную.")
    except Exception as e:
        logging.error(f"Критическая ошибка в работе воркера: {e}")