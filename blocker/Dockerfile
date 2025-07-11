FROM python:3.11-slim AS builder
WORKDIR /app
COPY requirements.txt .
RUN pip wheel --no-cache-dir --wheel-dir /app/wheels -r requirements.txt

FROM python:3.11-slim
WORKDIR /app
RUN apt-get update && \
    apt-get install -y --no-install-recommends nftables && \
    rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/wheels /wheels
RUN pip install --no-cache /wheels/*
COPY blocker_worker.py .
CMD ["python", "-u", "blocker_worker.py"]