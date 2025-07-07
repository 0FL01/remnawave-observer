FROM python:3.11-slim as builder

WORKDIR /app

COPY requirements.txt .

RUN pip wheel --no-cache-dir --wheel-dir /app/wheels -r requirements.txt


FROM python:3.11-slim

WORKDIR /app

COPY --from=builder /app/wheels /wheels

RUN pip install --no-cache /wheels/*

COPY observer_service.py .

EXPOSE 9000

CMD ["uvicorn", "observer_service:app", "--host", "0.0.0.0", "--port", "9000"]

