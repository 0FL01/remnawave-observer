import asyncio
import json
import os
import logging
import aio_pika
from aio_pika.exceptions import AMQPConnectionError

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - [BlockerWorker] - %(message)s'
)

RABBITMQ_URL = os.getenv("RABBITMQ_URL", "amqp://guest:guest@localhost/")
BLOCKING_EXCHANGE_NAME = "blocking_exchange"

async def run_command(command: str):
    """
    Выполняет одну команду nftables.
    """
    proc = await asyncio.create_subprocess_shell(
        command,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE)

    stdout, stderr = await proc.communicate()

    if proc.returncode != 0:
        logging.error(f"Ошибка выполнения команды '{command}': {stderr.decode().strip()}")
    else:
        logging.info(f"Команда '{command}' выполнена успешно.")

async def main():
    """
    Основная функция, которая запускает воркер с надежным циклом переподключения.
    """
    logging.info("Запуск Blocker Worker...")
    
    # Создаем бесконечный цикл для обеспечения переподключения
    while True:
        try:
            # При каждой итерации цикла будет происходить новый DNS-запрос
            # благодаря вызову aio_pika.connect
            connection = await aio_pika.connect(RABBITMQ_URL)
            logging.info("Соединение с RabbitMQ успешно установлено.")

            async with connection:
                # Логика работы с каналом и очередью остается прежней
                channel = await connection.channel()
                await channel.set_qos(prefetch_count=1)

                exchange = await channel.declare_exchange(
                    BLOCKING_EXCHANGE_NAME, aio_pika.ExchangeType.FANOUT, durable=True
                )

                queue = await channel.declare_queue(exclusive=True)
                await queue.bind(exchange)

                logging.info(" [*] Воркер готов. Ожидание сообщений о блокировке...")
                
                async with queue.iterator() as queue_iter:
                    async for message in queue_iter:
                        async with message.process():
                            try:
                                payload = json.loads(message.body.decode())
                                ips_to_block = payload.get("ips", [])
                                duration = payload.get("duration", "5m")

                                if ips_to_block:
                                    tasks = []
                                    for ip in ips_to_block:
                                        command = f"nft add element inet firewall user_blacklist {{ {ip} timeout {duration} }}"
                                        tasks.append(run_command(command))
                                    
                                    await asyncio.gather(*tasks)
                                    
                            except json.JSONDecodeError:
                                logging.error(f"Не удалось декодировать JSON из сообщения: {message.body.decode()}")
                            except Exception as e:
                                logging.error(f"Произошла непредвиденная ошибка при обработке сообщения: {e}")

        # Перехватываем конкретные ошибки соединения
        except (AMQPConnectionError, ConnectionError) as e:
            logging.warning(f"Не удалось подключиться к RabbitMQ или соединение было потеряно: {e}. Повторная попытка через 5 секунд...")
            await asyncio.sleep(5)
        except Exception as e:
            logging.error(f"Произошла критическая непредвиденная ошибка: {e}. Повторная попытка через 5 секунд...")
            await asyncio.sleep(5)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        logging.info("Воркер остановлен вручную.")