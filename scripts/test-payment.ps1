# Проверка payment-service и bank-mock "чёрным ящиком" по HTTP.
# Перед запуском должны работать:
#   go run ./mocks/bank-mock
#   go run ./services/payment-service
# Запуск:
#   powershell -ExecutionPolicy Bypass -File scripts\test-payment.ps1

param(
    [string]$Pay = "http://localhost:8083",
    [string]$Bank = "http://localhost:9090",
    [string]$Secret = "dev-secret"
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
$r = Call GET "$Pay/health"
Check "payment-service отвечает на /health (200, UP)" ($r.Status -eq 200 -and $r.Json.status -eq "UP") $r
$r = Call GET "$Bank/pay/unknown"
Check "bank-mock отвечает (404 на неизвестный платёж)" ($r.Status -eq 404) $r

# ---------------------------------------------------------------------------
Section "1. Создание платежа: валидация и единая модель ошибок"
$order = NewId
$r = Call POST "$Pay/internal/v1/payments" (ToJson @{ orderId = $order; amount = 450 })
Check "без Idempotency-Key -> 400 application/problem+json" (IsProblem $r 400) $r
Check "  поля Problem: type, title, status, detail, instance" ($r.Json.type -and $r.Json.title -and $r.Json.detail -and $r.Json.instance) $r

$r = Call POST "$Pay/internal/v1/payments" (ToJson @{ orderId = $order; amount = -5 }) @{ "Idempotency-Key" = (NewId) }
Check "отрицательная сумма -> 400" (IsProblem $r 400) $r

$r = Call POST "$Pay/internal/v1/payments" (ToJson @{ amount = 100 }) @{ "Idempotency-Key" = (NewId) }
Check "без orderId -> 400" (IsProblem $r 400) $r

$r = Call POST "$Pay/internal/v1/payments" '{"orderId":"x","amount":1,"hack":true}' @{ "Idempotency-Key" = (NewId) }
Check "лишнее поле в JSON -> 400" (IsProblem $r 400) $r

$r = Call POST "$Pay/internal/v1/payments" '{"orderId":"x","amount":' @{ "Idempotency-Key" = (NewId) }
Check "битый JSON -> 400" (IsProblem $r 400) $r

# ---------------------------------------------------------------------------
Section "2. Создание платежа и Idempotency-Key"
$key = NewId
$body = ToJson @{ orderId = $order; amount = 450 }
$r1 = Call POST "$Pay/internal/v1/payments" $body @{ "Idempotency-Key" = $key }
Check "создание -> 201" ($r1.Status -eq 201) $r1
Check "  статус PENDING" ($r1.Json.status -eq "PENDING") $r1
Check "  paymentUrl ведёт в bank-mock" ($r1.Json.paymentUrl -like "$Bank/pay/*") $r1
$paymentId = $r1.Json.paymentId
Write-Host "         paymentId = $paymentId"

$r2 = Call POST "$Pay/internal/v1/payments" $body @{ "Idempotency-Key" = $key }
Check "повтор с тем же ключом -> тот же ответ (201, тот же paymentId)" ($r2.Status -eq 201 -and $r2.Json.paymentId -eq $paymentId) $r2
Check "  заголовок Idempotent-Replayed: true" ($r2.Replayed -eq "true") $r2

$r = Call POST "$Pay/internal/v1/payments" (ToJson @{ orderId = $order; amount = 999 }) @{ "Idempotency-Key" = $key }
Check "тот же ключ, другое тело -> 409" (IsProblem $r 409) $r

$r = Call POST "$Pay/internal/v1/payments" $body @{ "Idempotency-Key" = (NewId) }
Check "новый ключ, тот же заказ -> 200 и существующий платёж" ($r.Status -eq 200 -and $r.Json.paymentId -eq $paymentId) $r

# ---------------------------------------------------------------------------
Section "3. Возврат до оплаты запрещён"
$r = Call POST "$Pay/internal/v1/payments/$paymentId/refund" (ToJson @{ reason = "TEST" })
Check "refund для PENDING -> 409" (IsProblem $r 409) $r

# ---------------------------------------------------------------------------
Section "4. Вебхук: защита"
$r = Call POST "$Pay/api/v1/payments/webhook" (ToJson @{ paymentId = $paymentId; status = "SUCCESS"; signature = "fake" })
Check "поддельная подпись -> 400 invalid-signature" ((IsProblem $r 400) -and ($r.Json.type -like "*invalid-signature")) $r

$r = Call POST "$Pay/api/v1/payments/webhook" (ToJson @{ paymentId = $paymentId; status = "MAYBE"; signature = "x" })
Check "неизвестный статус -> 400" (IsProblem $r 400) $r

$fake = NewId
$r = Call POST "$Pay/api/v1/payments/webhook" (ToJson @{ paymentId = $fake; status = "SUCCESS"; signature = (Sign $fake "SUCCESS") })
Check "верная подпись, но платежа нет -> 404" (IsProblem $r 404) $r

# ---------------------------------------------------------------------------
Section "5. Успешная оплата через банк-заглушку"
$r = Call GET $r1.Json.paymentUrl
Check "банк принял оплату и отправил вебхук (200)" ($r.Status -eq 200 -and $r.Text -like "*SUCCESS*(*200)*") $r

$r = Call POST "$Pay/api/v1/payments/webhook" (ToJson @{ paymentId = $paymentId; status = "SUCCESS"; signature = (Sign $paymentId "SUCCESS") })
Check "повторный вебхук -> 200, игнорируется (второго события нет, см. лог)" ($r.Status -eq 200 -and $r.Json.received -eq $true) $r

$r = Call POST "$Pay/api/v1/payments/webhook" (ToJson @{ paymentId = $paymentId; status = "FAILED"; signature = (Sign $paymentId "FAILED") })
Check "поздний FAILED после SUCCESS не меняет статус (200)" ($r.Status -eq 200) $r

# ---------------------------------------------------------------------------
Section "6. Возврат (компенсирующее действие)"
$r = Call POST "$Pay/internal/v1/payments/$paymentId/refund" (ToJson @{ reason = "" })
Check "пустой reason -> 400" (IsProblem $r 400) $r

$r = Call POST "$Pay/internal/v1/payments/$paymentId/refund" (ToJson @{ reason = "COURIER_NOT_FOUND_TIMEOUT" })
Check "возврат -> 200 REFUNDED" ($r.Status -eq 200 -and $r.Json.status -eq "REFUNDED" -and $r.Json.refundId) $r
$refundId = $r.Json.refundId

$r = Call POST "$Pay/internal/v1/payments/$paymentId/refund" (ToJson @{ reason = "COURIER_NOT_FOUND_TIMEOUT" })
Check "повторный возврат -> 200, тот же refundId (деньги не возвращаются дважды)" ($r.Status -eq 200 -and $r.Json.refundId -eq $refundId) $r

$r = Call POST "$Pay/internal/v1/payments/$(NewId)/refund" (ToJson @{ reason = "TEST" })
Check "возврат несуществующего платежа -> 404" (IsProblem $r 404) $r

# ---------------------------------------------------------------------------
Section "7. Отказ банка"
$order2 = NewId
$r = Call POST "$Pay/internal/v1/payments" (ToJson @{ orderId = $order2; amount = 300 }) @{ "Idempotency-Key" = (NewId) }
$p2 = $r.Json.paymentId
$r = Call GET ($r.Json.paymentUrl + "?result=fail")
Check "банк отклонил оплату (вебхук FAILED принят)" ($r.Status -eq 200 -and $r.Text -like "*FAILED*(*200)*") $r

$r = Call POST "$Pay/internal/v1/payments/$p2/refund" (ToJson @{ reason = "TEST" })
Check "возврат отклонённого платежа -> 409" (IsProblem $r 409) $r

$r = Call POST "$Pay/internal/v1/payments" (ToJson @{ orderId = $order2; amount = 300 }) @{ "Idempotency-Key" = (NewId) }
Check "после отказа можно создать новый платёж для заказа (201, новый id)" ($r.Status -eq 201 -and $r.Json.paymentId -ne $p2) $r

# ---------------------------------------------------------------------------
Section "8. Сквозной X-Request-Id"
$r = Call GET "$Pay/health" $null @{ "X-Request-Id" = "test-request-42" }
Check "переданный X-Request-Id возвращается в ответе" ($r.RequestId -eq "test-request-42") $r
$r = Call GET "$Pay/health"
Check "без заголовка сервис генерирует свой" ($r.RequestId.Length -eq 36) $r

# ---------------------------------------------------------------------------
Write-Host ""
$color = "Green"
if ($script:failed -gt 0) { $color = "Red" }
Write-Host "Итог: прошло $($script:passed), упало $($script:failed)" -ForegroundColor $color
Write-Host "Теперь загляни в лог payment-service: там должны быть события"
Write-Host "payment.completed (1 шт.), payment.refunded (1 шт.), payment.failed (1 шт.)."
