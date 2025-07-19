# Remnawave Observer: Модуль для борьбы с совместным использованием подписок

![Go](https://img.shields.io/badge/Go-1.24-00ADD8?style=for-the-badge&logo=go)
![Docker](https://img.shields.io/badge/Docker-28.0-2496ED?style=for-the-badge&logo=docker)
![RabbitMQ](https://img.shields.io/badge/RabbitMQ-4.1-FF6600?style=for-the-badge&logo=rabbitmq)
![Redis](https://img.shields.io/badge/Redis-8.2-DC382D?style=for-the-badge&logo=redis)
![nftables](https://img.shields.io/badge/nftables-1.0.9-orange?style=for-the-badge)

## Краткое описание

**Remnawave Observer** — это внешний модуль для расширения функционала панели управления Remnawave. Его основная задача — автоматически обнаруживать и блокировать пользователей, которые делятся своей подпиской с другими людьми, передавая им доступ. Система отслеживает, с какого количества уникальных IP-адресов подключается каждый пользователь, и при превышении установленного лимита временно блокирует доступ для всех IP-адресов этого пользователя.

Это помогает защитить ваш сервис от несанкционированного использования и потери дохода.

## Проблема

Когда пользователь покупает подписку на одного человека, а затем делится доступом с друзьями, семьей или продает его третьим лицам, сервис теряет потенциальных клиентов и, соответственно, доход. Ручное отслеживание таких нарушителей практически невозможно и требует огромных временных затрат.

## Решение

Remnawave Observer автоматизирует этот процесс. Он работает в фоновом режиме, анализируя подключения пользователей к вашим серверам (нодам) и принимая меры в реальном времени.

## Как это работает?

Система состоит из двух основных частей: центрального **сервера-наблюдателя (Observer)** и **агентов-блокировщиков (Blocker)**, установленных на каждой вашей ноде (сервере с Xray).

Вот пошаговая схема работы:

1.  **Сбор данных**: Пользователь подключается к одной из ваших `remnanode` (серверу с Xray). Xray записывает в лог-файл информацию о подключении: время, IP-адрес и `email` пользователя.
2.  **Парсинг логов**: На каждой ноде работает легкий сервис `Vector`. Он непрерывно читает лог-файл Xray, извлекает из него только нужную информацию (`email` и `IP-адрес`) и отправляет эти данные на центральный сервер-наблюдатель.
3.  **Анализ и подсчет**: Центральный сервер `Observer` получает данные от всех нод. Он использует базу данных `Redis` для ведения учета: для каждого пользователя (`email`) он хранит список уникальных IP-адресов, с которых были подключения. Система умная: даже если у вас 10 нод и пользователь переключается между ними, `Observer` не создаст дубликатов и будет вести единый счетчик IP для каждого пользователя.
4.  **Принятие решения**: `Observer` постоянно сравнивает количество IP-адресов пользователя с установленным лимитом (например, 12 IP).
5.  **Команда на блокировку**: Как только лимит превышен, `Observer` отправляет команду на блокировку через брокер сообщений `RabbitMQ`. Эта команда содержит список всех IP-адресов нарушителя.
6.  **Исполнение блокировки**: Агент `Blocker` на *каждой* вашей ноде получает эту команду. Он немедленно добавляет все IP-адреса из команды в специальный "черный список" в `nftables` (современный файрвол в Linux).
7.  **Результат**: `nftables` на всех нодах начинает блокировать любой трафик от этих IP-адресов на определенное время. Пользователь и те, с кем он поделился доступом, теряют подключение.

### Визуальная схема архитектуры

```
[ Пользователь 1 (IP: A) ] ----> [ Remnanode 1 ]
                                      |
[ Пользователь 1 (IP: B) ] ----> [ Remnanode 2 ]
                                      |
[ Пользователь 1 (IP: C) ] ----> [ Remnanode 3 ]
                                      |
                                      V
+-----------------------------------------------------------------------------+
| На каждой Remnanode:                                                        |
|   1. Xray (пишет в access.log)                                              |
|   2. Vector (парсит лог -> {email, ip}) ----> [ Nginx на сервере Observer ] |
|   3. Blocker (слушает команды от RabbitMQ)                                  |
|   4. nftables (файрвол с черным списком `user_blacklist`)                   |
+-----------------------------------------------------------------------------+
                                      |
                                      V
+-------------------------------------------------------------------------+
| Центральный сервер Observer:                                            |
|   1. Nginx (принимает данные от Vector'ов)                              |
|   2. Vector Aggregator (передает данные в Observer Service)             |
|   3. Observer Service (главная логика)                                  |
|      |-> [ Redis ] (хранит: user -> {IP1, IP2, ...})                    |
|      |                                                                  |
|      +-- (Если лимит превышен) --> [ RabbitMQ ] (отправляет команду)    |
|                                         |                               |
|   4. RabbitMQ (брокер сообщений) -------+--> (Команда всем Blocker'ам)  |
+-------------------------------------------------------------------------+
```

## Системные требования

### 1. Сервер для Observer (панель)

*   **ОС**: Debian 12
*   **RAM**: 2 GB
*   **CPU**: 1 vCPU

### 2. Сервер для Remnanode (каждая нода с Xray)

*   **ОС**: Debian 12
*   **RAM**: 1 GB
*   **CPU**: 1 vCPU
*   **Зависимости**: Установленный и включенный пакет `nftables`.

## Установка и настройка

### Шаг 1: Настройка сервера Observer (центральная панель)

1.  Подключитесь к серверу, где будет работать Observer.
2.  Установите `docker` и `docker-compose`.
3.  Скопируйте папку `observer-conf` на сервер (например, через `scp` или `git clone`).
    ```bash
    # Пример, если вы клонировали весь репозиторий:
    cd remnawave-observer-main/observer-conf
    ```
4.  Создайте и отредактируйте файл с переменными окружения:
    ```bash
    cp .env.example .env
    vim .env
    ```
    Заполните переменные:
    *   `MAX_IPS_PER_USER`: Рекомендуемое значение **12** или выше. Это позволяет пользователям без проблем переключаться между домашним Wi-Fi, мобильным интернетом (LTE), рабочим Wi-Fi и т.д.
    *   `ALERT_WEBHOOK_URL`: URL для отправки уведомлений (например, в Telegram).
    *   `EXCLUDED_USERS`: Email-адреса пользователей через запятую, которых не нужно проверять (например, `user1@example.com,user2@example.com`).
    *   `EXCLUDED_IPS`: **ВАЖНО!** IP-адреса, которые никогда не будут заблокированы. Укажите здесь IP-адреса всех ваших нод и сервера Observer, чтобы избежать случайной блокировки (например, `8.8.8.8,1.1.1.1,192.168.1.1`).
    *   `RABBIT_USER`, `RABBIT_PASSWD`: Создайте надежные логин и пароль для RabbitMQ.
    *   `RABBITMQ_URL`: Сформируйте URL для **внутреннего** использования сервисом Observer. Пример: `amqp://myuser:mypassword@rabbitmq:5672/`.

5.  Настройте Nginx. Вам понадобится домен и SSL-сертификат (например, от Let's Encrypt).
    ```bash
    vim nginx.conf
    ```
    Замените все вхождения `HEAD_DOMAIN` на ваш реальный домен. Убедитесь, что SSL-сертификаты (`fullchain.pem` и `privkey.pem`) находятся по указанным путям `/etc/letsencrypt/live/HEAD_DOMAIN/`.

6.  **Важно!** Для подключения удаленных нод к RabbitMQ необходимо использовать безопасное соединение. Убедитесь, что порт `5671` (стандартный для AMQPS) вашего сервера Observer доступен извне. Вам может потребоваться дополнительная настройка прокси или самого RabbitMQ для работы с SSL.

7.  Запустите все сервисы:
    ```bash
    docker-compose up -d
    ```

### Шаг 2: Настройка каждой Remnanode (ноды с Xray)

На *каждом* сервере, где работает Xray, выполните следующие действия.

#### 2.1. Настройка файрвола `nftables`

Это **критически важный** шаг. `Blocker` жестко запрограммирован на работу с конкретным набором правил в `nftables`.

**!! Убедитесь что у вас отключены другие фаерволы по типу ufw или iptables !!**

1.  Установите `nftables`:
    ```bash
    apt update && apt install nftables -y
    ```

2.  Создайте и откройте файл конфигурации:
    ```bash
    vim /etc/nftables.conf
    ```

3.  Скопируйте в него всё содержимое из файла `nftables_example.conf`, который находится в корне репозитория.

4.  **Отредактируйте файл `/etc/nftables.conf`**:
    *   Найдите строку `define SSH_PORT = 22` и **обязательно** измените порт, если вы используете нестандартный SSH-порт для подключения к ноде. Например, если ваш SSH порт 6666, измените на `define SSH_PORT = 6666`.
    *   Найдите секцию `set control_plane_sources` и в `elements = { IP_ADRESS }` впишите IP-адрес вашего сервера **Observer** и панели управления **Remnawave**.
    *   Найдите секцию `set monitoring_sources` и в `elements = { IP_ADRESS }` впишите IP-адрес вашего сервера мониторинга, если он есть.

5.  Примените правила и добавьте `nftables` в автозагрузку:
    ```bash
    # Применяем правила из файла
    nft -f /etc/nftables.conf

    # Включаем сервис и добавляем в автозагрузку, чтобы правила применялись после перезагрузки
    systemctl enable --now nftables
    ```
    **Внимание!** `Blocker` будет добавлять IP-адреса в набор `set user_blacklist` в таблице `table inet firewall`. Не изменяйте эти имена, иначе блокировка работать не будет.

#### 2.2. Интеграция Blocker и Vector в Remnanode

Сервисы `blocker-xray` и `vector` должны быть добавлены в ваш существующий `docker-compose.yml` файл, который управляет `remnanode` (обычно находится в `/opt/remnanode/`).

1.  Перейдите в рабочую директорию вашей ноды:
    ```bash
    cd /opt/remnanode/
    ```

2.  Создайте файл `.env` для хранения учетных данных RabbitMQ:
    ```bash
    vim .env
    ```
    Добавьте в него следующую строку, заменив значения на ваши:
    ```
    # Используйте протокол amqps для безопасного соединения
    RABBITMQ_URL=amqps://ВАШ_RABBIT_USER:ВАШ_RABBIT_PASSWD@ВАШ_ДОМЕН_OBSERVER:5671/
    ```

3.  Скопируйте конфигурацию для Vector в текущую директорию. Предполагается, что вы скачали репозиторий.
    ```bash
    # Скопируйте файл из скачанного репозитория в текущую папку
    cp /path/to/remnawave-observer-main/blocker_conf/vector/vector.toml ./vector.toml
    ```

4.  Отредактируйте скопированный файл `vector.toml`:
    ```bash
    vim vector.toml
    ```
    Найдите строку `uri = "https://HEAD_DOMAIN:38213/"` и замените `HEAD_DOMAIN` на домен вашего сервера Observer.

5.  Откройте ваш основной файл `docker-compose.yml` (например, `/opt/remnanode/docker-compose.yml`).

6.  Добавьте сервисы `blocker-xray` и `vector` в конец этого файла. Убедитесь, что отступы соответствуют синтаксису YAML.

    ```yaml
    # ... ваш сервис remnanode и другие сервисы ...

      blocker-xray:
        container_name: blocker-xray
        hostname: blocker-xray
        image: quay.io/0fl01/blocker-xray-go:0.0.6
        restart: unless-stopped
        network_mode: host
        logging:
          driver: "json-file"
          options:
            max-size: "8m"
            max-file: "5"
        env_file:
          - .env
        cap_add:
          - NET_ADMIN
          - NET_RAW
        depends_on:
          - remnanode
        deploy:
          resources:
            limits:
              memory: 64M
              cpus: '0.25'
            reservations:
              memory: 32M
              cpus: '0.10'

      vector:
        image: timberio/vector:0.48.0-alpine
        container_name: vector
        hostname: vector
        restart: unless-stopped
        network_mode: host
        command: ["--config", "/etc/vector/vector.toml"]
        depends_on:
          - remnanode
        volumes:
          # Путь к файлу vector.toml, который вы создали на шаге 4
          - ./vector.toml:/etc/vector/vector.toml:ro
          # Путь к логам remnanode, должен совпадать с тем, что в сервисе remnanode
          - /var/log/remnanode:/var/log/remnanode:ro
        logging:
          driver: "json-file"
          options:
            max-size: "8m"
            max-file: "3"
        deploy:
          resources:
            limits:
              memory: 128M
              cpus: '0.25'
            reservations:
              memory: 64M
              cpus: '0.10'
    ```

7.  Убедитесь, что ваш сервис `remnanode` в этом же `docker-compose.yml` имеет volume для логов, как в примере: `volumes: - /var/log/remnanode:/var/log/remnanode`.

8.  Перезапустите все сервисы, чтобы применить изменения:
    ```bash
    docker-compose up -d
    ```

Повторите **Шаг 2** для всех ваших нод.

## Совместимость

*   **Debian 12**: Это основная и полностью поддерживаемая операционная система. Вся разработка и тестирование велись именно на ней.
*   **Ubuntu 24.04**: Теоретически, система должна работать, так как она использует Docker и стандартные утилиты Linux (такие как `nftables`). Однако полная совместимость не гарантируется, и могут возникнуть непредвиденные проблемы или различия в поведении. **Настоятельно рекомендуется использовать Debian 12 для стабильной и предсказуемой работы.**



---

### Детальная настройка и принцип работы связки Nginx + Vector

Чтобы система работала надежно и безопасно, данные от каждой `remnanode` к центральному серверу `Observer` должны передаваться по зашифрованному каналу. Для этого мы используем связку из Nginx (в качестве реверс-прокси с SSL) и Vector.

**Общая схема потока данных:**

`Vector (на Remnanode)` -> `Интернет (HTTPS)` -> `Nginx (на Observer)` -> `Vector-агрегатор (на Observer)` -> `Observer Service`

#### На сервере Observer

На центральном сервере Nginx выполняет две ключевые функции:
1.  **Терминирование SSL**: Принимает зашифрованный трафик от нод, расшифровывает его с помощью вашего SSL-сертификата.
2.  **Проксирование**: Передает уже расшифрованный, "чистый" HTTP-трафик на сервис `vector-aggregator`, который работает внутри Docker-сети и не доступен извне напрямую.

##### 1. Конфигурация docker compose (`observer-conf/docker-compose.yml`)

```yml
services:
  observer-remna:
    container_name: observer-remna
    image: quay.io/0fl01/observer-xray-go:0.0.19
    restart: unless-stopped
    expose:
      - "9000"
    env_file:
      - .env
    depends_on:
      rabbitmq-obs:
        condition: service_healthy
      vector-aggregator:
        condition: service_started
      redis-obs:
        condition: service_started
    networks:
      - observer-net
      - remnawave-network
    logging:
      driver: json-file
      options:
        max-size: "8m"
        max-file: "3"

  rabbitmq-obs:
    image: rabbitmq:4.1.2-alpine
    container_name: rabbitmq-obs
    restart: unless-stopped
    expose:
      - "5672"
      - "15672"
    volumes:
      - rabbitmq-obs-data:/var/lib/rabbitmq
    environment:
      - RABBITMQ_DEFAULT_USER=chumba
      - RABBITMQ_DEFAULT_PASS=${RABBIT_PASSWD}
    networks:
      - observer-net
    healthcheck:
      test: ["CMD", "rabbitmq-diagnostics", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 20s

  vector-aggregator:
    image: timberio/vector:0.48.0-alpine
    container_name: vector-aggregator
    restart: unless-stopped
    volumes:
      - ./vector.toml:/etc/vector/vector.toml:ro
    expose:
      - "8686"
    command: ["--config", "/etc/vector/vector.toml"]
    networks:
      - observer-net
    logging:
      driver: json-file
      options:
        max-size: "8m"
        max-file: "3"

  nginx-obs:
    image: nginx:mainline-alpine
    container_name: nginx-obs
    restart: unless-stopped
    ports:
      - "38213:38213"  # Vector HTTPS
      - "38214:38214"  # RabbitMQ AMQP SSL
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
      - /etc/letsencrypt:/etc/letsencrypt:ro
    depends_on:
      vector-aggregator:
        condition: service_started
      rabbitmq-obs:
        condition: service_healthy 
    networks:
      - observer-net
    logging:
      driver: json-file
      options:
        max-size: "8m"
        max-file: "3"

  redis-obs:
    image: redis:8.2-m01-alpine3.22
    container_name: redis-obs
    restart: unless-stopped
    expose:
      - "6379"
    volumes:
      - redis-obs-data:/data
    networks:
      - observer-net
    logging:
      driver: json-file
      options:
        max-size: "8m"
        max-file: "3"

networks:
  observer-net:
    driver: bridge
  remnawave-network:
    external: true

volumes:
  redis-obs-data:
  rabbitmq-obs-data:
```

##### 2. Конфигурация Nginx (`observer-conf/nginx.conf`)

```nginx
user nginx;
worker_processes auto;
pid /var/run/nginx.pid;
include /etc/nginx/modules-enabled/*.conf;

events {
    worker_connections 1024;
}

# Stream блок для AMQP с SSL
stream {
    # AMQP порт с SSL терминацией
    server {
        listen 38214 ssl;
        
        ssl_certificate /etc/letsencrypt/live/HEAD_DOMAIN/fullchain.pem;
        ssl_certificate_key /etc/letsencrypt/live/HEAD_DOMAIN/privkey.pem;
        ssl_protocols TLSv1.2 TLSv1.3;
        ssl_ciphers 'TLS_AES_128_GCM_SHA256:TLS_AES_256_GCM_SHA384:TLS_CHACHA20_POLY1305_SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-RSA-AES256-GCM-SHA384';
        ssl_prefer_server_ciphers off;
        ssl_session_cache shared:SSL_STREAM:10m;
        ssl_session_timeout 10m;
        ssl_handshake_timeout 10s;

        proxy_pass rabbitmq-obs:5672;
        proxy_timeout 300s;
        proxy_connect_timeout 5s;
    }
}

# HTTP блок для веб-интерфейсов
http {
    include       /etc/nginx/mime.types;
    default_type  application/octet-stream;
    sendfile        on;
    keepalive_timeout  65;
    
    # Логирование
    log_format main '$remote_addr - $remote_user [$time_local] "$request" '
                   '$status $body_bytes_sent "$http_referer" '
                   '"$http_user_agent" "$http_x_forwarded_for"';
    
    access_log /var/log/nginx/access.log main;
    error_log /var/log/nginx/error.log warn;

    # Vector на порту 38213
    server {
        listen 38213 ssl;
        http2 on;
        server_name HEAD_DOMAIN;

        ssl_certificate /etc/letsencrypt/live/HEAD_DOMAIN/fullchain.pem;
        ssl_certificate_key /etc/letsencrypt/live/HEAD_DOMAIN/privkey.pem;
        ssl_protocols TLSv1.2 TLSv1.3;
        ssl_ciphers 'TLS_AES_128_GCM_SHA256:TLS_AES_256_GCM_SHA384:TLS_CHACHA20_POLY1305_SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-RSA-AES256-GCM-SHA384';
        ssl_prefer_server_ciphers off;
        ssl_session_cache shared:SSL_HTTP:10m;
        ssl_session_timeout 10m;

        add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

        location / {
            proxy_pass http://vector-aggregator:8686;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_redirect off;
        }
    }

    # RabbitMQ Management UI на порту 38215
    server {
        listen 38215 ssl;
        http2 on;
        server_name v.just-another-domain.uk;

        ssl_certificate /etc/letsencrypt/live/v.just-another-domain.uk/fullchain.pem;
        ssl_certificate_key /etc/letsencrypt/live/v.just-another-domain.uk/privkey.pem;
        ssl_protocols TLSv1.2 TLSv1.3;
        ssl_ciphers 'TLS_AES_128_GCM_SHA256:TLS_AES_256_GCM_SHA384:TLS_CHACHA20_POLY1305_SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-RSA-AES256-GCM-SHA384';
        ssl_prefer_server_ciphers off;
        ssl_session_cache shared:SSL_HTTP:10m;
        ssl_session_timeout 10m;

        add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

        location / {
            proxy_pass http://rabbitmq-obs:15672;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_redirect off;
            
            # Поддержка WebSocket для RabbitMQ Management
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection "upgrade";
            proxy_cache_bypass $http_upgrade;
        }
    }
}
```

##### 3. Конфигурация Vector-агрегатора (`observer-conf/vector.toml`)

Этот Vector работает как приемник данных от Nginx и передатчик для основного сервиса `observer`.

```toml
# Источник: принимаем данные по HTTP от Nginx.
[sources.http_receiver]
  type = "http"
  # Слушаем на всех интерфейсах ВНУТРИ Docker-сети на порту 8686
  # Именно сюда Nginx и отправляет трафик
  address = "0.0.0.0:8686"
  decoding.codec = "json"

# Назначение: отправляем полученные данные в сервис-наблюдатель.
[sinks.observer_service]
  type = "http"
  inputs = ["http_receiver"]
  # Используем имя сервиса 'observer' из docker-compose и его внутренний порт
  # Это уже внутренняя коммуникация между контейнерами
  uri = "http://observer:9000/log-entry"
  method = "post"
  encoding.codec = "json"

  batch.max_events = 100
  batch.timeout_secs = 5

```

#### На каждой ноде (Remnanode)

На каждой ноде Vector работает как агент: он читает локальные логи и отправляет их на защищенный публичный адрес вашего сервера Observer.

##### 4. Конфигурация Vector-агента (`blocker_conf/vector/vector.toml`)

```toml
# Источник данных: читаем access.log из директории remnanode.
[sources.xray_access_logs]
  type = "file"
  include = ["/var/log/remnanode/access.log"] 
  read_from = "end"

# Трансформация: парсим каждую строку лога, чтобы извлечь email и IP.
[transforms.parse_xray_log]
  type = "remap"
  inputs = ["xray_access_logs"]
  source = '''
    pattern = r'from (tcp:)?(?P<ip>\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):\d+.*? email: (?P<email>\S+)'
    parsed, err = parse_regex(.message, pattern)
    if err != null {
      log("Не удалось распарсить строку лога: " + err, level: "warn")
      abort
    }
    . = { "user_email": parsed.email, "source_ip": parsed.ip, "timestamp": to_string(now()) }
  '''

# Назначение: отправляем обработанные данные на наш центральный сервер-наблюдатель.
[sinks.central_observer_api]
  type = "http"
  inputs = ["parse_xray_log"]
  # ВАЖНО: Указываем HTTPS и ваш домен!
  # Vector на ноде обращается к публичному адресу сервера Observer.
  # Запрос сначала попадает на Nginx, который слушает порт 443 (в docker-compose он проброшен как 38213:443).
  # Поэтому здесь мы указываем внешний порт 38213.
  uri = "https://HEAD_DOMAIN:38213/"
  method = "post"
  encoding.codec = "json"
  compression = "gzip"

  [sinks.central_observer_api.batch]
    max_events = 100
    timeout_secs = 5

  [sinks.central_observer_api.request]
    retry_attempts = 5
    retry_backoff_secs = 2

  [sinks.central_observer_api.tls]
```