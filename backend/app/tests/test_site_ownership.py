"""Site files belong to the site's own Linux user, never to a shared account.

`fix-permissions` used to fall back to `chown -R www-data` whenever the caller
did not pass a site user. That handed one account ownership of the whole tree,
so any other site running PHP as www-data could read and write these files --
the exact isolation the per-site Linux user exists to provide.
"""
from pathlib import Path

import pytest

from app.services import site_users

PROJECT_ROOT = Path(__file__).resolve().parents[3]
HELPER_SCRIPT = PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh"


def _capture(monkeypatch):
    calls = []

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(site_users.shell, "privileged", fake_privileged)
    return calls


def test_fix_permissions_passes_the_site_user_through(monkeypatch):
    calls = _capture(monkeypatch)

    site_users.fix_site_permissions("/home/siteuser/example.test", "siteuser")

    command, args, kwargs = calls[0]
    assert command == "fix-permissions"
    assert args == ["/home/siteuser/example.test", "siteuser"]
    assert kwargs["fallback"] == ["chown", "-R", "siteuser:siteuser", "/home/siteuser/example.test"]


def test_fix_permissions_without_a_user_derives_it_from_the_path(monkeypatch):
    calls = _capture(monkeypatch)

    site_users.fix_site_permissions("/home/siteuser/example.test/public_html", None)

    command, args, kwargs = calls[0]
    assert args == ["/home/siteuser/example.test/public_html", "siteuser"]
    assert "www-data" not in " ".join(kwargs["fallback"])


def test_fix_permissions_refuses_a_path_it_cannot_attribute(monkeypatch):
    calls = _capture(monkeypatch)

    with pytest.raises(ValueError, match="Cannot tell which site user"):
        site_users.fix_site_permissions("/var/www/html", None)

    assert calls == []


def test_site_user_from_path_only_matches_managed_site_roots():
    assert site_users.site_user_from_path("/home/siteuser/example.test") == "siteuser"
    assert site_users.site_user_from_path("/home/siteuser/example.test/public_html/wp") == "siteuser"
    assert site_users.site_user_from_path("/home/siteuser") is None
    assert site_users.site_user_from_path("/etc/passwd") is None


def test_helper_fix_permissions_never_chowns_to_www_data():
    content = HELPER_SCRIPT.read_text(encoding="utf-8")
    start = content.index("  fix-permissions)")
    block = content[start:content.index("  site-path-fix)", start)]
    code = "\n".join(l for l in block.splitlines() if not l.lstrip().startswith("#"))

    assert "www-data" not in code
    assert 'fix_site_tree "$target" "$site_user"' in block
    assert 'require_linux_user "$site_user"' in block


def test_helper_no_longer_offers_a_chown_www_command():
    content = HELPER_SCRIPT.read_text(encoding="utf-8")
    start = content.index("  chown-www)")
    block = content[start:content.index("  fix-permissions)", start)]

    assert "chown -R www-data" not in block
    assert "deny " in block
