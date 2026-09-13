import pytest

from app.schemas.schemas import PhpConfigUpdate
from app.services import php


@pytest.fixture(autouse=True)
def dry_run(monkeypatch):
    monkeypatch.setattr(php.settings, "command_dry_run", True)


def _ini(**kw):
    return php.update_php_ini(PhpConfigUpdate(php_version="8.3", **kw))


def test_toggle_on_writes_enable_1():
    out = _ini(opcache_enable=True)
    assert "opcache.enable = 1" in out
    assert "opcache.enable_cli = 1" in out


def test_toggle_off_writes_enable_0():
    out = _ini(opcache_enable=False)
    assert "opcache.enable = 0" in out
    assert "opcache.enable_cli = 0" in out


def test_default_is_on():
    assert "opcache.enable = 1" in _ini()


def test_tuned_block_survives_an_unrelated_edit(tmp_path, monkeypatch):
    """Regression: update_php_ini re-emitted only the basic directives, so saving
    memory_limit wiped the whole tuned opcache block written by the auto-tuner."""
    ini = tmp_path / "99-opanel-ent.ini"
    ini.write_text(
        "memory_limit = 512M\n"
        "opcache.enable = 1\n"
        "opcache.memory_consumption = 512\n"
        "opcache.max_accelerated_files = 20000\n"
        "opcache.validate_timestamps = 1\n"
        "opcache.jit = 1255\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(php, "_lsphp_opanel_ini_path", lambda v: ini)

    out = _ini(memory_limit="2048M", opcache_enable=True)

    assert "memory_limit = 2048M" in out
    for kept in (
        "opcache.memory_consumption = 512",
        "opcache.max_accelerated_files = 20000",
        "opcache.validate_timestamps = 1",
        "opcache.jit = 1255",
    ):
        assert kept in out, kept
    # and the switch itself is written exactly once, from the payload
    assert out.count("opcache.enable = ") == 1


def test_turning_it_off_keeps_the_tuning_for_later(tmp_path, monkeypatch):
    ini = tmp_path / "99-opanel-ent.ini"
    ini.write_text("opcache.enable = 1\nopcache.memory_consumption = 256\n", encoding="utf-8")
    monkeypatch.setattr(php, "_lsphp_opanel_ini_path", lambda v: ini)
    out = _ini(opcache_enable=False)
    assert "opcache.enable = 0" in out
    assert "opcache.memory_consumption = 256" in out
