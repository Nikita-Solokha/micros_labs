// Package webhook — подпись вебхуков платёжного шлюза (HMAC-SHA256).
// Шлюз подписывает уведомление общим секретом, payment-service проверяет подпись,
// чтобы никто посторонний не мог «подтвердить» оплату.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Sign считает подпись для пары paymentId + status.
func Sign(secret, paymentID, status string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(paymentID + ":" + status))
	return hex.EncodeToString(m.Sum(nil))
}

// Verify сравнивает подпись за постоянное время (защита от timing-атак).
func Verify(secret, paymentID, status, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, paymentID, status)), []byte(signature))
}
