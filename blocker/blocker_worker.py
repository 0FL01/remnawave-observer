import asyncio
import json
import os
import logging
import aio_pika

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
    Основная функция, которая запускает воркер.
    """
    logging.info("Запуск Blocker Worker...")
    connection = await aio_pika.connect_robust(RABBITMQ_URL)

    async with connection:
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
                            # Создаем список задач, где каждая задача - это выполнение
                            # отдельной команды nft для каждого IP.
                            tasks = []
                            for ip in ips_to_block:
                                # Формируем простую и надежную команду для одного IP
                                command = f"nft add element inet firewall user_blacklist {{ {ip} timeout {duration} }}"
                                tasks.append(run_command(command))
                            
                            # Запускаем все команды параллельно и ждем их завершения
                            await asyncio.gather(*tasks)
                            
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