# Проверка API Gateway (Nginx) при запуске всей системы в docker-compose:
#   docker compose up -d --build
# Запуск:
#   powershell -ExecutionPolicy Bypass -File scripts\test-gateway.ps1

param(
    [string]$Gw = "http://localhost:8080"
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
        $r = Call GET "$Gw/api/v1/orders/$id"
        if ($r.Json.status -eq $want) { return $r }
        Start-Sleep -Milliseconds 300
    } while ((Get-Date) -lt $deadline)
    return $r
}

# ---------------------------------------------------------------------------
Section "1. Gateway: базовое поведение"
$r = Call GET "$Gw/health"
Check "gateway /health -> 200" ($r.Status -eq 200 -and $r.Json.service -eq "api-gateway") $r
$r = Call GET "$Gw/no/such/route"
Check "неизвестный маршрут -> 404 Problem" (IsProblem $r 404) $r
$r = Call GET "$Gw/internal/v1/orders/123/status"
Check "internal-эндпоинты закрыты снаружи -> 404 Problem" (IsProblem $r 404) $r
$r = Call GET "$Gw/internal/v1/notifications/logs"
Check "журнал уведомлений снаружи недоступен -> 404" (IsProblem $r 404) $r
$r = Call GET "$Gw/health"
Check "gateway сам генерирует X-Request-Id" ($r.RequestId.Length -ge 16) $r
$r = Call GET "$Gw/api/v1/orders" $null @{ "X-Request-Id" = "gw-trace-1" }
Check "переданный X-Request-Id проходит до сервиса и обратно" ($r.Status -eq 200 -and $r.RequestId -eq "gw-trace-1") $r

# ---------------------------------------------------------------------------
Section "2. Маршрутизация в сервисы"
$r = Call POST "$Gw/api/v1/orders" (ToJson @{ originAddress = "A street"; destinationAddress = "B street"; weight = 1 }) @{ "Idempotency-Key" = (NewId) }
Check "POST /api/v1/orders -> order-service (201)" ($r.Status -eq 201) $r
$o = $r.Json
$r = Call GET "$Gw/api/v1/orders/$($o.id)"
Check "GET /api/v1/orders/{id} -> 200" ($r.Status -eq 200 -and $r.Json.id -eq $o.id) $r
$r = Call POST "$Gw/api/v1/orders" (ToJson @{ weight = -1 }) @{ "Idempotency-Key" = (NewId) }
Check "ошибка сервиса проходит через gateway как есть (400 Problem)" (IsProblem $r 400) $r

$courier = NewId
$r = Call POST "$Gw/api/v1/couriers/shifts" (ToJson @{ courierId = $courier; action = "START_SHIFT" })
Check "POST /api/v1/couriers/shifts -> dispatch-service" ($r.Status -eq 200 -and $r.Json.status -eq "ONLINE") $r
$r = Call GET "$Gw/api/v1/couriers/$courier/assignment"
Check "GET /api/v1/couriers/{id}/assignment -> 404 (заказов нет)" (IsProblem $r 404) $r
$r = Call POST "$Gw/api/v1/couriers/shifts" (ToJson @{ courierId = $courier; action = "END_SHIFT" })
Check "курьер закрыл смену" ($r.Status -eq 200) $r

# ---------------------------------------------------------------------------
Section "3. Вебхук банка идёт через gateway"
$r = Call GET $o.paymentUrl
Check "банк-заглушка отправил вебхук через gateway (ответ 200)" ($r.Status -eq 200 -and $r.Text -like "*SUCCESS*(*200)*") $r
$r = WaitStatus $o.id "PAID"
Check "bank -> gateway -> payment -> Kafka -> order: заказ PAID" ($r.Json.status -eq "PAID") $r

# ---------------------------------------------------------------------------
Section "4. Rate limiting (20 запросов/с, всплеск 40)"
# HttpClient заметно быстрее Invoke-WebRequest — нужен настоящий «всплеск» запросов
Add-Type -AssemblyName System.Net.Http
$hc = New-Object System.Net.Http.HttpClient
$limited = 0
for ($i = 0; $i -lt 150; $i++) {
    $resp = $hc.GetAsync("$Gw/api/v1/orders?page=1&size=1").Result
    if ([int]$resp.StatusCode -eq 429) { $limited++ }
    $resp.Dispose()
}
$hc.Dispose()
Check "из 150 быстрых запросов часть отклонена с 429 (отклонено: $limited)" ($limited -gt 0) $null
$r = Call GET "$Gw/api/v1/orders"
if ($r.Status -eq 429) {
    Check "ответ 429 в формате Problem Details" (IsProblem $r 429) $r
}
Start-Sleep -Seconds 3
$r = Call GET "$Gw/api/v1/orders"
Check "через пару секунд лимит восстанавливается (200)" ($r.Status -eq 200) $r

# ---------------------------------------------------------------------------
Write-Host ""
$color = "Green"
if ($script:failed -gt 0) { $color = "Red" }
Write-Host "Итог: прошло $($script:passed), упало $($script:failed)" -ForegroundColor $color
Write-Host "Логи gateway: docker logs gateway"
