"""Every user must see the PHP versions actually installed on the box.

`GET /maintenance/php-versions` was admin-only, so the panel never called it for
an end user and the UI fell back to a hard-coded ['8.3', '8.4'] -- an end user
who had asked for PHP 8.1 or 8.5 could not pick it, and could not even see that
it was installed. Reading the list is not privileged; installing one still is.
"""
from pathlib import Path

import pytest
from fastapi import HTTPException

from app.api import maintenance
from app.models.entities import User
from app.services import php

FRONTEND_APP = Path(__file__).resolve().parents[3] / "frontend" / "src" / "App.jsx"


def _user(role: str) -> User:
    return User(id=1, username=role, email=f"{role}@example.test", role=role, is_active=True)


def test_end_user_can_read_the_installed_php_versions(monkeypatch):
    monkeypatch.setattr(php, "list_installed_php", lambda: ["8.1", "8.3", "8.4", "8.5"])

    result = maintenance.get_php_versions(current_user=_user("end_user"))

    assert result["installed"] == ["8.1", "8.3", "8.4", "8.5"]
    assert set(result["supported"]) == set(php.SUPPORTED_PHP_VERSIONS)


def test_reseller_can_read_them_too(monkeypatch):
    monkeypatch.setattr(php, "list_installed_php", lambda: ["8.2"])

    assert maintenance.get_php_versions(current_user=_user("reseller"))["installed"] == ["8.2"]


def test_installing_a_php_version_is_still_admin_only():
    with pytest.raises(HTTPException) as exc:
        maintenance.install_php_version("8.5", current_user=_user("end_user"))

    assert exc.value.status_code == 403


def test_frontend_has_no_hard_coded_installed_list():
    source = FRONTEND_APP.read_text(encoding="utf-8")

    # The old default silently became the answer whenever the fetch was skipped.
    assert "installed: ['8.3', '8.4']" not in source
    assert "useState({ installed: [], supported: [] })" in source
    # ...and the fetch must not be gated on the admin role any more.
    assert "role === 'admin') loadPhpVersions()" not in source
    # Selects render through the helper that keeps the value in use visible.
    assert "phpVersions.installed.map" not in source
    assert source.count("phpVersionOptions(phpVersions.installed") == 3
