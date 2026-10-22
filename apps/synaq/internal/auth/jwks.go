package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
)

// newHTTPFetcher — загрузка JWKS: таймаут 3 с, без редиректов, Host = jwksHost (если задан).
func newHTTPFetcher(url, host string) JWKSFetcher {
	client := &http.Client{
		Timeout: jwksFetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if host != "" {
			req.Host = host
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		// Как httpx raise_for_status: всё, кроме 2xx (включая редиректы), — ошибка.
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("JWKS: HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBodyBytes+1))
		if err != nil {
			return nil, err
		}
		if len(body) > maxJWKSBodyBytes {
			return nil, errors.New("JWKS: слишком большой ответ")
		}
		return body, nil
	}
}

// b64uint декодирует base64url (паддинг допускается) в big-endian целое.
func b64uint(s string) (*big.Int, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, false
	}
	return new(big.Int).SetBytes(raw), true
}

// parseRSAKey собирает публичный ключ из n/e с проверками как в cryptography:
// e ≥ 3, e < n, e нечётное.
func parseRSAKey(jwk map[string]any) (*rsa.PublicKey, bool) {
	if _, ok := jwk["oth"]; ok {
		return nil, false
	}
	ns, _ := jwk["n"].(string)
	es, _ := jwk["e"].(string)
	n, ok1 := b64uint(ns)
	e, ok2 := b64uint(es)
	if !ok1 || !ok2 || n.Cmp(big.NewInt(3)) < 0 {
		return nil, false
	}
	if e.Cmp(big.NewInt(3)) < 0 || e.Cmp(n) >= 0 || e.Bit(0) == 0 || !e.IsInt64() || e.Int64() > 1<<31-1 {
		return nil, false
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, true
}

// parseJWKS отбирает только годные RS256-ключи подписи: kty=RSA, use ∈ {нет, sig},
// alg ∈ {нет, RS256}, непустой строковый kid. Негодные ключи молча пропускаются.
func parseJWKS(raw []byte) map[string]*rsa.PublicKey {
	keys := map[string]*rsa.PublicKey{}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return keys
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return keys
	}
	list, ok := obj["keys"].([]any)
	if !ok {
		return keys
	}
	for _, item := range list {
		jwk, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kid, _ := jwk["kid"].(string)
		if kid == "" {
			continue
		}
		if kty, _ := jwk["kty"].(string); kty != "RSA" {
			continue
		}
		if use, present := jwk["use"]; present && use != nil && use != "sig" {
			continue
		}
		if alg, present := jwk["alg"]; present && alg != nil && alg != algRS256 {
			continue
		}
		if key, ok := parseRSAKey(jwk); ok {
			keys[kid] = key
		}
	}
	return keys
}

// refresh — один запрос JWKS; при успехе заменяет кэш ключей. Ошибки не пробрасывает.
// Выполняется в отдельной горутине, не привязанной к контексту запроса: отмена одного
// HTTP-запроса не отменяет общую загрузку ключей (аналог asyncio.shield).
func (v *Verifier) refresh(done chan struct{}) {
	var parsed map[string]*rsa.PublicKey
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Не удалось получить JWKS: panic %T", r)
			parsed = nil
		}
		v.mu.Lock()
		if len(parsed) > 0 {
			v.keys = parsed
			v.fetchedAt = v.now()
		}
		v.inflight = nil
		close(done)
		v.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), jwksFetchTimeout)
	defer cancel()
	raw, err := v.fetch(ctx)
	if err != nil {
		log.Printf("Не удалось получить JWKS: %T", err)
		return
	}
	parsed = parseJWKS(raw)
	if len(parsed) == 0 {
		log.Printf("JWKS не содержит подходящих RS256-ключей")
	}
}

// signingKey — ключ по kid из кэша; при промахе/истечении TTL — перезапрос
// (с ограничением частоты). (nil, nil) — kid неизвестен (→ 401).
//
// Параллельные запросы ждут одну и ту же загрузку — нет ни шторма запросов
// к Zitadel, ни всплеска 503 на холодном старте.
func (v *Verifier) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	now := v.now()
	fresh := len(v.keys) > 0 && now.Sub(v.fetchedAt) < jwksTTL
	if fresh {
		if key, ok := v.keys[kid]; ok {
			v.mu.Unlock()
			return key, nil
		}
	}
	interval := jwksEmptyRetryInterval
	if len(v.keys) > 0 {
		interval = jwksMinRefreshInterval
	}
	allowStart := v.lastAttempt.IsZero() || now.Sub(v.lastAttempt) >= interval
	wait := v.inflight
	if wait == nil && allowStart {
		// Отмечаем попытку до загрузки, чтобы следующие запросы попали под rate limit
		v.lastAttempt = now
		wait = make(chan struct{})
		v.inflight = wait
		go v.refresh(wait)
	}
	v.mu.Unlock()

	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ErrUnavailable
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.keys) == 0 {
		return nil, ErrUnavailable
	}
	if v.now().Sub(v.fetchedAt) >= jwksMaxStale {
		v.warnOnce("jwks_too_stale", "Ключи JWKS устарели более чем на %v — отвечаем 503", jwksMaxStale)
		return nil, ErrUnavailable
	}
	// Если обновить не удалось — используем устаревшие (но не старше суток) ключи, чем отказывать всем
	return v.keys[kid], nil
}
