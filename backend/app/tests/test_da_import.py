from io import BytesIO
import os
import tarfile
import threading
from pathlib import Path

import pytest

from app.core.config import settings
from app.services import da_import

HELPER_SCRIPT = Path(__file__).resolve().parents[3] / "installer" / "files" / "opanel-ent-helper.sh"


def test_import_da_backup_resolves_filename_from_da_backup_dir(tmp_path, monkeypatch):
    backup_dir = tmp_path / "da"
    backup_dir.mkdir()
    archive = backup_dir / "user.admin.tar.zst"
    archive.write_bytes(b"placeholder")
    seen = {}

    def fake_extract(path, stage):
        seen["archive"] = path
        seen["stage_exists"] = stage.exists()

    def fake_process(stage, archive_name, db, credentials, overwrite=False):
        credentials.append("panel-user: password")
        return {"archive": archive_name, "created_users": ["panel-user"]}

    monkeypatch.setattr(settings, "da_backup_dir", str(backup_dir))
    monkeypatch.setattr(da_import, "_safe_extract_tar", fake_extract)
    monkeypatch.setattr(da_import, "_process_archive", fake_process)

    summary = da_import.import_da_backup("user.admin.tar.zst", db=object())

    assert summary["archive"] == "user.admin.tar.zst"
    assert seen == {"archive": archive.resolve(), "stage_exists": True}
    assert (backup_dir / "user.admin-credentials.txt").read_text(encoding="utf-8").endswith(
        "panel-user: password\n"
    )


def test_delete_da_backup_accepts_filename_only(tmp_path, monkeypatch):
    backup_dir = tmp_path / "da"
    backup_dir.mkdir()
    archive = backup_dir / "user.admin.tar.zst"
    archive.write_bytes(b"placeholder")
    monkeypatch.setattr(settings, "da_backup_dir", str(backup_dir))

    deleted = da_import.delete_da_backup("user.admin.tar.zst")

    assert deleted == "user.admin.tar.zst"
    assert not archive.exists()


def test_delete_da_backup_rejects_nested_relative_path(tmp_path, monkeypatch):
    backup_dir = tmp_path / "da"
    nested = backup_dir / "nested"
    nested.mkdir(parents=True)
    (nested / "user.admin.tar.zst").write_bytes(b"placeholder")
    monkeypatch.setattr(settings, "da_backup_dir", str(backup_dir))

    with pytest.raises(FileNotFoundError, match="Backup not found"):
        da_import.delete_da_backup("nested/user.admin.tar.zst")


def test_safe_extract_tar_zst_uses_zstdcat_without_decompress_flag(tmp_path, monkeypatch):
    raw_tar = BytesIO()
    source = tmp_path / "index.html"
    source.write_text("restored", encoding="utf-8")
    with tarfile.open(fileobj=raw_tar, mode="w") as archive:
        archive.add(source, arcname="domains/example.test/public_html/index.html")
    raw_tar.seek(0)

    zst_archive = tmp_path / "user.admin.tar.zst"
    zst_archive.write_bytes(b"compressed-placeholder")
    destination = tmp_path / "stage"
    destination.mkdir()
    calls = []

    class FakePopen:
        """Feeds the tar bytes through a real OS pipe (non-seekable), like a
        genuine subprocess.Popen(..., stdout=PIPE) would — a BytesIO stand-in
        would stay seekable and silently miss bugs that only show up on a
        real pipe (e.g. "[Errno 29] Illegal seek")."""

        def __init__(self, args, stdout=None, stderr=None):
            calls.append(args)
            read_fd, write_fd = os.pipe()
            self.stdout = os.fdopen(read_fd, "rb")
            self.returncode = 0

            def _feed():
                with os.fdopen(write_fd, "wb") as w:
                    w.write(raw_tar.getvalue())

            threading.Thread(target=_feed, daemon=True).start()

        def communicate(self, timeout=None):
            return b"", b""

    monkeypatch.setattr(da_import.shutil, "which", lambda name: "/usr/bin/zstdcat" if name == "zstdcat" else None)
    monkeypatch.setattr(da_import.subprocess, "Popen", FakePopen)

    da_import._safe_extract_tar(zst_archive, destination)

    assert calls == [["/usr/bin/zstdcat", str(zst_archive)]]
    assert (destination / "domains/example.test/public_html/index.html").read_text(encoding="utf-8") == "restored"


def test_da_db_credentials_reads_a_plaintext_password(tmp_path):
    backup = tmp_path / "backup"
    backup.mkdir()
    dump = backup / "admin_shop.sql"
    dump.write_text("-- dump", encoding="utf-8")
    (backup / "admin_shop.conf").write_text(
        "user=admin_shop\npasswd=S3cret!pass\n", encoding="utf-8"
    )

    assert da_import._da_db_credentials(dump, tmp_path) == {"password": "S3cret!pass", "password_hash": ""}


def test_da_db_credentials_reports_a_hash_separately(tmp_path):
    """A mysql_native hash cannot be shown to the admin as a password, so it
    must not be handed back as one -- the import falls back to a fresh
    password and says so in the log."""
    backup = tmp_path / "backup"
    backup.mkdir()
    dump = backup / "admin_blog.sql.gz"
    dump.write_bytes(b"")
    (backup / "admin_blog.conf").write_text(
        "admin_blog=*A1B2C3D4E5F60718293A4B5C6D7E8F90A1B2C3D4\n", encoding="utf-8"
    )

    creds = da_import._da_db_credentials(dump, tmp_path)

    assert creds["password"] == ""
    assert creds["password_hash"].startswith("*")


def test_da_db_credentials_returns_empty_without_a_conf(tmp_path):
    dump = tmp_path / "admin_none.sql"
    dump.write_text("-- dump", encoding="utf-8")

    assert da_import._da_db_credentials(dump, tmp_path) == {"password": "", "password_hash": ""}


def test_importer_reuses_the_password_from_the_site_config():
    """Regression: the importer always generated a random database password and
    relied on rewriting wp-config.php. Wherever that rewrite missed the config
    the app actually reads, the site was left pointing at a password MariaDB
    had just replaced, and the admin had to reset it by hand."""
    password, reused = da_import._import_db_password(
        {"DB_NAME": "shop", "DB_PASSWORD": "keep-me"}, {"password": "", "password_hash": ""}
    )

    assert (password, reused) == ("keep-me", True)


def test_importer_falls_back_to_the_backup_conf_password():
    password, reused = da_import._import_db_password({}, {"password": "from-conf", "password_hash": ""})

    assert (password, reused) == ("from-conf", True)


def test_importer_generates_a_password_when_the_backup_has_none():
    password, reused = da_import._import_db_password({"DB_NAME": "shop"}, {"password": "", "password_hash": "*AB"})

    assert reused is False
    assert len(password) >= 16

def _wp_config(password: str, db: str = "admin_shop", user: str = "admin_shop") -> str:
    return (
        "<?php\n"
        f"define('DB_NAME', '{db}');\n"
        f"define('DB_USER', '{user}');\n"
        f"define('DB_PASSWORD', '{password}');\n"
        "define('DB_HOST', 'localhost');\n"
    )


def test_wp_config_is_found_one_level_above_the_document_root(tmp_path):
    """WordPress reads wp-config.php from the parent of the webroot too, and DA
    accounts do use that layout. Missing it there is what left the imported
    database on a password the site never learned about."""
    site = tmp_path / "domains" / "example.test"
    public = site / "public_html"
    public.mkdir(parents=True)
    (site / "wp-config.php").write_text(_wp_config("above-root"), encoding="utf-8")

    assert da_import._parse_app_db_config(public)["DB_PASSWORD"] == "above-root"


def test_wp_config_is_found_in_a_subfolder(tmp_path):
    public = tmp_path / "public_html"
    (public / "shop").mkdir(parents=True)
    (public / "shop" / "wp-config.php").write_text(_wp_config("in-subfolder"), encoding="utf-8")

    assert da_import._parse_app_db_config(public)["DB_PASSWORD"] == "in-subfolder"


def test_config_scan_skips_heavy_content_directories(tmp_path):
    """wp-content holds themes and plugins, not the site's own credentials.
    Walking it on a big site costs far more than it can ever find."""
    public = tmp_path / "public_html"
    buried = public / "wp-content" / "plugins"
    buried.mkdir(parents=True)
    (buried / "wp-config.php").write_text(_wp_config("should-not-be-read"), encoding="utf-8")

    assert da_import._parse_app_db_config(public) == {}


def test_update_reaches_the_config_above_the_document_root(tmp_path):
    site = tmp_path / "domains" / "example.test"
    public = site / "public_html"
    public.mkdir(parents=True)
    (site / "wp-config.php").write_text(_wp_config("old"), encoding="utf-8")

    da_import._update_app_db_config(public, "admin_shop", "admin_shop", "new-pass")

    assert "new-pass" in (site / "wp-config.php").read_text(encoding="utf-8")


def test_update_leaves_another_app_in_a_subfolder_alone(tmp_path):
    """A second app in a subfolder has its own database. Writing these
    credentials into it would take that app down rather than fix anything."""
    public = tmp_path / "public_html"
    (public / "crm").mkdir(parents=True)
    other = public / "crm" / ".env"
    other.write_text("DB_DATABASE=admin_crm\nDB_PASSWORD=crm-secret\n", encoding="utf-8")

    da_import._update_app_db_config(public, "admin_shop", "admin_shop", "new-pass")

    assert other.read_text(encoding="utf-8") == "DB_DATABASE=admin_crm\nDB_PASSWORD=crm-secret\n"


def test_da_conf_is_found_under_mysql_as_well(tmp_path):
    mysql_dir = tmp_path / "mysql"
    mysql_dir.mkdir()
    dump = mysql_dir / "admin_shop.sql.gz"
    dump.write_bytes(b"")
    (mysql_dir / "admin_shop.conf").write_text("passwd=from-mysql-dir\n", encoding="utf-8")

    assert da_import._da_db_credentials(dump, tmp_path)["password"] == "from-mysql-dir"


# ---------------------------------------------------------------------------
# DirectAdmin subdomains
# ---------------------------------------------------------------------------

def test_discover_subdomains_reads_the_newer_per_domain_layout(tmp_path):
    listing = tmp_path / "backup" / "example.test" / "subdomain.list"
    listing.parent.mkdir(parents=True)
    listing.write_text("blog\nshop\n\n# a comment\nBAD_LABEL\ndev\n", encoding="utf-8")

    assert da_import._discover_subdomains(tmp_path, "example.test") == ["blog", "shop", "dev"]


def test_discover_subdomains_reads_the_older_flat_layout(tmp_path):
    listing = tmp_path / "backup" / "example.test.subdomains"
    listing.parent.mkdir(parents=True)
    listing.write_text("staging\napi\n", encoding="utf-8")

    assert da_import._discover_subdomains(tmp_path, "example.test") == ["staging", "api"]


def test_discover_subdomains_is_empty_without_a_list(tmp_path):
    (tmp_path / "backup").mkdir()
    assert da_import._discover_subdomains(tmp_path, "example.test") == []


def test_relocate_da_subdomain_sources_moves_docroots_out_of_the_parent(tmp_path):
    public = tmp_path / "domains" / "example.test" / "public_html"
    (public / "blog").mkdir(parents=True)
    (public / "blog" / "index.php").write_text("<?php // blog", encoding="utf-8")
    (public / "shop").mkdir()
    (public / "shop" / "index.html").write_text("shop", encoding="utf-8")
    (public / "index.php").write_text("<?php // parent", encoding="utf-8")

    listing = tmp_path / "backup" / "example.test" / "subdomain.list"
    listing.parent.mkdir(parents=True)
    listing.write_text("blog\nshop\ndeleted\n", encoding="utf-8")

    sources = da_import._relocate_da_subdomain_sources(tmp_path, ["example.test"])

    # each declared label is present; a label with no files maps to None
    assert set(sources) == {"blog.example.test", "shop.example.test", "deleted.example.test"}
    assert sources["deleted.example.test"] is None

    # the subdomain docroots left the parent's public_html ...
    assert not (public / "blog").exists()
    assert not (public / "shop").exists()
    assert (public / "index.php").exists()

    # ... and moved into the opanel-ent-owned staging sibling with their files intact
    assert sources["blog.example.test"] == tmp_path / "_opanel_subdomains" / "blog.example.test"
    assert (sources["blog.example.test"] / "index.php").read_text(encoding="utf-8") == "<?php // blog"
    assert (sources["shop.example.test"] / "index.html").read_text(encoding="utf-8") == "shop"


def test_relocate_da_subdomain_sources_no_op_without_subdomains(tmp_path):
    public = tmp_path / "domains" / "example.test" / "public_html"
    public.mkdir(parents=True)
    (tmp_path / "backup").mkdir()

    assert da_import._relocate_da_subdomain_sources(tmp_path, ["example.test"]) == {}
    assert not (tmp_path / "_opanel_subdomains").exists()


def test_helper_has_deferred_vhost_and_waf_subcommands_that_skip_the_reload():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    # deferred vhost write: same case, guarded reload
    assert "ols-vhost-write|ols-vhost-write-defer)" in helper
    assert 'if [[ "$cmd" == "ols-vhost-write" ]]; then' in helper
    # deferred WAF save
    assert "waf-site-save-defer)" in helper
    assert 'save_waf_site_rules "$1" defer' in helper
    waf_fn = helper.split("save_waf_site_rules() {", 1)[1].split("\n}", 1)[0]
    assert '[[ "$defer" == "defer" ]] || restart_openlitespeed' in waf_fn


def test_import_refuses_to_delete_a_live_panel_user(tmp_path, monkeypatch):
    """Importing used to delete a same-named user and all of their websites."""
    from app.models.entities import User, Website
    from app.core.database import SessionLocal
    from app.core.security import hash_password

    db = SessionLocal()
    try:
        existing = User(username="clash", email="clash@example.test",
                        hashed_password=hash_password("PasswordLongEnough1"), role="end_user")
        db.add(existing)
        db.commit()

        monkeypatch.setattr(da_import, "_find_backup_root", lambda d: d)
        monkeypatch.setattr(da_import, "_extract_nested_domain_archives", lambda r: None)
        monkeypatch.setattr(da_import, "_discover_domains", lambda r: ["clash.test"])
        monkeypatch.setattr(da_import, "_relocate_da_subdomain_sources", lambda r, d: [])
        monkeypatch.setattr(da_import, "_discover_username", lambda r, n: ("clash", "clash@example.test"))

        with pytest.raises(da_import.DAImportError, match="Already on this server"):
            da_import._process_archive(tmp_path, "clash.tar.gz", db, [])

        assert db.query(User).filter(User.username == "clash").first() is not None
    finally:
        db.query(Website).filter(Website.domain == "clash.test").delete()
        db.query(User).filter(User.username == "clash").delete()
        db.commit()
        db.close()
