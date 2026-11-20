# Живая проверка всей саги через Kafka.
# Должны работать: docker compose up -d (Kafka) и пять окон, в окнах 2-4 сначала $env:KAFKA_BROKERS="localhost:9092":
#   1: go run ./mocks/bank-mock
#   2: go run ./services/payment-service
#   3: go run ./services/order-service
#   4: $env:SEARCH_TIMEOUT="20s"; go run ./services/dispatch-service
# Запуск (окно 5):
#   powershell -ExecutionPolicy Bypass -File scripts\test-saga.ps1

param(
    [string]$Order = "http://localhost:8081",
    [string]$Dispatch = "http://localhost:8082",
    [string]$Pay = "http://localhost:8083",
    [int]$SearchTimeoutSec = 20
)

$ErrorActionPreference = "Stop"
$script:passed = 0
$script:failed = 0

function Parse-Json([string]$text) {
    try { return $text | ConvertFrom-Json } catch { return $null }
}

# HTTP-запрос, который НЕ падает на 4xx/5xx, а возвращает код, тип и тело ответа.
function Call {
    param([string]$Method, [string]$Url, [string]$Body = $null, [hashtable]$Headers = @{})
    $p = @{ Method = $Method; Uri = $Url; Headers = $Headers; UseBasicParsing = $true }
    if ($Body) {
        $p.ContentType = "application/json"
        $p.Body = [Text.Encoding]::UTF8.GetBytes($Body)
    }
    try {
        $r = Invoke-WebRequest @p
        $text = [Text.Encoding]::UTF8.GetString($r.RawContentStream.ToArray())
        return [pscustomobject]@{
            Status      = [int]$r.StatusCode
            ContentType = "$($r.Headers['Content-Type'])"
            Replayed    = "$($r.Headers['Idempotent-Replayed'])"
            RequestId   = "$($r.Headers['X-Request-Id'])"
            Text        = $text
            Json        = Parse-Json $text
        }
    } catch {
        $resp = $_.Exception.Response
        if ($null -eq $resp) { throw "Сервис недоступен: $Url ($($_.Exception.Message))" }
        $text = ""
        $ct = ""
        if ($resp.PSObject.Methods['GetResponseStream']) {
            # Windows PowerShell 5.1
            $ct = $resp.ContentType
            try {
                $sr = New-Object IO.StreamReader($resp.GetResponseStream(), [Text.Encoding]::UTF8)
                try { $sr.BaseStream.Position = 0; $sr.DiscardBufferedData() } catch {}
                $text = $sr.ReadToEnd()
            } catch {}
        } else {
            # PowerShell 7
            $ct = "$($resp.Content.Headers.ContentType)"
        }
        if (-not $text -and $_.ErrorDetails) { $text = $_.ErrorDetails.Message }
        return [pscustomobject]@{
            Status = [int]$resp.StatusCode; ContentType = $ct; Replayed = ""; RequestId = ""
            Text = $text; Json = Parse-Json $text
        }
    }
}

function Check([string]$name, [bool]$ok, $resp = $null) {
    if ($ok) {
        Write-Host "  [PASS] $name" -ForegroundColor Green
        $script:passed++
    } else {
        Write-Host "  [FAIL] $name" -ForegroundColor Red
        if ($resp) { Write-Host "         ответ: $($resp.Status) $($resp.Text)" -ForegroundColor DarkYellow }
        $script:failed++
    }
}

function Section([string]$title) { Write-Host ""; Write-Host "=== $title ===" -ForegroundColor Cyan }

function Sign([string]$id, [string]$status) {
    $h = New-Object System.Security.Cryptography.HMACSHA256
    $h.Key = [Text.Encoding]::UTF8.GetBytes($Secret)
    $bytes = $h.ComputeHash([Text.Encoding]::UTF8.GetBytes("${id}:${status}"))
    return (($bytes | ForEach-Object { $_.ToString("x2") }) -join "")
}

function NewId { return [guid]::NewGuid().ToString() }
function ToJson($obj) { return ($obj | ConvertTo-Json -Compress) }
function IsProblem($r, [int]$code) {
    return ($r.Status -eq $code) -and ($r.ContentType -like "application/problem+json*") -and ($r.Json.status -eq $code)
}


function WaitStatus([string]$id, [string]$want, [int]$seconds = 15) {
    $deadline = (Get-Date).AddSeconds($seconds)
    do {
        $r = Call GET "$Order/api/v1/orders/$id"
        if ($r.Json.status -eq $want) { return $r }
        Start-Sleep -Milliseconds 300
    } while ((Get-Date) -lt $deadline)
    return $r
}

function NewOrder {
    return (Call POST "$Order/api/v1/orders" (ToJson @{ originAddress = "pl. Gagarina, 1"; destinationAddress = "ul. Bolshaya Sadovaya, 105"; weight = 2.5 }) @{ "Idempotency-Key" = (NewId) })
}

# Центр Ростова: все адреса мок-геокодера в радиусе ~6 км отсюда.
$lat = 47.2357
$lon = 39.7015

Section "0. Сервисы запущены"
foreach ($u in @($Order, $Dispatch, $Pay)) {
    $r = Call GET "$u/health"
    Check "$u/health -> 200" ($r.Status -eq 200) $r
}

# ---------------------------------------------------------------------------
Section "1. Успешный сценарий: заказ -> оплата -> курьер -> доставка"
$courier = NewId
$r = Call POST "$Dispatch/api/v1/couriers/shifts" (ToJson @{ courierId = $courier; action = "START_SHIFT" })
Check "курьер вышел на смену (ONLINE)" ($r.Status -eq 200 -and $r.Json.status -eq "ONLINE") $r
$r = Call POST "$Dispatch/api/v1/dispatch/location" (ToJson @{ courierId = $courier; lat = $lat; lon = $lon; status = "ONLINE" })
Check "курьер передал координаты" ($r.Status -eq 200 -and $r.Json.success -eq $true) $r

$r = NewOrder
Check "заказ создан (PENDING_PAYMENT)" ($r.Status -eq 201) $r
$o1 = $r.Json
$r = Call GET $o1.paymentUrl
Check "клиент оплатил в банке" ($r.Status -eq 200 -and $r.Text -like "*SUCCESS*") $r

$r = WaitStatus $o1.id "ASSIGNED"
Check "payment.completed -> dispatch нашёл курьера -> заказ ASSIGNED" ($r.Json.status -eq "ASSIGNED") $r
Check "  назначен наш курьер" ($r.Json.courierId -eq $courier) $r

$r = Call GET "$Dispatch/api/v1/couriers/$courier/assignment"
Check "курьер видит своё назначение" ($r.Status -eq 200 -and $r.Json.orderId -eq $o1.id) $r

$r = Call POST "$Dispatch/api/v1/couriers/shifts" (ToJson @{ courierId = $courier; action = "END_SHIFT" })
Check "нельзя закрыть смену с активным заказом -> 409" (IsProblem $r 409) $r

$r = Call POST "$Dispatch/api/v1/couriers/$courier/assignment/complete"
Check "курьер отметил доставку (dispatch -> order, sync)" ($r.Status -eq 200) $r
$r = WaitStatus $o1.id "DELIVERED" 5
Check "заказ DELIVERED" ($r.Json.status -eq "DELIVERED") $r

$r = Call POST "$Dispatch/api/v1/couriers/shifts" (ToJson @{ courierId = $courier; action = "END_SHIFT" })
Check "курьер закрыл смену (OFFLINE)" ($r.Status -eq 200 -and $r.Json.status -eq "OFFLINE") $r

# ---------------------------------------------------------------------------
Section "2. Сбой: курьер не найден -> компенсации (отмена + возврат)"
Write-Host "         курьеров на смене нет; ждём таймаут поиска ($SearchTimeoutSec с)..."
$r = NewOrder
$o2 = $r.Json
$null = Call GET $o2.paymentUrl
$r = WaitStatus $o2.id "PAID"
Check "заказ оплачен (PAID), идёт поиск курьера" ($r.Json.status -eq "PAID") $r

$r = WaitStatus $o2.id "CANCELLED" ($SearchTimeoutSec + 15)
Check "dispatch.search_failed -> заказ CANCELLED" ($r.Json.status -eq "CANCELLED") $r
Check "  причина COURIER_NOT_FOUND" ($r.Json.cancelReason -eq "COURIER_NOT_FOUND") $r

$deadline = (Get-Date).AddSeconds(10)
do {
    $p = Call GET "$Pay/internal/v1/payments/$($o2.paymentId)"
    if ($p.Json.status -eq "REFUNDED") { break }
    Start-Sleep -Milliseconds 300
} while ((Get-Date) -lt $deadline)
Check "dispatch.search_failed -> payment вернул деньги (REFUNDED)" ($p.Json.status -eq "REFUNDED") $p

# ---------------------------------------------------------------------------
Write-Host ""
$color = "Green"
if ($script:failed -gt 0) { $color = "Red" }
Write-Host "Итог: прошло $($script:passed), упало $($script:failed)" -ForegroundColor $color
Write-Host "Kafka UI: http://localhost:8090 -> Topics -> dispatch.events -> Messages"
