package auth

// Порт tests/test_auth.py (классы TestRoles, TestMissingOrMalformed, TestClaims,
// TestJwks, TestUnavailable, TestConfigFile, TestVerifyTokenUnit) + проверки,
// специфичные для Go-реализации. Все ключи, токены и данные синтетические.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ════════════════ 1. Роли (TestRoles) ════════════════

func TestRoles(t *testing.T) {
	t.Run("admin_can_create", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.post(e.admin()), http.StatusCreated)
		assertFetches(t, e.srv, 1)
	})
	t.Run("admin_can_list", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
	})
	t.Run("analyst_can_list", func(t *testing.T) {
		e := newEnv(t)
		w := e.get(e.token(withRoles(RoleAnalyst)))
		assertStatus(t, w, http.StatusOK)
		if !strings.Contains(w.Body.String(), `"sub":"test_user_1"`) {
			t.Fatalf("principal не передан в обработчик: %s", w.Body.String())
		}
	})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, experiments},
		{http.MethodDelete, experiments + "test_exp_1"},
		{http.MethodPatch, experiments + "test_exp_1"},
	} {
		t.Run("analyst_forbidden_on_writes/"+tc.method, func(t *testing.T) {
			e := newEnv(t)
			w := e.do(tc.method, tc.path, "Bearer "+e.token(withRoles(RoleAnalyst)))
			assertStatus(t, w, http.StatusForbidden)
			if d := detailOf(t, w); d != detailForbidden {
				t.Fatalf("detail %q", d)
			}
			if h := w.Header().Get("WWW-Authenticate"); h != "" {
				t.Fatalf("WWW-Authenticate не ожидался на 403: %q", h)
			}
		})
	}
	for name, opts := range map[string][]tokenOpt{
		"empty":  {withRoles()},
		"viewer": {withRoles("viewer")},
		"none":   nil,
	} {
		t.Run("no_known_role_forbidden_on_list/"+name, func(t *testing.T) {
			e := newEnv(t)
			assertStatus(t, e.get(e.token(opts...)), http.StatusForbidden)
		})
	}
	t.Run("project_specific_roles_claim", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withClaim(projectRolesClaim(testAudience),
			map[string]any{"admin": map[string]any{"100000000000000002": "test.example.com"}}))
		assertStatus(t, e.post(tok), http.StatusCreated)
	})
	t.Run("other_project_roles_claim_ignored", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withClaim(projectRolesClaim("999999999999999999"), map[string]any{"admin": map[string]any{}}))
		assertStatus(t, e.get(tok), http.StatusForbidden)
	})
	t.Run("roles_claim_not_a_dict_ignored", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.token(withClaim(rolesClaim, []any{"admin"}))), http.StatusForbidden)
	})
	t.Run("generic_roles_claim_alone_forbidden", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withClaim(genericRolesClaim,
			map[string]any{"admin": map[string]any{"100000000000000002": "test.example.com"}}))
		assertStatus(t, e.post(tok), http.StatusForbidden)
	})
	t.Run("roles_claim_list_of_objects", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withClaim(rolesClaim,
			[]any{map[string]any{"admin": map[string]any{"100000000000000002": "test.example.com"}}}))
		assertStatus(t, e.post(tok), http.StatusCreated)
	})
	// Из TestPatchExperiment: PATCH требует аутентификации и пускает admin.
	t.Run("patch_requires_auth", func(t *testing.T) {
		e := unconfiguredEnv(t)
		assert401(t, e.do(http.MethodPatch, experiments+"test_exp_1", ""))
	})
	t.Run("patch_with_real_admin_token", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.do(http.MethodPatch, experiments+"test_exp_1", "Bearer "+e.admin()), http.StatusOK)
	})
}

// ════════════════ 2. Отсутствующий / битый токен → 401 (TestMissingOrMalformed) ════════════════

func TestMissingOrMalformed(t *testing.T) {
	t.Run("no_token", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.do(http.MethodGet, experiments, ""))
		assertFetches(t, e.srv, 0)
	})
	t.Run("no_token_when_unconfigured_still_401", func(t *testing.T) {
		e := unconfiguredEnv(t)
		assert401(t, e.do(http.MethodPost, experiments, ""))
	})
	for _, h := range []string{
		"Basic dGVzdF91c2VyOnRlc3RfcGFzcw==",
		"Bearer",
		"Bearer ",
		"Bearer not-a-jwt",
		"Bearer a.b.c",
		"Token abc",
	} {
		t.Run("malformed_authorization/"+h, func(t *testing.T) {
			e := newEnv(t)
			assert401(t, e.do(http.MethodGet, experiments, h))
		})
	}
	for _, h := range []string{"Bearer not-a-jwt", "Bearer a.b.c"} {
		t.Run("garbage_token_when_unconfigured_401/"+h, func(t *testing.T) {
			e := unconfiguredEnv(t)
			assert401(t, e.do(http.MethodGet, experiments, h))
		})
	}
	t.Run("missing_kid_when_unconfigured_401", func(t *testing.T) {
		e := unconfiguredEnv(t)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withoutKID())))
	})
	t.Run("missing_kid", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withoutKID())))
		assertFetches(t, e.srv, 0)
	})
	t.Run("too_long_token", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withRoles(RoleAdmin), withClaim("padding", strings.Repeat("x", 9000)))
		if len(tok) <= maxTokenLength {
			t.Fatalf("токен короче лимита: %d", len(tok))
		}
		assert401(t, e.get(tok))
		assertFetches(t, e.srv, 0)
	})
	t.Run("error_response_does_not_echo_token", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withRoles(RoleAdmin), withIssuer("https://evil.example.com"))
		w := e.get(tok)
		assert401(t, w)
		for _, part := range strings.Split(tok, ".") {
			if strings.Contains(w.Body.String(), part) {
				t.Fatal("тело ответа содержит часть токена")
			}
			for _, vals := range w.Header() {
				for _, v := range vals {
					if strings.Contains(v, part) {
						t.Fatal("заголовок ответа содержит часть токена")
					}
				}
			}
		}
		if strings.Contains(w.Body.String(), "evil.example.com") {
			t.Fatal("тело ответа содержит issuer токена")
		}
	})
}

// ════════════════ 3. Claims и подпись → 401 (TestClaims) ════════════════

func TestClaims(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withLifetime(-120*time.Second))))
	})
	t.Run("expired_within_leeway_accepted", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.token(withRoles(RoleAdmin), withLifetime(-10*time.Second))), http.StatusOK)
	})
	t.Run("nbf_in_future", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withRoles(RoleAdmin), withClaim("nbf", e.clock.Now().Unix()+600), withLifetime(time.Hour))
		assert401(t, e.get(tok))
	})
	t.Run("wrong_issuer", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withIssuer("https://other.example.com"))))
	})
	t.Run("wrong_audience", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withAudience("999999999999999999"))))
	})
	t.Run("audience_list_containing_project", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withRoles(RoleAdmin), withAudience([]any{"test_client_1@example", testAudience}))
		assertStatus(t, e.get(tok), http.StatusOK)
	})
	t.Run("audience_list_without_project", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withRoles(RoleAdmin), withAudience([]any{"test_client_1@example", "999999999999999999"}))
		assert401(t, e.get(tok))
	})
	for _, claim := range []string{"exp", "iat", "iss", "aud", "sub", "jti"} {
		t.Run("required_claim_missing/"+claim, func(t *testing.T) {
			e := newEnv(t)
			assert401(t, e.get(e.token(withRoles(RoleAdmin), withDrop(claim))))
		})
	}
	t.Run("id_token_as_bearer_rejected", func(t *testing.T) {
		e := newEnv(t)
		tok := e.token(withRoles(RoleAdmin), withDrop("jti"), withClaim("nonce", "test_nonce_1"),
			withClaim("at_hash", "test_at_hash_1"), withClaim("auth_time", 1700000000))
		assert401(t, e.post(tok))
	})
	t.Run("empty_sub", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withSub(""))))
	})
	t.Run("alg_none", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.get(noneAlgToken(e.clock.Now(), withRoles(RoleAdmin))))
	})
	for _, alg := range []string{"HS256", "RS256"} {
		t.Run("hmac_with_public_key_confusion/"+alg, func(t *testing.T) {
			e := newEnv(t)
			assert401(t, e.post(hs256WithPublicPEM(e.clock.Now(), alg, withRoles(RoleAdmin))))
		})
	}
	t.Run("signed_by_other_key_same_kid", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.post(e.token(withRoles(RoleAdmin), withKey(rsaKey("attacker")))))
	})
	t.Run("tampered_payload", func(t *testing.T) {
		e := newEnv(t)
		good := strings.Split(e.token(withRoles(RoleAnalyst)), ".")
		forged := strings.Split(e.token(withRoles(RoleAdmin), withKey(rsaKey("attacker"))), ".")
		assert401(t, e.post(good[0]+"."+forged[1]+"."+good[2]))
	})

	// ── Дополнительно (поведение PyJWT, явно не покрытое Python-тестами) ──
	t.Run("go/iat_in_future_beyond_leeway", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withClaim("iat", e.clock.Now().Unix()+120))))
	})
	t.Run("go/iat_in_future_within_leeway", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.token(withRoles(RoleAdmin), withClaim("iat", e.clock.Now().Unix()+10))), http.StatusOK)
	})
	t.Run("go/nbf_null_rejected", func(t *testing.T) {
		e := newEnv(t)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withClaim("nbf", nil))))
	})
	t.Run("go/nbf_in_past_accepted", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.token(withRoles(RoleAdmin), withClaim("nbf", e.clock.Now().Unix()-60))), http.StatusOK)
	})
	for name, opt := range map[string]tokenOpt{
		"jti_not_string":   withClaim("jti", 42),
		"jti_null":         withClaim("jti", nil),
		"sub_not_string":   withSub(42),
		"iss_not_string":   withIssuer(42),
		"aud_not_string":   withAudience(42),
		"aud_list_mixed":   withAudience([]any{testAudience, 1}),
		"aud_empty_list":   withAudience([]any{}),
		"exp_null":         withClaim("exp", nil),
		"iat_null":         withClaim("iat", nil),
		"crit_header":      withHeader("crit", []any{"exp"}),
		"kid_not_string":   withHeader("kid", 7),
		"kid_empty":        withKID(""),
		"alg_header_RS512": withHeader("alg", "RS512"),
	} {
		t.Run("go/"+name, func(t *testing.T) {
			e := newEnv(t)
			assert401(t, e.get(e.token(withRoles(RoleAdmin), opt)))
		})
	}
	t.Run("go/empty_jti_accepted_like_pyjwt", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.token(withRoles(RoleAdmin), withClaim("jti", ""))), http.StatusOK)
	})
	t.Run("go/lowercase_bearer_scheme", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.do(http.MethodGet, experiments, "bearer "+e.admin()), http.StatusOK)
		assertStatus(t, e.do(http.MethodGet, experiments, "BEARER   "+e.admin()+"  "), http.StatusOK)
	})
}

// ════════════════ 4. JWKS (TestJwks) ════════════════

func TestJWKS(t *testing.T) {
	t.Run("keys_cached_between_requests", func(t *testing.T) {
		e := newEnv(t)
		for range 3 {
			assertStatus(t, e.get(e.admin()), http.StatusOK)
		}
		assertFetches(t, e.srv, 1)
	})
	t.Run("unknown_kid_triggers_refetch_then_401", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		assertFetches(t, e.srv, 1)
		e.clock.Advance(31 * time.Second)
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withKID("test_kid_unknown"))))
		assertFetches(t, e.srv, 2)
	})
	t.Run("unknown_kid_refetch_rate_limited", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		e.clock.Advance(31 * time.Second)
		unknown := e.token(withRoles(RoleAdmin), withKID("test_kid_unknown"))
		assert401(t, e.get(unknown))
		e.clock.Advance(10 * time.Second)
		assert401(t, e.get(unknown))
		assert401(t, e.get(e.token(withRoles(RoleAdmin), withKID("test_kid_unknown_2"))))
		assertFetches(t, e.srv, 2)        // первичная загрузка + один перезапрос
		e.clock.Advance(25 * time.Second) // 35 c с последней попытки
		assert401(t, e.get(unknown))
		assertFetches(t, e.srv, 3)
	})
	t.Run("unknown_kid_first_request_fetches_once", func(t *testing.T) {
		e := newEnv(t)
		unknown := e.token(withRoles(RoleAdmin), withKID("test_kid_unknown"))
		assert401(t, e.get(unknown))
		assert401(t, e.get(unknown))
		assertFetches(t, e.srv, 1)
	})
	t.Run("key_rotation_new_kid_picked_up", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		e.srv.setDoc(jwksDoc(
			publicJWK(rsaKey("main"), testKID, nil),
			publicJWK(rsaKey("rotated"), "test_kid_2", nil),
		))
		e.clock.Advance(31 * time.Second)
		tok := e.token(withRoles(RoleAdmin), withKey(rsaKey("rotated")), withKID("test_kid_2"))
		assertStatus(t, e.get(tok), http.StatusOK)
		assertFetches(t, e.srv, 2)
	})
	t.Run("stale_keys_used_when_refetch_fails", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		e.srv.fail()
		e.clock.Advance(jwksTTL + time.Second) // ключи протухли → перезапрос, он падает
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		assertFetches(t, e.srv, 2)
	})
	t.Run("stale_keys_kept_when_refetch_returns_empty", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		e.srv.setDoc(map[string]any{"keys": []any{}})
		e.clock.Advance(jwksTTL + time.Second)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
	})
	t.Run("stale_keys_rejected_after_max_age", func(t *testing.T) {
		e := newEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		e.srv.fail()
		e.clock.Advance(jwksMaxStale - 60*time.Second)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		e.clock.Advance(120 * time.Second) // теперь ключам больше суток
		assert503(t, e.get(e.admin()), detailUnavailable)
		// JWKS снова доступен → после rate limit ключи обновляются
		e.srv.setDoc(jwksDoc(publicJWK(rsaKey("main"), testKID, nil)))
		e.clock.Advance(jwksMinRefreshInterval + time.Second)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
	})
	for name, extra := range map[string]map[string]any{
		"use_enc":   {"use": "enc"},
		"alg_RS512": {"alg": "RS512"},
		"kty_EC":    {"kty": "EC"},
		"kid_empty": {"kid": ""},
	} {
		t.Run("unusable_jwk_ignored/"+name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.setDoc(jwksDoc(publicJWK(rsaKey("main"), testKID, extra)))
			assert503(t, e.get(e.admin()), detailUnavailable)
		})
	}
	for name, extra := range map[string]map[string]any{
		"use_sig":           {"use": "sig"},
		"alg_RS256":         {"alg": "RS256"},
		"use_sig+alg_RS256": {"use": "sig", "alg": "RS256"},
	} {
		t.Run("jwk_with_sig_use_and_rs256_alg_accepted/"+name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.setDoc(jwksDoc(publicJWK(rsaKey("main"), testKID, extra)))
			assertStatus(t, e.get(e.admin()), http.StatusOK)
		})
	}

	// ── Дополнительно: детали HTTP-загрузки JWKS ──
	t.Run("go/host_and_accept_headers_sent", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.JWKSHost = "localhost:8081" })
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		if len(e.srv.hosts) != 1 || e.srv.hosts[0] != "localhost:8081" {
			t.Fatalf("Host = %v", e.srv.hosts)
		}
		if e.srv.accepts[0] != "application/json" {
			t.Fatalf("Accept = %q", e.srv.accepts[0])
		}
	})
	t.Run("go/redirect_not_followed", func(t *testing.T) {
		e := newEnv(t)
		target := e.srv.URL
		redirect := httptest.NewServer(http.RedirectHandler(target, http.StatusFound))
		t.Cleanup(redirect.Close)
		e2 := newEnv(t, func(c *Config) { c.JWKSURL = redirect.URL })
		assert503(t, e2.get(e2.admin()), detailUnavailable)
		assertFetches(t, e.srv, 0)
	})
	t.Run("go/oversized_body_rejected", func(t *testing.T) {
		e := newEnv(t)
		e.srv.mu.Lock()
		e.srv.body = []byte(`{"keys":[],"pad":"` + strings.Repeat("x", maxJWKSBodyBytes) + `"}`)
		e.srv.mu.Unlock()
		assert503(t, e.get(e.admin()), detailUnavailable)
	})
	t.Run("go/parse_jwks_rejects_bad_rsa_params", func(t *testing.T) {
		k := rsaKey("main")
		for name, extra := range map[string]map[string]any{
			"e_even":    {"e": b64([]byte{0x01, 0x00, 0x00})},
			"e_one":     {"e": b64([]byte{0x01})},
			"n_bad_b64": {"n": "!!!"},
			"n_missing": {"n": nil},
			"oth":       {"oth": []any{}},
		} {
			raw := mustJSON(jwksDoc(publicJWK(k, testKID, extra)))
			if keys := parseJWKS(raw); len(keys) != 0 {
				t.Fatalf("%s: ключ не должен приниматься", name)
			}
		}
		if keys := parseJWKS(mustJSON(jwksDoc(publicJWK(k, testKID, nil)))); len(keys) != 1 {
			t.Fatal("годный ключ отклонён")
		}
	})
}

// ════════════════ 5. 503: не настроено / JWKS недоступен (TestUnavailable) ════════════════

func TestUnavailable(t *testing.T) {
	for name, clear := range map[string]func(*Config){
		"OIDC_ISSUER":   func(c *Config) { c.Issuer = "" },
		"OIDC_AUDIENCE": func(c *Config) { c.Audience = "" },
		"OIDC_JWKS_URL": func(c *Config) { c.JWKSURL = "" },
	} {
		t.Run("unconfigured_returns_503/"+name, func(t *testing.T) {
			e := newEnv(t, clear)
			assert503(t, e.get(e.admin()), detailNotConfigured)
			assertFetches(t, e.srv, 0)
		})
	}
	t.Run("jwks_fetch_fails_returns_503", func(t *testing.T) {
		e := newEnv(t)
		e.srv.fail()
		assert503(t, e.get(e.admin()), detailUnavailable)
	})
	t.Run("jwks_garbage_returns_503", func(t *testing.T) {
		e := newEnv(t)
		e.srv.setDoc([]any{"not", "a", "jwks"})
		assertStatus(t, e.get(e.admin()), http.StatusServiceUnavailable)
	})
	t.Run("jwks_recovers_after_empty_retry_interval", func(t *testing.T) {
		e := newEnv(t)
		e.srv.fail()
		assertStatus(t, e.get(e.admin()), http.StatusServiceUnavailable)
		e.srv.setDoc(jwksDoc(publicJWK(rsaKey("main"), testKID, nil)))
		e.clock.Advance(jwksEmptyRetryInterval + time.Second)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
	})
	// ── Дополнительно ──
	t.Run("go/nil_verifier_fails_closed", func(t *testing.T) {
		e := newEnv(t)
		e.r = newRouter(nil)
		assert401(t, e.do(http.MethodGet, experiments, ""))
		assert503(t, e.get(e.admin()), detailNotConfigured)
	})
	t.Run("go/fetcher_error_via_injection", func(t *testing.T) {
		e := newEnv(t, func(c *Config) {
			c.Fetcher = func(context.Context) ([]byte, error) { return nil, errors.New("jwks down") }
		})
		assert503(t, e.get(e.admin()), detailUnavailable)
	})
	t.Run("go/fetcher_panic_recovered", func(t *testing.T) {
		e := newEnv(t, func(c *Config) {
			c.Fetcher = func(context.Context) ([]byte, error) { panic("boom") }
		})
		assert503(t, e.get(e.admin()), detailUnavailable)
	})
}

// ════════════════ 6. OIDC_CONFIG_FILE fallback (TestConfigFile) ════════════════

func writeFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fileEnv — issuer/audience пустые в env, берутся из файла.
func fileEnv(t *testing.T, mutate ...func(*Config)) *env {
	path := filepath.Join(t.TempDir(), "oidc.json")
	writeFile(t, path, string(mustJSON(map[string]any{
		"issuer": testIssuer, "project_id": testAudience, "client_id": "test_client_1",
	})))
	return newEnv(t, append([]func(*Config){func(c *Config) {
		c.Issuer, c.Audience, c.ConfigFile = "", "", path
	}}, mutate...)...)
}

func TestConfigFile(t *testing.T) {
	t.Run("issuer_and_audience_from_file", func(t *testing.T) {
		e := fileEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
	})
	t.Run("project_roles_claim_uses_file_project_id", func(t *testing.T) {
		e := fileEnv(t)
		tok := e.token(withClaim(projectRolesClaim(testAudience), map[string]any{"analyst": map[string]any{}}))
		assertStatus(t, e.get(tok), http.StatusOK)
	})
	t.Run("env_audience_overrides_file", func(t *testing.T) {
		e := fileEnv(t, func(c *Config) { c.Audience = "200000000000000002" })
		assert401(t, e.get(e.admin()))
		assertStatus(t, e.get(e.token(withRoles(RoleAdmin), withAudience("200000000000000002"))), http.StatusOK)
	})
	t.Run("env_issuer_overrides_file", func(t *testing.T) {
		e := fileEnv(t, func(c *Config) { c.Issuer = "https://other.example.com" })
		assert401(t, e.get(e.admin()))
	})
	t.Run("missing_file_returns_503", func(t *testing.T) {
		e := newEnv(t, func(c *Config) {
			c.Audience, c.ConfigFile = "", filepath.Join(t.TempDir(), "absent.json")
		})
		assert503(t, e.get(e.admin()), detailNotConfigured)
	})
	for _, content := range []string{`{"issuer": "https://auth.example.com"}`, "not json", "[1, 2]", `{"project_id": 123}`} {
		t.Run("invalid_file_returns_503/"+content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oidc.json")
			writeFile(t, path, content)
			e := newEnv(t, func(c *Config) { c.Audience, c.ConfigFile = "", path })
			assertStatus(t, e.get(e.admin()), http.StatusServiceUnavailable)
		})
	}
	t.Run("file_appearing_later_is_picked_up", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oidc.json")
		e := newEnv(t, func(c *Config) { c.Audience, c.ConfigFile = "", path })
		assertStatus(t, e.get(e.admin()), http.StatusServiceUnavailable)
		writeFile(t, path, string(mustJSON(map[string]any{"issuer": testIssuer, "project_id": testAudience})))
		assertStatus(t, e.get(e.admin()), http.StatusOK)
	})
	// ── Дополнительно ──
	t.Run("go/directory_instead_of_file_503", func(t *testing.T) {
		dir := t.TempDir()
		e := newEnv(t, func(c *Config) { c.Audience, c.ConfigFile = "", dir })
		assertStatus(t, e.get(e.admin()), http.StatusServiceUnavailable)
	})
	t.Run("go/valid_file_cached", func(t *testing.T) {
		e := fileEnv(t)
		assertStatus(t, e.get(e.admin()), http.StatusOK)
		writeFile(t, e.v.cfg.ConfigFile, "not json")
		assertStatus(t, e.get(e.admin()), http.StatusOK)
	})
}

// ════════════════ 7. Verify / RequireRoles напрямую (TestVerifyTokenUnit) ════════════════

func roleSet(roles ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, r := range roles {
		m[r] = struct{}{}
	}
	return m
}

func sameRoles(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func TestVerifyTokenUnit(t *testing.T) {
	ctx := context.Background()
	t.Run("returns_principal", func(t *testing.T) {
		e := newEnv(t)
		p, err := e.v.Verify(ctx, e.token(withRoles(RoleAdmin, RoleAnalyst), withSub("test_user_42")))
		if err != nil {
			t.Fatal(err)
		}
		if p.Subject != "test_user_42" || !sameRoles(p.Roles, roleSet(RoleAdmin, RoleAnalyst)) {
			t.Fatalf("principal %+v", p)
		}
	})
	t.Run("generic_roles_ignored", func(t *testing.T) {
		e := newEnv(t)
		p, err := e.v.Verify(ctx, e.token(withRoles(RoleAnalyst),
			withClaim(genericRolesClaim, map[string]any{"admin": map[string]any{}})))
		if err != nil {
			t.Fatal(err)
		}
		if !sameRoles(p.Roles, roleSet(RoleAnalyst)) {
			t.Fatalf("roles %v", p.Roles)
		}
	})
	t.Run("list_shape_roles_merged", func(t *testing.T) {
		e := newEnv(t)
		p, err := e.v.Verify(ctx, e.token(withClaim(rolesClaim, []any{
			map[string]any{"admin": map[string]any{}}, map[string]any{"analyst": map[string]any{}},
			"junk", map[string]any{"": map[string]any{}},
		})))
		if err != nil {
			t.Fatal(err)
		}
		if !sameRoles(p.Roles, roleSet(RoleAdmin, RoleAnalyst)) {
			t.Fatalf("roles %v", p.Roles)
		}
	})
	t.Run("concurrent_cold_start_single_fetch", func(t *testing.T) {
		// Пока идёт первая загрузка JWKS, остальные запросы ждут её, а не получают 503.
		e := newEnv(t)
		e.srv.setDelay(50 * time.Millisecond)
		var wg sync.WaitGroup
		errs := make([]error, 8)
		subs := make([]string, 8)
		for i := range 8 {
			tok := e.token(withRoles(RoleAdmin), withSub("test_user_"+string(rune('0'+i))))
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := e.v.Verify(ctx, tok)
				errs[i], subs[i] = err, p.Subject
				if err == nil && !sameRoles(p.Roles, roleSet(RoleAdmin)) {
					errs[i] = errors.New("неверные роли")
				}
			}()
		}
		wg.Wait()
		for i := range 8 {
			if errs[i] != nil || subs[i] != "test_user_"+string(rune('0'+i)) {
				t.Fatalf("#%d: %v %q", i, errs[i], subs[i])
			}
		}
		assertFetches(t, e.srv, 1)
	})
	t.Run("concurrent_cold_start_fetch_fails_all_503", func(t *testing.T) {
		e := newEnv(t)
		e.srv.setDelay(20 * time.Millisecond)
		e.srv.fail()
		var wg sync.WaitGroup
		errs := make([]error, 5)
		for i := range 5 {
			tok := e.admin()
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = e.v.Verify(ctx, tok)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if asAuthError(err).Status != http.StatusServiceUnavailable || err == nil {
				t.Fatalf("#%d: %v", i, err)
			}
		}
		assertFetches(t, e.srv, 1)
	})
	t.Run("cancelled_waiter_does_not_cancel_shared_fetch", func(t *testing.T) {
		e := newEnv(t)
		e.srv.setDelay(50 * time.Millisecond)
		firstCtx, cancel := context.WithCancel(ctx)
		firstDone := make(chan error, 1)
		go func() {
			_, err := e.v.Verify(firstCtx, e.admin())
			firstDone <- err
		}()
		time.Sleep(10 * time.Millisecond)
		secondDone := make(chan Principal, 1)
		secondErr := make(chan error, 1)
		go func() {
			p, err := e.v.Verify(ctx, e.token(withRoles(RoleAdmin), withSub("test_user_2")))
			secondDone <- p
			secondErr <- err
		}()
		time.Sleep(5 * time.Millisecond)
		cancel()
		<-firstDone
		p := <-secondDone
		if err := <-secondErr; err != nil || p.Subject != "test_user_2" {
			t.Fatalf("второй запрос: %v %+v", err, p)
		}
		assertFetches(t, e.srv, 1)
	})
	t.Run("require_roles_without_roles_rejected", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("ожидалась паника")
			}
		}()
		RequireRoles(NewVerifier(Config{}))
	})
	// ── Дополнительно ──
	t.Run("go/errors_are_sentinels", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.v.Verify(ctx, "garbage"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err = %v", err)
		}
		e.srv.fail()
		if _, err := e.v.Verify(ctx, e.admin()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("go/principal_from_absent", func(t *testing.T) {
		if _, ok := PrincipalFrom(nil); ok {
			t.Fatal("ожидалось отсутствие principal")
		}
	})
}

// TestConfigFromEnv — чтение OIDC_* из окружения.
func TestConfigFromEnv(t *testing.T) {
	t.Setenv("OIDC_ISSUER", testIssuer)
	t.Setenv("OIDC_AUDIENCE", testAudience)
	t.Setenv("OIDC_JWKS_URL", "http://zitadel.invalid/oauth/v2/keys")
	t.Setenv("OIDC_JWKS_HOST", "localhost:8081")
	t.Setenv("OIDC_CONFIG_FILE", "/zitadel-public/oidc.json")
	got := ConfigFromEnv()
	want := Config{Issuer: testIssuer, Audience: testAudience, JWKSURL: "http://zitadel.invalid/oauth/v2/keys",
		JWKSHost: "localhost:8081", ConfigFile: "/zitadel-public/oidc.json"}
	if got.Issuer != want.Issuer || got.Audience != want.Audience || got.JWKSURL != want.JWKSURL ||
		got.JWKSHost != want.JWKSHost || got.ConfigFile != want.ConfigFile {
		t.Fatalf("got %+v", got)
	}
}
