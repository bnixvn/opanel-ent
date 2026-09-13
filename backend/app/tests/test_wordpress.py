from app.services import wordpress


def test_install_wordpress_writes_wp_config_with_standard_file_mode(monkeypatch, tmp_path):
    calls = []

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(wordpress.shell, "privileged", fake_privileged)
    monkeypatch.setattr(wordpress.site_users, "fix_site_permissions", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(wordpress.settings, "command_dry_run", False)

    wordpress.install_wordpress(
        "example.test",
        {"db_name": "wp", "db_user": "wpuser", "db_password": "strong-db-pass"},
        "Example",
        "admin_user",
        "StrongPass123!",
        "admin@example.test",
        "8.4",
        linux_user="siteuser",
        root_path=str(tmp_path / "example.test"),
    )

    config_writes = [
        helper_args
        for command, helper_args, _kwargs in calls
        if command == "site-file-write" and helper_args[2] == "public_html/wp-config.php"
    ]
    assert config_writes
    assert all(helper_args[3] == "0644" for helper_args in config_writes)


def test_delete_wordpress_binds_rm_site_to_owner_and_site_root(monkeypatch):
    calls = []
    root = "/home/siteuser/example.test"

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(wordpress.shell, "privileged", fake_privileged)

    wordpress.delete_wordpress(root)

    assert calls[0][0] == "rm-site"
    assert calls[0][1][0] == "siteuser"
    assert calls[0][1][1].replace("\\", "/").endswith(root)
    assert calls[0][1][2].replace("\\", "/").endswith(root)
    assert calls[0][2]["fallback"][0:2] == ["rm", "-rf"]
    assert calls[0][2]["fallback"][2].replace("\\", "/").endswith(root)
