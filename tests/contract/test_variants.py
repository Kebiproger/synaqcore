"""Smoke: публичная выдача вариантов и конфигов (Client API)."""
from ._helpers import assert_status, expected_variant, json_of

EXP = "/api/v1/experiments/"
VAR = "/api/v1/variants"
USERS = [f"user_test_{i}" for i in range(100)]


def test_variant_by_path_has_shape(client, make_experiment):
    name = make_experiment({"A": 50, "B": 50})
    body = json_of(client.get(f"{VAR}/{name}", params={"user_id": "user_test_1"}))
    assert body == {
        "experiment_name": name,
        "user_id": "user_test_1",
        "variant": expected_variant("user_test_1", name, {"A": 50, "B": 50}),
    }


def test_variant_by_query_equals_path(client, make_experiment):
    name = make_experiment({"A": 1, "B": 1, "C": 1})
    for user in USERS[:10]:
        by_path = json_of(client.get(f"{VAR}/{name}", params={"user_id": user}))
        by_query = json_of(client.get(f"{VAR}/", params={"experiment_name": name, "user_id": user}))
        assert by_path == by_query


def test_variant_without_user_id_returns_400(client, make_experiment):
    name = make_experiment()
    assert_status(client.get(f"{VAR}/{name}"), 400)


def test_variant_of_unknown_experiment_is_control(client, exp_name):
    body = json_of(client.get(f"{VAR}/{exp_name('nope')}", params={"user_id": "user_test_1"}))
    assert body["variant"] == "control"


def test_variant_of_inactive_experiment_is_control(client, make_experiment):
    name = make_experiment({"A": 50, "B": 50}, is_active=False)
    for user in USERS[:20]:
        assert json_of(client.get(f"{VAR}/{name}", params={"user_id": user}))["variant"] == "control"


def test_allocation_is_deterministic_and_matches_rule(client, make_experiment):
    variants = {"control": 34, "treatment_a": 33, "treatment_b": 33}
    name = make_experiment(variants)
    got = {u: json_of(client.get(f"{VAR}/{name}", params={"user_id": u}))["variant"] for u in USERS}
    expected = {u: expected_variant(u, name, variants) for u in USERS}
    mismatched = {u: (got[u], expected[u]) for u in USERS if got[u] != expected[u]}
    assert not mismatched, f"расхождения с эталонной аллокацией: {dict(list(mismatched.items())[:5])}"
    # Повторный запрос — тот же вариант
    assert json_of(client.get(f"{VAR}/{name}", params={"user_id": USERS[0]}))["variant"] == got[USERS[0]]


def test_batch_matches_single_and_unknown_is_control(client, make_experiment, exp_name):
    variants = {"A": 20, "B": 80}
    names = [make_experiment(variants), make_experiment(variants)]
    unknown = exp_name("nope")
    body = json_of(client.post(f"{VAR}/batch", json={"user_id": "user_test_7", "experiments": names + [unknown]}))
    assert body["user_id"] == "user_test_7"
    assert body["variants"] == {
        names[0]: expected_variant("user_test_7", names[0], variants),
        names[1]: expected_variant("user_test_7", names[1], variants),
        unknown: "control",
    }


def test_batch_empty_list_returns_422(client):
    assert_status(client.post(f"{VAR}/batch", json={"user_id": "user_test_1", "experiments": []}), 422)


def test_batch_missing_user_id_returns_422(client, exp_name):
    assert_status(client.post(f"{VAR}/batch", json={"experiments": [exp_name()]}), 422)


def test_active_lists_only_active(client, make_experiment):
    active = make_experiment({"A": 1, "B": 1}, url="/promo")
    inactive = make_experiment(is_active=False)
    items = json_of(client.get(f"{VAR}/active"))
    assert isinstance(items, list)
    by_name = {i["name"]: i for i in items}
    assert inactive not in by_name
    item = by_name.get(active)
    assert item is not None, f"{active} нет в /active"
    assert set(item) == {"name", "url", "variants"}
    assert item["url"] == "/promo"
    assert sorted(item["variants"]) == ["A", "B"]


def test_eval_configs_has_active_with_weights(client, make_experiment):
    active = make_experiment({"A": 25, "B": 75}, config={"A": {"k": 1}})
    inactive = make_experiment(is_active=False)
    body = json_of(client.get(f"{VAR}/eval-configs"))
    exps = body["experiments"]
    assert isinstance(exps, dict)
    assert inactive not in exps
    assert exps[active] == {"name": active, "variants": {"A": 25, "B": 75}, "config": {"A": {"k": 1}}, "url": None}


def test_config_has_shape_and_default_config(client, make_experiment):
    name = make_experiment({"A": 50, "B": 50})
    body = json_of(client.get(f"{VAR}/config/{name}"))
    assert body == {"name": name, "variants": {"A": 50, "B": 50}, "config": {}, "url": None, "is_active": True}


def test_config_of_unknown_returns_404(client, exp_name):
    assert_status(client.get(f"{VAR}/config/{exp_name('nope')}"), 404)


def test_read_your_writes_after_patch(client, admin_headers, make_experiment):
    name = make_experiment({"A": 50, "B": 50})
    user = "user_test_42"
    assert json_of(client.get(f"{VAR}/{name}", params={"user_id": user}))["variant"] in ("A", "B")

    # Выключение видно сразу (кэш не отдаёт устаревший конфиг)
    json_of(client.patch(f"{EXP}{name}", json={"is_active": False}, headers=admin_headers))
    assert json_of(client.get(f"{VAR}/{name}", params={"user_id": user}))["variant"] == "control"
    assert json_of(client.get(f"{VAR}/config/{name}"))["is_active"] is False

    # Новые веса + включение — сразу новая аллокация
    new = {"only": 1}
    json_of(client.patch(f"{EXP}{name}", json={"variants": new}, headers=admin_headers))
    json_of(client.patch(f"{EXP}{name}", json={"is_active": True}, headers=admin_headers))
    assert json_of(client.get(f"{VAR}/{name}", params={"user_id": user}))["variant"] == "only"
    assert json_of(client.get(f"{VAR}/config/{name}"))["variants"] == new


def test_delete_makes_config_404_and_variant_control(client, admin_headers, exp_name):
    name = exp_name()
    assert_status(client.post(EXP, json={"name": name, "variants": {"A": 1}}, headers=admin_headers), 201)
    assert json_of(client.get(f"{VAR}/{name}", params={"user_id": "user_test_1"}))["variant"] == "A"
    json_of(client.delete(f"{EXP}{name}", headers=admin_headers))
    assert_status(client.get(f"{VAR}/config/{name}"), 404)
    assert json_of(client.get(f"{VAR}/{name}", params={"user_id": "user_test_1"}))["variant"] == "control"
