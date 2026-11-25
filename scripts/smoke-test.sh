#!/usr/bin/env bash
# Smoke-тест развёрнутой системы (используется на CD-этапе пайплайна, ЛР5).
# Проверяет через API Gateway успешный сценарий и сагу со сбоем (курьер не найден).
#
#   bash scripts/smoke-test.sh [GATEWAY] [PAYMENT] [NOTIFICATION] [DISPATCH]
#
# Нужны curl и jq. Возвращает код 1 при первой же неудачной проверке.
set -euo pipefail

GW=${1:-http://localhost:8080}
PAY=${2:-http://localhost:8083}
NOTIFY=${3:-http://localhost:8084}
DISPATCH=${4:-http://localhost:8082}

PASSED=0
CODE=""
BODY=""
pass() { echo "  [PASS] $1"; PASSED=$((PASSED + 1)); }
fail() { echo "  [FAIL] $1"; [ -n "$BODY" ] && echo "         ответ: $CODE $BODY"; exit 1; }
section() { echo; echo "== $1"; }
uuid() { cat /proc/sys/kernel/random/uuid; }

# req METHOD URL BODY [HEADER...]  → CODE, BODY
req() {
  local method=$1 url=$2 body=$3
  shift 3
  local args=(-s -o /tmp/smoke-resp -w '%{http_code}' -X "$method" "$url" -H 'Content-Type: application/json')
  local h
  for h in "$@"; do args+=(-H "$h"); done
  if [ -n "$body" ]; then args+=(-d "$body"); fi
  CODE=$(curl "${args[@]}" || true)
  BODY=$(cat /tmp/smoke-resp 2>/dev/null || true)
}
field() { jq -r "$1" <<<"$BODY" 2>/dev/null || true; }

# wait_order ID STATUS SECONDS — события асинхронные, поэтому ждём с опросом
wait_order() {
  local id=$1 want=$2 secs=$3 i
  for ((i = 0; i < secs * 2; i++)); do
    req GET "$GW/api/v1/orders/$id" ""
    [ "$(field .status)" = "$want" ] && return 0
    sleep 0.5
  done
  return 1
}

# wait_sms ORDER_ID COUNT SECONDS
wait_sms() {
  local id=$1 want=$2 secs=$3 i
  for ((i = 0; i < secs * 2; i++)); do
    req GET "$NOTIFY/internal/v1/notifications/logs?orderId=$id" ""
    [ "$(field .total)" = "$want" ] && return 0
    sleep 0.5
  done
  return 1
}

ORDER_BODY='{"originAddress":"pl. Gagarina, 1","destinationAddress":"ul. Bolshaya Sadovaya, 105","weight":2.5}'

# ------------------------------------------------------------------ готовность
section "0. Ожидание готовности системы"
ready=0
for _ in $(seq 1 90); do
  if curl -sf "$GW/api/v1/orders" >/dev/null 2>&1 && curl -sf "$PAY/health" >/dev/null 2>&1 \
    && curl -sf "$NOTIFY/health" >/dev/null 2>&1 && curl -sf "$DISPATCH/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 2
done
[ "$ready" = 1 ] && pass "gateway и все сервисы отвечают" || fail "система не поднялась за 3 минуты"

# ------------------------------------------------------------------ контракт и gateway
section "1. Gateway и ошибки"
req GET "$GW/internal/v1/orders/x/status" ""
[ "$CODE" = 404 ] && [ "$(field .status)" = 404 ] && pass "internal-эндпоинты закрыты снаружи (404)" || fail "internal доступен через gateway"
req POST "$GW/api/v1/orders" "$ORDER_BODY"
[ "$CODE" = 400 ] && pass "без Idempotency-Key -> 400 Problem Details" || fail "ожидался 400"
req POST "$GW/api/v1/orders" '{"originAddress":"A","destinationAddress":"B","weight":-1}' "Idempotency-Key: $(uuid)"
[ "$CODE" = 400 ] && [ -n "$(field .detail)" ] && pass "отрицательный вес -> 400 с detail" || fail "ожидался 400"

# ------------------------------------------------------------------ успешный сценарий
section "2. Успешный сценарий: заказ -> оплата -> курьер -> доставка"
COURIER=$(uuid)
req POST "$GW/api/v1/couriers/shifts" "{\"courierId\":\"$COURIER\",\"action\":\"START_SHIFT\"}"
[ "$CODE" = 200 ] && pass "курьер на смене" || fail "START_SHIFT"
req POST "$GW/api/v1/dispatch/location" "{\"courierId\":\"$COURIER\",\"lat\":47.2357,\"lon\":39.7015,\"status\":\"ONLINE\"}"
[ "$CODE" = 200 ] && pass "координаты курьера" || fail "location"

KEY=$(uuid)
req POST "$GW/api/v1/orders" "$ORDER_BODY" "Idempotency-Key: $KEY" "X-Client-Id: smoke"
[ "$CODE" = 201 ] && [ "$(field .status)" = PENDING_PAYMENT ] && pass "заказ создан (order -> payment, REST)" || fail "создание заказа"
ORDER=$(field .id)
PAY_URL=$(field .paymentUrl)
req POST "$GW/api/v1/orders" "$ORDER_BODY" "Idempotency-Key: $KEY" "X-Client-Id: smoke"
[ "$CODE" = 201 ] && [ "$(field .id)" = "$ORDER" ] && pass "повтор с тем же Idempotency-Key -> тот же заказ" || fail "идемпотентность"

PAID_PAGE=$(curl -sf "$PAY_URL" || true)
[[ "$PAID_PAGE" == *SUCCESS* ]] && pass "оплата в банке-заглушке, вебхук через gateway" || fail "оплата"
wait_order "$ORDER" ASSIGNED 30 && pass "payment.completed -> dispatch -> courier_assigned: заказ ASSIGNED" || fail "заказ не стал ASSIGNED"
[ "$(field .courierId)" = "$COURIER" ] && pass "назначен наш курьер" || fail "назначен другой курьер"

req POST "$GW/api/v1/couriers/$COURIER/assignment/complete" ""
[ "$CODE" = 200 ] && pass "курьер отметил доставку (dispatch -> order, REST)" || fail "complete"
wait_order "$ORDER" DELIVERED 10 && pass "заказ DELIVERED" || fail "заказ не стал DELIVERED"
wait_sms "$ORDER" 2 15 && pass "клиенту ушли 2 SMS" || fail "нет SMS"

req POST "$GW/api/v1/couriers/shifts" "{\"courierId\":\"$COURIER\",\"action\":\"END_SHIFT\"}"
[ "$CODE" = 200 ] && pass "курьер закрыл смену" || fail "END_SHIFT"

# ------------------------------------------------------------------ сага со сбоем
section "3. Сага: курьер не найден -> отмена и возврат денег"
req POST "$GW/api/v1/orders" "$ORDER_BODY" "Idempotency-Key: $(uuid)"
[ "$CODE" = 201 ] || fail "создание заказа"
ORDER2=$(field .id)
PAYMENT2=$(field .paymentId)
curl -sf "$(field .paymentUrl)" >/dev/null || fail "оплата"
wait_order "$ORDER2" PAID 30 && pass "заказ оплачен, идёт поиск курьера" || fail "заказ не стал PAID"
wait_order "$ORDER2" CANCELLED 60 && pass "dispatch.search_failed -> заказ CANCELLED" || fail "заказ не отменён"
[ "$(field .cancelReason)" = COURIER_NOT_FOUND ] && pass "причина COURIER_NOT_FOUND" || fail "неверная причина"
refunded=0
for _ in $(seq 1 40); do
  req GET "$PAY/internal/v1/payments/$PAYMENT2" ""
  [ "$(field .status)" = REFUNDED ] && refunded=1 && break
  sleep 0.5
done
[ "$refunded" = 1 ] && pass "компенсация: платёж REFUNDED" || fail "деньги не возвращены"
wait_sms "$ORDER2" 1 15 && pass "SMS об отмене" || fail "нет SMS об отмене"

echo
echo "Итог: прошло $PASSED проверок, упало 0"
