# Демонстрационные сценарии (ЛР4)

Все сценарии проверены на системе, запущенной командой `docker compose up -d --build`
(Kafka в KRaft-режиме, 4 сервиса, Nginx-gateway, банк-заглушка). Клиентские запросы идут
**только через API Gateway** `http://localhost:8080`. Internal-эндпоинты (`/internal/...`)
снаружи закрыты и в сценариях вызываются напрямую в сервис только для проверки результата.

Как воспроизвести:

* автоматически — `scripts/test-saga.ps1 -Order http://localhost:8080 -Dispatch http://localhost:8080` (сценарии 1 и 3) и `scripts/test-gateway.ps1`;
* вручную по шагам — Postman-коллекция `postman/delivery-lab4.postman_collection.json` (папки 1–4);
* без Docker — сквозные тесты `go test ./tests/e2e/ -v` (сценарии 1–4, in-memory брокер).

Обозначения: 🟢 реальный сервис/инфраструктура, 🟡 мок.

| Участник | Реализация |
| :--- | :--- |
| order-, dispatch-, payment-, notification-service | 🟢 отдельные контейнеры |
| REST между сервисами | 🟢 настоящий HTTP |
| Kafka | 🟢 apache/kafka 3.9 |
| API Gateway | 🟢 Nginx |
| Платёжный шлюз банка | 🟡 `bank-mock` (HTTP) |
| SMS-провайдер | 🟡 in-process мок в notification-service |
| Геокодер | 🟡 in-process мок в dispatch-service |
| БД (PostgreSQL / Redis) | 🟡 in-memory хранилище в каждом сервисе |

---

## Сценарий 1. Успешная доставка (happy path)

Затрагивает 4 сервиса, 3 синхронных и 5 асинхронных взаимодействий.

| # | Действие | Взаимодействие | Ожидаемый результат | Результат |
| :-: | :--- | :--- | :--- | :-: |
| 1 | Курьер выходит на смену `POST /api/v1/couriers/shifts` | клиент → gateway → dispatch | `200`, `status: ONLINE` | ✅ |
| 2 | Курьер передаёт координаты `POST /api/v1/dispatch/location` | клиент → gateway → dispatch | `200`, `success: true` | ✅ |
| 3 | Клиент создаёт заказ `POST /api/v1/orders` с `Idempotency-Key` | клиент → gateway → order → **REST** → payment → 🟡 bank-mock | `201`, `PENDING_PAYMENT`, цена 450, `paymentUrl` банка; событие `order.created` | ✅ |
| 4 | Повтор запроса с тем же ключом | gateway → order | `201`, тот же `id`, `Idempotent-Replayed: true`, второй платёж не создан | ✅ |
| 5 | Клиент открывает `paymentUrl` | 🟡 bank-mock → gateway → payment (подписанный вебхук) | `payment.completed` в `payments.events` | ✅ |
| 6 | — | Kafka → order; Kafka → dispatch | order: `PAID`; dispatch начинает поиск | ✅ |
| 7 | — | dispatch → **REST** `GET /internal/v1/orders/{id}/status` → order | заказ активен → ближайший курьер назначен, `dispatch.courier_assigned` | ✅ |
| 8 | — | Kafka → order; Kafka → notification | order: `ASSIGNED`, `courierId`; SMS «курьер назначен» | ✅ |
| 9 | Курьер смотрит назначение `GET /api/v1/couriers/{id}/assignment` | gateway → dispatch | `200`, `orderId` заказа | ✅ |
| 10 | Курьер пытается закрыть смену | gateway → dispatch | `409` Problem: активный заказ | ✅ |
| 11 | Курьер отмечает доставку `POST /api/v1/couriers/{id}/assignment/complete` | gateway → dispatch → **REST** `POST /internal/v1/orders/{id}/delivered` → order | `200`; заказ `DELIVERED`; `order.delivered` | ✅ |
| 12 | — | Kafka → notification | SMS «заказ доставлен»; в журнале 2 SMS | ✅ |

Пример SMS из журнала:
```
+79990746236: Заказ 4f2aaf5a: курьер назначен и уже едет за посылкой.
+79990746236: Заказ 4f2aaf5a доставлен. Спасибо, что пользуетесь нашим сервисом!
```

---

## Сценарий 2. Сбой: банк отклонил оплату

| # | Действие | Ожидаемый результат | Результат |
| :-: | :--- | :--- | :-: |
| 1 | Создать заказ | `201`, `PENDING_PAYMENT` | ✅ |
| 2 | Открыть `paymentUrl?result=fail` (🟡 банк отклоняет) | payment: `FAILED`, событие `payment.failed` | ✅ |
| 3 | — | order: **`CANCELLED`, `cancelReason: PAYMENT_FAILED`** (компенсация) | ✅ |
| 4 | — | поиск курьера не запускается, курьер не назначается | ✅ |
| 5 | Повторный вебхук `SUCCESS` после `FAILED` | игнорируется, заказ остаётся отменённым | ✅ |

---

## Сценарий 3. Сбой: курьер не найден → сага с компенсациями

Курьеров на смене нет. `SEARCH_TIMEOUT=20s` (по ТЗ — 15 минут, уменьшено для демонстрации).

| # | Действие | Взаимодействие | Ожидаемый результат | Результат |
| :-: | :--- | :--- | :--- | :-: |
| 1 | Создать и оплатить заказ | как в сценарии 1 | заказ `PAID`, dispatch ищет курьера | ✅ |
| 2 | Ждём 20 с | dispatch: фоновый поиск каждые 2 с | событие `dispatch.search_failed` (`reason: COURIER_NOT_FOUND_TIMEOUT`) | ✅ |
| 3 | — | Kafka → order | **компенсация 1:** заказ `CANCELLED`, `cancelReason: COURIER_NOT_FOUND` | ✅ |
| 4 | — | Kafka → payment → 🟡 bank-mock `POST /refunds` | **компенсация 2:** платёж `REFUNDED`, событие `payment.refunded` | ✅ |
| 5 | — | Kafka → notification | SMS «заказ отменён, деньги будут возвращены» | ✅ |

Проверка возврата: `GET http://localhost:8083/internal/v1/payments/{paymentId}` → `status: REFUNDED`.

Дополнительно (банк недоступен при возврате): `$env:FAIL_REFUNDS="true"; docker compose up -d bank-mock` →
payment-service получает ошибку банка, событие обрабатывается повторно (до 5 попыток с паузой), платёж
остаётся `COMPLETED`, деньги не теряются; после исчерпания попыток событие уходит в `dispatch.events.dlq`
(видно в Kafka UI).

---

## Сценарий 4. Сбой: payment-service недоступен

```powershell
docker compose stop payment-service
```

| # | Действие | Ожидаемый результат | Результат |
| :-: | :--- | :--- | :-: |
| 1 | `POST /api/v1/orders` через gateway | order-service делает 3 попытки с паузой, затем `503` Problem Details `Зависимый сервис недоступен` | ✅ |
| 2 | — | **компенсация:** заказ сохранён как `CANCELLED`, `cancelReason: PAYMENT_SERVICE_UNAVAILABLE` | ✅ |
| 3 | Ещё несколько запросов подряд | после 5 неудач срабатывает circuit breaker: ответ `503 ... circuit breaker открыт` сразу, без обращения к payment | ✅ |
| 4 | `docker compose start payment-service`, через 15 с новый заказ | `201`, система восстановилась | ✅ |

---

## Сценарий 5. API Gateway

| Проверка | Ожидаемый результат | Результат |
| :--- | :--- | :-: |
| `GET /internal/v1/orders/{id}/status` через gateway | `404` Problem — internal-эндпоинты снаружи закрыты | ✅ |
| Неизвестный маршрут | `404` Problem | ✅ |
| Запрос без `X-Request-Id` | gateway генерирует id, он виден в логах всех сервисов цепочки | ✅ |
| Запрос с `X-Request-Id: gw-trace-1` | тот же id возвращается клиенту и попадает в логи сервиса | ✅ |
| 150 запросов подряд | часть отклонена `429` Problem (лимит 20 запросов/с, всплеск 40), через 2–3 с доступ восстанавливается | ✅ (отклонено ~105) |
| Вебхук банка | идёт через gateway (`/api/v1/payments/webhook`), поддельная подпись → `400` | ✅ |

---

## Где смотреть результат

* **Kafka UI** http://localhost:8090 → Topics → `payments.events` / `dispatch.events` / `orders.events` → Messages: события с `eventId`, `eventType`, `correlationId` (= `orderId`), `payload`.
* **Логи**: `docker logs -f dispatch-service` и т.д. — JSON-строки с `requestId`, `orderId`, `msg` (`courier assigned`, `courier search timeout`, `payment refunded`, `duplicate event ignored`, …).
* **Журнал SMS**: `GET http://localhost:8084/internal/v1/notifications/logs`.
