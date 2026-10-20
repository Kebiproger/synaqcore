"""Smoke: аутентификация (JWT RS256 + JWKS) и роли admin/analyst."""
import time

import pytest

from ._helpers import assert_status, assert_unauthorized

EXP = "/api/v1/experiments/"


@pytest.mark.parametrize(
    "method,path",
    [("POST", EXP), ("GET", EXP), ("PATCH", f"{EXP}any_name"), ("DELETE", f"{EXP}any_name")],
)
def test_admin_endpoints_require_token(client, method, path):
    body = {"name": "s_noauth", "variants": {"A": 1}} if method == "POST" else ({"is_active": False} if method == "PATCH" else None)
    assert_unauthorized(client.request(method, path, json=body))


def test_garbage_token_returns_401(client):
    assert_unauthorized(client.get(EXP, headers={"Authorization": "Bearer not-a-jwt"}))


def test_bad_signature_returns_401(client, tokens):
    assert_unauthorized(client.get(EXP, headers=tokens.bearer(("admin",), foreign_key=True)))


def test_expired_token_returns_401(client, tokens):
    past = int(time.time()) - 7200
    assert_unauthorized(client.get(EXP, headers=tokens.bearer(("admin",), iat=past - 3600, nbf=past - 3600, exp=past)))


def test_wrong_audience_returns_401(client, tokens):
    assert_unauthorized(client.get(EXP, headers=tokens.bearer(("admin",), aud=["999999999999999999"])))


def test_missing_jti_returns_401(client, tokens):
    assert_unauthorized(client.get(EXP, headers=tokens.bearer(("admin",), jti=None)))


def test_analyst_can_list_but_not_create(client, analyst_headers, exp_name):
    assert_status(client.get(EXP, headers=analyst_headers), 200)
    r = client.post(EXP, json={"name": exp_name(), "variants": {"A": 1}}, headers=analyst_headers)
    assert_status(r, 403)


def test_token_without_roles_returns_403(client, norole_headers):
    assert_status(client.get(EXP, headers=norole_headers), 403)


def test_role_of_other_project_is_ignored(client, tokens):
    other = {"urn:zitadel:iam:org:project:999999999999999999:roles": {"admin": {"1": "x"}}}
    headers = tokens.bearer((), **other)
    assert_status(client.get(EXP, headers=headers), 403)


@pytest.mark.parametrize(
    "method,path,body",
    [
        ("GET", "/api/v1/variants/s_public?user_id=user_test_1", None),
        ("GET", "/api/v1/variants/active", None),
        ("GET", "/api/v1/variants/eval-configs", None),
        ("POST", "/api/v1/variants/batch", {"user_id": "user_test_1", "experiments": ["s_public"]}),
    ],
)
def test_public_endpoints_need_no_token(client, method, path, body):
    assert_status(client.request(method, path, json=body), 200)
