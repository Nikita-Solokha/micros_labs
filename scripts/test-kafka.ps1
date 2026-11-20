# Проверка асинхронного взаимодействия через Kafka.
# Должны работать: docker compose up -d (Kafka), а также в окнах с $env:KAFKA_BROKERS="localhost:9092":
#   go run ./mocks/bank-mock
#   go run ./services/payment-service
#   go run ./services/order-service
# Запуск:
#   powershell -ExecutionPolicy Bypass -File scripts\test-kafka.ps1

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


# Ждёт, пока заказ перейдёт в нужный статус (события асинхронные).
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
    $r = Call POST "$Order/api/v1/orders" (ToJson @{ originAddress = "A"; destinationAddress = "B"; weight = 1 }) @{ "Idempotency-Key" = (NewId) }
    return $r
}

# ---------------------------------------------------------------------------
Section "1. payment.completed -> заказ PAID (payment -> Kafka -> order)"
$r = NewOrder
Check "заказ создан (201, PENDING_PAYMENT)" ($r.Status -eq 201 -and $r.Json.status -eq "PENDING_PAYMENT") $r
$o1 = $r.Json
$r = Call GET $o1.paymentUrl
Check "банк принял оплату" ($r.Status -eq 200 -and $r.Text -like "*SUCCESS*") $r
$t0 = Get-Date
$r = WaitStatus $o1.id "PAID"
$ms = [int]((Get-Date) - $t0).TotalMilliseconds
Check "заказ стал PAID через Kafka (за $ms мс)" ($r.Json.status -eq "PAID") $r

# ---------------------------------------------------------------------------
Section "2. payment.failed -> заказ CANCELLED (компенсация)"
$r = NewOrder
$o2 = $r.Json
$r = Call GET ($o2.paymentUrl + "?result=fail")
Check "банк отклонил оплату" ($r.Status -eq 200 -and $r.Text -like "*FAILED*") $r
$r = WaitStatus $o2.id "CANCELLED"
Check "заказ стал CANCELLED" ($r.Json.status -eq "CANCELLED") $r
Check "  причина PAYMENT_FAILED" ($r.Json.cancelReason -eq "PAYMENT_FAILED") $r

# ---------------------------------------------------------------------------
Section "3. Повторный вебхук не ломает статус"
$r = Call GET $o1.paymentUrl
Start-Sleep -Seconds 2
$r = Call GET "$Order/api/v1/orders/$($o1.id)"
Check "заказ остался PAID" ($r.Json.status -eq "PAID") $r

# ---------------------------------------------------------------------------
Write-Host ""
$color = "Green"
if ($script:failed -gt 0) { $color = "Red" }
Write-Host "Итог: прошло $($script:passed), упало $($script:failed)" -ForegroundColor $color
Write-Host "Открой http://localhost:8090 -> Topics -> payments.events -> Messages: там видны события."
