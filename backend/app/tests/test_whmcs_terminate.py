"""Terminate destroys everything, and every layer must say so.

The WHMCS module used to send `backup=true` against a contract that promised a
soft delete keeping the home directory. The API has no such parameter -- FastAPI
drops unknown query params silently -- so the call always ran a full teardown
while the caller believed data was kept, and returned {"terminated": true} with
no hint that the parameter had been ignored.
"""
import re
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parents[3]
MODULE = PROJECT_ROOT / "modules" / "servers" / "opanelent" / "opanelent.php"
CONTRACT = PROJECT_ROOT / "docs" / "whmcs-opanel-ent-contract.md"
HELPER = PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh"


def _terminate_function() -> str:
    source = MODULE.read_text(encoding="utf-8")
    start = source.index("function opanelent_TerminateAccount(")
    return source[start:source.index("\nfunction ", start + 1)]


def test_module_sends_no_backup_parameter():
    body = _terminate_function()
    call = next(line for line in body.splitlines() if "opanelent_request(" in line)

    assert "'DELETE'" in call
    assert "backup" not in call
    # opanelent_request($params, $method, $path, $payload = null, $query = []).
    # Neither trailing argument is passed: no payload, and no query array.
    assert "[" not in call, call.strip()
    assert "null" not in call, call.strip()


def test_module_says_terminate_is_destructive():
    body = _terminate_function().lower()

    assert "full teardown" in body or "deletes everything" in body


def test_contract_no_longer_advertises_a_soft_delete():
    text = CONTRACT.read_text(encoding="utf-8")
    terminate = text[text.index("#### `DELETE /accounts/"):]
    terminate = terminate[:terminate.index("#### `PATCH")]

    assert "backup=true" not in terminate
    assert "soft delete" not in terminate.lower()
    assert "irreversible" in terminate.lower()

    lifecycle = text[text.index("### Terminate"):]
    lifecycle = lifecycle[:lifecycle.index("### ChangePassword")]
    assert "soft" not in lifecycle.lower()
    assert "suspend" in lifecycle.lower(), "point the reader at the non-destructive option"


def test_the_documented_route_matches_the_implemented_one():
    api = (PROJECT_ROOT / "backend" / "app" / "api" / "provisioning.py").read_text(encoding="utf-8")
    start = api.index('@router.delete("/accounts/{external_id}")')
    signature = api[start:api.index(") ->", start)]

    # If a backup parameter is ever added, the contract and module must change
    # with it -- this test is the reminder.
    assert "backup" not in signature


def test_contract_does_not_sell_backups_as_a_rollback_path():
    text = CONTRACT.read_text(encoding="utf-8")
    terminate = text[text.index("#### `DELETE /accounts/"):]
    terminate = terminate[:terminate.index("#### `PATCH")]

    assert "/var/backups/opanel-ent" in terminate
    assert re.search(r"only if backups were configured", terminate, re.I)


def test_helper_teardown_still_removes_the_home_directory():
    # The contract's wording is only honest while this is what actually happens.
    helper = HELPER.read_text(encoding="utf-8")
    start = helper.index("delete_panel_user_runtime() {")
    block = helper[start:helper.index("\n}", start)]

    assert 'rm -rf "$HOME_ROOT/$user"' in block
    assert 'userdel "$user"' in block
