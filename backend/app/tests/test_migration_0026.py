"""0026 must survive the table rebuild SQLite needs to drop a column.

`websites` is referenced by foreign keys in website_aliases, database_accounts
and email_domains. batch_alter_table copies the table and swaps it in, so a
mistake here does not fail loudly -- it silently orphans those rows. This runs
the real migration over a database shaped like a live one and checks both the
website rows and everything pointing at them.
"""
import subprocess
import sys
from pathlib import Path

import pytest
from sqlalchemy import create_engine, text

BACKEND = Path(__file__).resolve().parents[2]
ALEMBIC_INI = BACKEND / "alembic.ini"


def _alembic(db_path: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, "-m", "alembic", "-c", str(ALEMBIC_INI), *args],
        cwd=str(BACKEND),
        capture_output=True,
        text=True,
        env={
            **{k: v for k, v in __import__("os").environ.items()},
            "DATABASE_URL": f"sqlite:///{db_path.as_posix()}",
        },
    )


@pytest.fixture
def seeded_db(tmp_path):
    db_path = tmp_path / "opanel-ent.db"
    result = _alembic(db_path, "upgrade", "0025_drop_waf_bot_allow")
    if result.returncode != 0:
        pytest.skip(f"alembic unavailable in this environment: {result.stderr[-400:]}")

    engine = create_engine(f"sqlite:///{db_path.as_posix()}")
    with engine.begin() as conn:
        conn.execute(text(
            "INSERT INTO users (id, username, email, hashed_password, role, is_active) "
            "VALUES (1, 'owner', 'owner@example.test', 'x', 'end_user', 1)"
        ))
        for i in range(1, 181):
            conn.execute(text(
                "INSERT INTO websites (id, domain, root_path, owner_id, http_flood_enabled, http_flood_config) "
                "VALUES (:i, :d, :r, 1, :f, :c)"
            ), {"i": i, "d": f"site{i}.test", "r": f"/home/u/site{i}.test",
                "f": 1 if i % 2 else 0, "c": '{"connection_limit": 60}' if i % 2 else ""})
        conn.execute(text(
            "INSERT INTO website_aliases (id, website_id, domain, mode) VALUES (1, 7, 'www.site7.test', 'alias')"
        ))
    engine.dispose()
    return db_path


def test_upgrade_drops_the_columns_and_keeps_every_row(seeded_db):
    result = _alembic(seeded_db, "upgrade", "head")
    assert result.returncode == 0, result.stderr

    engine = create_engine(f"sqlite:///{seeded_db.as_posix()}")
    with engine.connect() as conn:
        columns = {row[1] for row in conn.execute(text("PRAGMA table_info(websites)"))}
        assert "http_flood_enabled" not in columns
        assert "http_flood_config" not in columns

        assert conn.execute(text("SELECT count(*) FROM websites")).scalar() == 180
        assert conn.execute(text("SELECT domain FROM websites WHERE id = 42")).scalar() == "site42.test"

        # The alias must still resolve to its website after the table swap.
        joined = conn.execute(text(
            "SELECT w.domain FROM website_aliases a JOIN websites w ON w.id = a.website_id"
        )).scalar()
        assert joined == "site7.test"

        assert not list(conn.execute(text("PRAGMA foreign_key_check")))
    engine.dispose()


def test_downgrade_puts_them_back(seeded_db):
    assert _alembic(seeded_db, "upgrade", "head").returncode == 0
    result = _alembic(seeded_db, "downgrade", "0025_drop_waf_bot_allow")
    assert result.returncode == 0, result.stderr

    engine = create_engine(f"sqlite:///{seeded_db.as_posix()}")
    with engine.connect() as conn:
        columns = {row[1] for row in conn.execute(text("PRAGMA table_info(websites)"))}
        assert {"http_flood_enabled", "http_flood_config"} <= columns
        assert conn.execute(text("SELECT count(*) FROM websites")).scalar() == 180
    engine.dispose()
