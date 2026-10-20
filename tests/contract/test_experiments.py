"""Smoke: Admin API экспериментов (CRUD, PATCH-правила)."""
from ._helpers import assert_status, json_of

EXP = "/api/v1/experiments/"


def test_create_returns_201_with_name(client, admin_headers, exp_name):
    name = exp_name()
    r = client.post(EXP, json={"name": name, "variants": {"A": 50, "B": 50}}, headers=admin_headers)
    body = json_of(r, 201)
    assert body["name"] == name
    assert isinstance(body.get("message"), str) and body["message"]
    client.delete(f"{EXP}{name}", headers=admin_headers)


def test_create_duplicate_returns_400(client, admin_headers, make_experiment):
    name = make_experiment()
    r = client.post(EXP, json={"name": name, "variants": {"A": 1}}, headers=admin_headers)
    assert_status(r, 400)
    assert "detail" in r.json()


def test_create_invalid_name_returns_422(client, admin_headers):
    r = client.post(EXP, json={"name": "bad name!", "variants": {"A": 1}}, headers=admin_headers)
    assert_status(r, 422)


def test_create_reserved_name_returns_422(client, admin_headers):
    r = client.post(EXP, json={"name": "Active", "variants": {"A": 1}}, headers=admin_headers)
    assert_status(r, 422)


def test_create_non_positive_weight_returns_422(client, admin_headers, exp_name):
    r = client.post(EXP, json={"name": exp_name(), "variants": {"A": 0, "B": 10}}, headers=admin_headers)
    assert_status(r, 422)


def test_create_missing_variants_returns_422(client, admin_headers, exp_name):
    r = client.post(EXP, json={"name": exp_name()}, headers=admin_headers)
    assert_status(r, 422)


def test_list_contains_created_with_full_shape(client, admin_headers, make_experiment):
    name = make_experiment({"A": 30, "B": 70}, config={"A": {"color": "red"}}, url="/landing")
    items = json_of(client.get(EXP, headers=admin_headers))
    assert isinstance(items, list)
    item = next((i for i in items if i.get("name") == name), None)
    assert item is not None, f"{name} нет в списке"
    assert item == {
        "name": name,
        "variants": {"A": 30, "B": 70},
        "config": {"A": {"color": "red"}},
        "url": "/landing",
        "is_active": True,
        "total_weight": 100,
    }


def test_list_is_newest_first(client, admin_headers, make_experiment):
    first = make_experiment()
    second = make_experiment()
    names = [i["name"] for i in json_of(client.get(EXP, headers=admin_headers))]
    assert names.index(second) < names.index(first)


def test_patch_returns_updated_experiment(client, admin_headers, make_experiment):
    name = make_experiment({"A": 50, "B": 50})
    body = json_of(client.patch(f"{EXP}{name}", json={"is_active": False, "url": "/x"}, headers=admin_headers))
    assert body["name"] == name
    assert body["is_active"] is False
    assert body["url"] == "/x"
    assert body["variants"] == {"A": 50, "B": 50}
    assert body["total_weight"] == 100


def test_patch_variants_of_active_returns_409(client, admin_headers, make_experiment):
    name = make_experiment({"A": 50, "B": 50})
    r = client.patch(f"{EXP}{name}", json={"variants": {"A": 10, "B": 90}}, headers=admin_headers)
    assert_status(r, 409)


def test_patch_variants_with_deactivation_returns_200(client, admin_headers, make_experiment):
    name = make_experiment({"A": 50, "B": 50})
    r = client.patch(f"{EXP}{name}", json={"variants": {"A": 10, "B": 90}, "is_active": False}, headers=admin_headers)
    body = json_of(r)
    assert body["variants"] == {"A": 10, "B": 90}
    assert body["is_active"] is False


def test_patch_empty_body_returns_400(client, admin_headers, make_experiment):
    name = make_experiment()
    assert_status(client.patch(f"{EXP}{name}", json={}, headers=admin_headers), 400)


def test_patch_unknown_field_returns_422(client, admin_headers, make_experiment):
    name = make_experiment()
    assert_status(client.patch(f"{EXP}{name}", json={"name": "other"}, headers=admin_headers), 422)


def test_patch_missing_returns_404(client, admin_headers, exp_name):
    assert_status(client.patch(f"{EXP}{exp_name('missing')}", json={"is_active": False}, headers=admin_headers), 404)


def test_delete_then_delete_again_returns_404(client, admin_headers, exp_name):
    name = exp_name()
    assert_status(client.post(EXP, json={"name": name, "variants": {"A": 1}}, headers=admin_headers), 201)
    body = json_of(client.delete(f"{EXP}{name}", headers=admin_headers))
    assert isinstance(body.get("message"), str)
    assert_status(client.delete(f"{EXP}{name}", headers=admin_headers), 404)
