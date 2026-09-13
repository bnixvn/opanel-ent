from app.services import php


def test_timestamp_validation_is_always_on():
    """Regression: validate_timestamps was derived as `1 if revalidate > 0 else 0`,
    so the default revalidate_freq of 0 -- which in PHP means "check on every
    request" -- silently turned validation off instead. An edited file was then
    never picked up while any LSAPI worker for the pool stayed alive, which on a
    site with steady traffic is forever."""
    cfg = php.recommend_php_config()
    assert cfg["opcache_validate_timestamps"] == 1


def test_opcache_stays_enabled():
    assert php.recommend_php_config()["opcache_enable"] == 1


def test_revalidate_freq_is_not_negative():
    assert php.recommend_php_config()["opcache_revalidate_freq"] >= 0


def test_rendered_ini_turns_validation_on():
    ini = php.render_php_ini()
    assert "opcache.validate_timestamps = 1" in ini.replace("  ", " ").replace("  ", " ")


def test_rendered_ini_still_enables_opcache():
    ini = php.render_php_ini()
    assert "opcache.enable" in ini


def test_rendered_ini_switches_cli_with_the_web_setting():
    """opcache.enable_cli follows the same switch, so wp-cli and cron runs do
    not quietly diverge from web requests."""
    ini = php.render_php_ini()
    assert "opcache.enable_cli    = 1" in ini

    cfg = php.recommend_php_config()
    cfg["opcache_enable"] = 0
    off = php.render_php_ini(cfg)
    assert "opcache.enable        = 0" in off
    assert "opcache.enable_cli    = 0" in off


def test_apply_tuning_keeps_opcache_off_when_the_operator_disabled_it(monkeypatch):
    """Regression: apply_php_tuning rendered the recommendation verbatim, and
    the recommendation always says opcache_enable=1, so auto-tuning a version
    the operator had switched off turned OPcache back on."""
    written: dict[str, str] = {}

    monkeypatch.setattr(php.settings, "command_dry_run", False)
    monkeypatch.setattr(php, "list_installed_php", lambda: ["8.3", "8.4"])
    monkeypatch.setattr(php, "current_opcache_enabled", lambda ver: ver != "8.4")
    monkeypatch.setattr(
        php.shell,
        "privileged",
        lambda *args, helper_args=None, input="", **kwargs: written.update(
            {helper_args[0]: input}
        ),
    )

    php.apply_php_tuning()

    assert "opcache.enable        = 1" in written["8.3"]
    assert "opcache.enable        = 0" in written["8.4"]
    assert "opcache.enable_cli    = 0" in written["8.4"]
