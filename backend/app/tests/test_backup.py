import json
import tarfile
from pathlib import Path

from app.core.config import settings
from app.models.entities import Website
from app.services import backup


def _site_backup(backup_root: Path) -> Path:
    domain_dir = backup_root / "example.test"
    domain_dir.mkdir(parents=True)
    archive_path = domain_dir / "example.test-20260802000000.tar.gz"
    source = backup_root / "index.html"
    source.write_text("restored", encoding="utf-8")
    database = backup_root / "database.sql"
    database.write_text("SELECT 1;", encoding="utf-8")
    with tarfile.open(archive_path, "w:gz") as archive:
        archive.add(source, arcname="site/public_html/index.html")
        archive.add(database, arcname="database/example.sql")
    return archive_path


def test_restore_backup_for_site_user_uses_privileged_restore(tmp_path, monkeypatch):
    backup_root = tmp_path / "backups"
    archive_path = _site_backup(backup_root)
    site_root = tmp_path / "site"
    site_root.mkdir()
    calls = []

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(settings, "backup_root", str(backup_root))
    monkeypatch.setattr(backup.shell, "privileged", fake_privileged)
    # restore_backup returns before the helper call when dry-run is on, and the
    # suite leaves it on; this test is about the privileged path itself.
    monkeypatch.setattr(settings, "command_dry_run", False)
    website = Website(
        domain="example.test",
        owner_id=1,
        root_path=str(site_root),
        linux_user="siteuser",
    )

    restored = backup.restore_backup(website, str(archive_path))

    assert restored == str(site_root.resolve())
    assert [call[0] for call in calls] == ["site-backup-restore"]
    assert calls[0][1][0:3] == ["siteuser", str(site_root), str(archive_path)]


def test_restore_helper_validates_and_extracts_external_backup():
    helper = (Path(__file__).resolve().parents[3] / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    assert "site-backup-restore)" in helper
    assert "backup path escapes website root" in helper


def _user_backup_archive(path: Path, *, kind: str) -> Path:
    manifest = {
        "kind": kind,
        "version": 1,
        "generated_at": "2026-09-02T00:00:00Z",
        "user": {"username": "movingco", "email": "old@users.bpanel.vn", "role": "end_user"},
        "websites": [{"domain": "shop.movingco.test", "app_type": "wordpress",
                      "document_root": "public_html",
                      "database": {"db_name": "movingco_shop", "db_user": "movingco_shop",
                                   "db_password": "x", "sql_member": "databases/shop.movingco.test.sql"}}],
        "applications": [{"name": "queue-worker"}],  # bpanel-only, must be ignored
    }
    mfile = path.parent / "manifest.json"
    mfile.write_text(json.dumps(manifest), encoding="utf-8")
    with tarfile.open(path, "w:gz") as tar:
        tar.add(mfile, arcname=backup.BACKUP_MANIFEST)
    return path


def test_bpanel_user_backup_is_accepted_for_restore(tmp_path, monkeypatch):
    monkeypatch.setattr(settings, "backup_root", str(tmp_path))
    assert "bpanel_user" in backup.RESTORABLE_BACKUP_KINDS

    good = _user_backup_archive(tmp_path / "user-movingco.tar.gz", kind="bpanel_user")
    info = backup.describe_user_backup(str(good))
    assert info["valid"] is True
    assert info["username"] == "movingco"
    assert info["websites"] == 1
    assert info["error"] == ""


def test_unknown_backup_kind_is_still_rejected(tmp_path, monkeypatch):
    monkeypatch.setattr(settings, "backup_root", str(tmp_path))
    bad = _user_backup_archive(tmp_path / "user-other.tar.gz", kind="cpanel_user")
    info = backup.describe_user_backup(str(bad))
    assert info["valid"] is False
    assert info["error"] == "This is not a full user backup"


def test_private_key_loader_survives_paramikos_dsskey_removal(monkeypatch):
    """paramiko 5 dropped DSSKey. The loader used to name it directly, so the
    whole backup service raised AttributeError on import after the upgrade."""
    import paramiko

    monkeypatch.delattr(paramiko, "DSSKey", raising=False)
    # Still reaches the "tried every key type" path instead of AttributeError.
    try:
        backup._load_private_key("not-a-key")
    except ValueError as exc:
        assert "Cannot load SFTP private key" in str(exc)
    else:  # pragma: no cover - the junk key must not load
        raise AssertionError("junk key should not load")
