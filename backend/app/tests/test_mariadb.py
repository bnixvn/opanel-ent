import pytest

from app.services import mariadb


def test_reserved_names_cannot_be_created():
    """db_user="root" used to run ALTER USER on the server's own root account."""
    for name in ("root", "mysql", "opanelent", "opanel_ent", "opanel"):
        with pytest.raises(ValueError, match="reserved"):
            mariadb.create_database_credentials("safe_db", name, "PasswordLongEnough1")
        with pytest.raises(ValueError, match="reserved"):
            mariadb.create_database_credentials(name, "safe_user", "PasswordLongEnough1")
    # Uppercase never reaches the reserved check -- the charset rule rejects it
    # first -- but it must still not get through.
    with pytest.raises(ValueError):
        mariadb.create_database_credentials("safe_db", "ROOT", "PasswordLongEnough1")


def test_user_facing_create_refuses_to_take_over_an_existing_name(monkeypatch):
    captured = {}
    monkeypatch.setattr(mariadb, "_run_sql", lambda sql, **kw: captured.setdefault("sql", sql))
    mariadb.create_database_credentials("site_db", "site_user", "PasswordLongEnough1")
    sql = captured["sql"]
    assert "CREATE DATABASE `site_db`" in sql
    assert "IF NOT EXISTS" not in sql
    assert "ALTER USER" not in sql


def test_restore_path_may_still_recreate_its_own_accounts(monkeypatch):
    captured = {}
    monkeypatch.setattr(mariadb, "_run_sql", lambda sql, **kw: captured.setdefault("sql", sql))
    mariadb.create_database_credentials(
        "site_db", "site_user", "PasswordLongEnough1", allow_existing=True
    )
    assert "IF NOT EXISTS" in captured["sql"]
    assert "ALTER USER" in captured["sql"]
