# Проверка order-service по HTTP (вместе с настоящим payment-service и bank-mock).
# Перед запуском должны работать три сервиса:
#   go run ./mocks/bank-mock
#   go run ./services/payment-service
#   go run ./services/order-service
# Запуск:
#   powershell -ExecutionPolicy Bypass -File scripts\test-order.ps1

param(
    [string]$Order = "http://localhost:8081",
    [string]$Bank = "http://localhost:9090"
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


# ---------------------------------------------------------------------------
Section "0. Сервисы запущены"
$r = Call GET "$Order/health"
Check "order-service отвечает на /health" ($r.Status -eq 200) $r

$client = "client-" + (NewId).Substring(0, 8)
$H = @{ "X-Client-Id" = $client }
function Hk { return @{ "X-Client-Id" = $client; "Idempotency-Key" = (NewId) } }
$valid = ToJson @{ originAddress = "pl. Gagarina, 1"; destinationAddress = "ul. Bolshaya Sadovaya, 105"; weight = 2.5 }

# ---------------------------------------------------------------------------
Section "1. Валидация (400 + Problem Details)"
$r = Call POST "$Order/api/v1/orders" $valid $H
Check "без Idempotency-Key -> 400" (IsProblem $r 400) $r
$r = Call POST "$Order/api/v1/orders" (ToJson @{ originAddress = "A"; destinationAddress = "B"; weight = 0 }) (Hk)
Check "вес 0 -> 400" (IsProblem $r 400) $r
$r = Call POST "$Order/api/v1/orders" (ToJson @{ originAddress = "A"; destinationAddress = "B"; weight = 51 }) (Hk)
Check "вес 51 кг -> 400" (IsProblem $r 400) $r
$r = Call POST "$Order/api/v1/orders" (ToJson @{ originAddress = "A"; destinationAddress = "A"; weight = 1 }) (Hk)
Check "одинаковые адреса -> 400" (IsProblem $r 400) $r
$r = Call POST "$Order/api/v1/orders" (ToJson @{ destinationAddress = "B"; weight = 1 }) (Hk)
Check "нет originAddress -> 400" (IsProblem $r 400) $r

# ---------------------------------------------------------------------------
Section "2. Создание заказа = синхронный вызов order -> payment"
$key = NewId
$hdr = @{ "X-Client-Id" = $client; "Idempotency-Key" = $key }
$r1 = Call POST "$Order/api/v1/orders" $valid $hdr
Check "создание -> 201" ($r1.Status -eq 201) $r1
Check "  статус PENDING_PAYMENT" ($r1.Json.status -eq "PENDING_PAYMENT") $r1
Check "  цена 200 + 100*2.5 = 450" ($r1.Json.price -eq 450) $r1
Check "  paymentId получен от payment-service" ([bool]$r1.Json.paymentId) $r1
Check "  paymentUrl ведёт в bank-mock" ($r1.Json.paymentUrl -like "$Bank/pay/*") $r1
$orderId = $r1.Json.id
Write-Host "         orderId = $orderId"

$r2 = Call POST "$Order/api/v1/orders" $valid $hdr
Check "повтор с тем же ключом -> тот же заказ, без нового платежа" ($r2.Status -eq 201 -and $r2.Json.id -eq $orderId -and $r2.Json.paymentId -eq $r1.Json.paymentId) $r2
Check "  заголовок Idempotent-Replayed: true" ($r2.Replayed -eq "true") $r2
$r = Call POST "$Order/api/v1/orders" (ToJson @{ originAddress = "X"; destinationAddress = "Y"; weight = 1 }) $hdr
Check "тот же ключ, другое тело -> 409" (IsProblem $r 409) $r

# ---------------------------------------------------------------------------
Section "3. Чтение заказа"
$r = Call GET "$Order/api/v1/orders/$orderId"
Check "GET /orders/{id} -> 200" ($r.Status -eq 200 -and $r.Json.id -eq $orderId) $r
$r = Call GET "$Order/api/v1/orders/$(NewId)"
Check "несуществующий заказ -> 404" (IsProblem $r 404) $r
$r = Call GET "$Order/internal/v1/orders/$orderId/status"
Check "internal: статус заказа -> 200 PENDING_PAYMENT" ($r.Status -eq 200 -and $r.Json.status -eq "PENDING_PAYMENT") $r

# ---------------------------------------------------------------------------
Section "4. Список заказов с пагинацией"
for ($i = 0; $i -lt 4; $i++) { $null = Call POST "$Order/api/v1/orders" $valid (Hk) }
$r = Call GET "$Order/api/v1/orders?page=1&size=2" $null $H
Check "page=1,size=2 -> 2 элемента из 5" ($r.Status -eq 200 -and $r.Json.total -eq 5 -and @($r.Json.items).Count -eq 2) $r
$r = Call GET "$Order/api/v1/orders?page=3&size=2" $null $H
Check "page=3,size=2 -> 1 элемент" (@($r.Json.items).Count -eq 1) $r
$r = Call GET "$Order/api/v1/orders?page=1&size=20" $null @{ "X-Client-Id" = "other-client" }
Check "чужой клиент не видит эти заказы" ($r.Json.total -eq 0) $r
$r = Call GET "$Order/api/v1/orders?page=0" $null $H
Check "page=0 -> 400" (IsProblem $r 400) $r
$r = Call GET "$Order/api/v1/orders?size=500" $null $H
Check "size=500 -> 400" (IsProblem $r 400) $r
$r = Call GET "$Order/api/v1/orders?page=abc" $null $H
Check "page=abc -> 400" (IsProblem $r 400) $r

# ---------------------------------------------------------------------------
Section "5. Доставка (internal, вызывает dispatch-service)"
$r = Call POST "$Order/internal/v1/orders/$orderId/delivered" (ToJson @{ courierId = "c1" })
Check "доставить неоплаченный заказ -> 409" (IsProblem $r 409) $r
$r = Call POST "$Order/internal/v1/orders/$orderId/delivered" (ToJson @{ courierId = "" })
Check "без courierId -> 400" (IsProblem $r 400) $r
$r = Call POST "$Order/internal/v1/orders/$(NewId)/delivered" (ToJson @{ courierId = "c1" })
Check "несуществующий заказ -> 404" (IsProblem $r 404) $r

# ---------------------------------------------------------------------------
Section "6. Сквозной X-Request-Id (order -> payment)"
$r = Call POST "$Order/api/v1/orders" $valid @{ "Idempotency-Key" = (NewId); "X-Request-Id" = "trace-order-1" }
Check "запрос с X-Request-Id: trace-order-1 -> 201" ($r.Status -eq 201) $r
Write-Host "         найди trace-order-1 в логах ОБОИХ сервисов: order-service и payment-service"

# ---------------------------------------------------------------------------
Write-Host ""
$color = "Green"
if ($script:failed -gt 0) { $color = "Red" }
Write-Host "Итог: прошло $($script:passed), упало $($script:failed)" -ForegroundColor $color
