"""Who may touch the WAF, and where the line sits.

A site owner runs the WAF on their own sites: rule groups, bot blocking, the
on/off switch, and the access log for their domains. They may not write custom
rules -- those are raw ModSecurity directives in a file the web server includes,
ModSecurity actions can run programs, and a directive that fails to parse can
stop the server coming back up for every site on the box.
"""
import pytest
from fastapi import HTTPException

from app.api import waf as waf_api
from app.api import websites as websites_api
from app.models.entities import User, Website


class _Query:
    """Stands in for db.query(Website) over a fixed list."""

    def __init__(self, rows):
        self._rows = list(rows)

    def filter(self, criterion):
        # Only two filters are used here: by id, and by owner.
        rows = self._rows
        try:
            wanted = criterion.right.value
        except AttributeError:
            return _Query(rows)
        name = getattr(criterion.left, "key", "")
        return _Query([r for r in rows if getattr(r, name, None) == wanted])

    def order_by(self, *_):
        return self

    def first(self):
        return self._rows[0] if self._rows else None

    def all(self):
        return list(self._rows)


class _DB:
    def __init__(self, rows):
        self._rows = rows

    def query(self, _model):
        return _Query(self._rows)

    def add(self, *_):
        pass

    def commit(self):
        pass

    def refresh(self, *_):
        pass


def _user(user_id: int, role: str) -> User:
    return User(id=user_id, username=f"u{user_id}", email=f"u{user_id}@example.test", role=role, is_active=True)


def _site(site_id: int, owner_id: int, domain: str) -> Website:
    return Website(
        id=site_id, domain=domain, root_path=f"/home/u/{domain}", owner_id=owner_id,
        waf_enabled=True, waf_default_rules="", waf_custom_rules="",
    )


OWNER = _user(2, "end_user")
STRANGER = _user(3, "end_user")
ADMIN = _user(1, "admin")

MINE = _site(10, owner_id=2, domain="mine.test")
THEIRS = _site(11, owner_id=3, domain="theirs.test")
DB = _DB([MINE, THEIRS])


# --------------------------------------------------------------------------
# Reaching another owner's site
# --------------------------------------------------------------------------

def test_owner_reaches_their_own_site():
    assert waf_api._authorized_website(DB, 10, OWNER) is MINE


def test_owner_cannot_reach_someone_elses_site():
    with pytest.raises(HTTPException) as exc:
        waf_api._authorized_website(DB, 11, OWNER)

    assert exc.value.status_code == 403


def test_admin_reaches_any_site():
    assert waf_api._authorized_website(DB, 11, ADMIN) is THEIRS


def test_a_missing_site_is_404_not_403():
    with pytest.raises(HTTPException) as exc:
        waf_api._authorized_website(DB, 999, ADMIN)

    assert exc.value.status_code == 404


# --------------------------------------------------------------------------
# Log scoping
# --------------------------------------------------------------------------

def test_an_owner_only_gets_their_own_domains_for_log_reads():
    assert waf_api._readable_domains(DB, OWNER) == ["mine.test"]
    assert waf_api._readable_domains(DB, STRANGER) == ["theirs.test"]


def test_an_admin_gets_every_domain():
    assert sorted(waf_api._readable_domains(DB, ADMIN)) == ["mine.test", "theirs.test"]


def test_the_domain_list_is_an_allow_list_not_a_hint():
    from app.services import waf

    # This is what stops ?domain=theirs.test reaching another owner's log.
    with pytest.raises(ValueError, match="not managed"):
        waf._domains_for_log(["mine.test"], domain="theirs.test")


# --------------------------------------------------------------------------
# Custom rules stay with administrators
# --------------------------------------------------------------------------

def test_an_owner_sending_new_custom_rules_is_refused():
    payload = waf_api.WebsiteWafRulesUpdate(
        enabled_rule_ids=["sql-injection"],
        custom_rules='SecRule REQUEST_URI "@rx x" "id:9,phase:1,deny,exec:/tmp/pwn.sh"',
    )

    with pytest.raises(HTTPException) as exc:
        waf_api.save_website_waf(payload, 10, db=DB, current_user=OWNER)

    assert exc.value.status_code == 403
    assert "administrator" in exc.value.detail


def test_an_owner_may_save_everything_else(monkeypatch):
    seen = {}

    def fake_save(website, rule_ids, custom_rules, **kwargs):
        seen["rule_ids"] = list(rule_ids)
        seen["custom_rules"] = custom_rules
        return type("R", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(waf_api.waf, "save_website_config", fake_save)
    monkeypatch.setattr(waf_api.openlitespeed, "update_waf_block", lambda *a, **k: "")
    monkeypatch.setattr(waf_api.waf, "site_config", lambda website: {"domain": website.domain})

    payload = waf_api.WebsiteWafRulesUpdate(
        enabled_rule_ids=["sql-injection", "xss"],
        custom_rules="",
        bot_blocking_enabled=False,
    )
    result = waf_api.save_website_waf(payload, 10, db=DB, current_user=OWNER)

    assert result["domain"] == "mine.test"
    assert seen["rule_ids"] == ["sql-injection", "xss"]
    assert seen["custom_rules"] == ""


def test_resending_the_stored_custom_rules_unchanged_is_allowed(monkeypatch):
    """The UI shows an owner the existing rules read-only and posts them back."""
    site = _site(12, owner_id=2, domain="withcustom.test")
    site.waf_custom_rules = 'SecRule ARGS "@rx evil" "id:7,phase:2,deny,status:403"'
    db = _DB([site])

    monkeypatch.setattr(waf_api.waf, "save_website_config",
                        lambda *a, **k: type("R", (), {"returncode": 0, "stdout": "", "stderr": ""})())
    monkeypatch.setattr(waf_api.openlitespeed, "update_waf_block", lambda *a, **k: "")
    monkeypatch.setattr(waf_api.waf, "site_config", lambda website: {"domain": website.domain})

    payload = waf_api.WebsiteWafRulesUpdate(
        enabled_rule_ids=[], custom_rules=site.waf_custom_rules,
    )

    assert waf_api.save_website_waf(payload, 12, db=db, current_user=OWNER)["domain"] == "withcustom.test"


def test_an_admin_may_write_custom_rules(monkeypatch):
    seen = {}

    def fake_save(website, rule_ids, custom_rules, **kwargs):
        seen["custom_rules"] = custom_rules
        return type("R", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(waf_api.waf, "save_website_config", fake_save)
    monkeypatch.setattr(waf_api.openlitespeed, "update_waf_block", lambda *a, **k: "")
    monkeypatch.setattr(waf_api.waf, "site_config", lambda website: {"domain": website.domain})

    payload = waf_api.WebsiteWafRulesUpdate(enabled_rule_ids=[], custom_rules="SecRule ARGS \"@rx x\" \"id:8,phase:2,deny\"")
    waf_api.save_website_waf(payload, 10, db=DB, current_user=ADMIN)

    assert "id:8" in seen["custom_rules"]


# --------------------------------------------------------------------------
# Server-wide controls stay with administrators
# --------------------------------------------------------------------------

def test_the_rule_catalogue_is_readable_by_an_owner_without_engine_details():
    payload = waf_api.get_waf_rules(current_user=OWNER)

    assert payload["default_rule_definitions"]
    # Nothing describing the machine or the server-wide rule file.
    assert "status" not in payload
    assert "custom_rules" not in payload


def test_an_admin_still_gets_the_engine_details(monkeypatch):
    monkeypatch.setattr(waf_api.waf, "status", lambda: type("R", (), {"__dict__": {"stdout": "ok"}})())
    monkeypatch.setattr(waf_api.waf, "default_rules", lambda: type("R", (), {"stdout": ""})())
    monkeypatch.setattr(waf_api.waf, "custom_rules", lambda: type("R", (), {"stdout": ""})())

    payload = waf_api.get_waf_rules(current_user=ADMIN)

    assert "status" in payload and "custom_rules" in payload


@pytest.mark.parametrize("call", [
    lambda: waf_api.get_waf_status(current_user=OWNER),
    lambda: waf_api.save_waf_custom_rules(waf_api.WafCustomRulesUpdate(content="x"), current_user=OWNER),
    lambda: waf_api.install_waf(current_user=OWNER),
    lambda: waf_api.update_waf_rules(current_user=OWNER),
])
def test_server_wide_actions_refuse_an_owner(call):
    with pytest.raises(HTTPException) as exc:
        call()

    assert exc.value.status_code == 403


def test_saving_the_server_wide_bot_list_refuses_an_owner():
    with pytest.raises(HTTPException) as exc:
        waf_api.save_bad_bots(waf_api.BadBotListUpdate(patterns=["x"]), db=DB, current_user=OWNER)

    assert exc.value.status_code == 403


# --------------------------------------------------------------------------
# The on/off switch lives on the websites router
# --------------------------------------------------------------------------

def test_the_waf_toggle_uses_the_owner_check():
    import inspect

    source = inspect.getsource(websites_api.set_website_waf)

    assert "_get_authorized_website(db, website_id, current_user)" in source
    assert "ensure_role(current_user.role, Role.admin)" not in source
