import pytest
from pathlib import Path

from app.services import waf

HELPER_SCRIPT = Path(__file__).resolve().parents[3] / "installer" / "files" / "opanel-ent-helper.sh"


def test_default_rules_cover_the_app_stacks_plus_server_and_injection():
    definitions = waf.default_rule_definitions()

    assert {rule["category"] for rule in definitions} == {
        "Laravel", "PHP", "WordPress", "Server", "Injection",
    }
    # Everything is on out of the box except XSS, which stays opt-in because
    # its predecessor was dropped on purpose -- see test_waf_rules.py.
    off = {rule["id"] for rule in definitions if not rule["enabled_default"]}
    assert off == {"xss"}


def test_legacy_heavy_rule_ids_are_mapped_or_ignored():
    assert waf.validate_enabled_rule_ids([
        "general-sensitive-files",
        "general-path-traversal",
        "general-sqli",
        "general-xss",
        "general-command-injection",
    ]) == ["php-sensitive-files", "php-path-traversal", "php-runtime-probes"]


def test_render_site_rules_only_includes_selected_wordpress_rule():
    content = waf.render_site_rules("example.com", ["wordpress-sensitive-files"])

    assert "id:1001101" in content
    assert "id:1001102" not in content
    assert "id:1001201" not in content
    assert "id:1001301" not in content


def test_render_site_rules_includes_laravel_and_php_rules():
    content = waf.render_site_rules("example.com", ["laravel-sensitive-files", "php-sensitive-files"])

    assert "id:1001201" in content
    assert "id:1001301" in content


def test_wordpress_xmlrpc_rule_blocks_xmlrpc():
    content = waf.render_site_rules("example.com", ["wordpress-xmlrpc-author-scan"])

    assert "id:1001102" in content
    assert "/xmlrpc\\.php" in content
    assert "id:1001103" in content


def test_wordpress_wp2shell_rules_block_batch_paths():
    content = waf.render_site_rules("example.com", ["wordpress-wp2shell"])

    assert "id:1000001" in content
    assert "id:1000002" in content
    assert "/wp-json/batch/v1" in content
    assert "/batch/v1" in content


def test_default_rules_do_not_scan_request_body_or_headers():
    content = waf.render_site_rules("example.com", [rule["id"] for rule in waf.DEFAULT_RULES])

    assert "REQUEST_BODY" not in content
    assert "REQUEST_HEADERS" not in content


def test_unknown_rule_ids_are_rejected():
    with pytest.raises(ValueError, match="Unknown WAF rule"):
        waf.validate_enabled_rule_ids(["joomla-sensitive-files"])


def test_parse_access_log_line_marks_xmlrpc_403_as_blocked():
    line = '36.67.165.162 - - [27/Jul/2026:02:03:54 +0000] "POST /xmlrpc.php HTTP/1.1" 403 441 "-" "Jetpack by WordPress.com"'

    entry = waf.parse_access_log_line(line, "example.com")

    assert entry["verdict"] == "block"
    assert entry["method"] == "POST"
    assert entry["path"] == "/xmlrpc.php"
    assert entry["status"] == 403
    assert entry["reason"] == "Block WordPress XML-RPC"


def test_parse_access_log_line_marks_200_as_allowed():
    line = '47.82.49.89 - - [27/Jul/2026:10:39:39 +0000] "GET / HTTP/1.1" 200 1200 "-" "Mozilla/5.0"'

    entry = waf.parse_access_log_line(line, "example.com")

    assert entry["verdict"] == "allow"
    assert entry["reason"] == "Allowed"


def test_parse_access_log_line_marks_wp2shell_paths_as_blocked():
    line = '198.51.100.23 - - [27/Jul/2026:10:40:12 +0000] "POST /wp-json/batch/v1 HTTP/1.1" 403 0 "-" "Mozilla/5.0"'

    entry = waf.parse_access_log_line(line, "example.com")

    assert entry["verdict"] == "block"
    assert entry["reason"] == "Block wp2shell Path"


def test_sync_site_rules_uses_the_deferred_subcommand_when_asked(monkeypatch):
    seen = {}

    def fake_privileged(command, helper_args=None, check=False, fallback=None, **kwargs):
        seen["command"] = command
        return waf.CommandResult(command, 0, "", "")

    monkeypatch.setattr(waf.shell, "privileged", fake_privileged)

    waf.sync_site_rules("example.com", set(), "")
    assert seen["command"] == "waf-site-save"

    waf.sync_site_rules("example.com", set(), "", defer_reload=True)
    assert seen["command"] == "waf-site-save-defer"


def test_access_log_report_filters_and_paginates(monkeypatch):
    payload = "\n".join([
        "opanel_ent_LOG_PATH=example.com\t/var/log/openlitespeed/example.com.access.log",
        'example.com\t47.82.49.89 - - [27/Jul/2026:10:39:39 +0000] "GET / HTTP/1.1" 200 1200 "-" "Mozilla/5.0"',
        'example.com\t36.67.165.162 - - [27/Jul/2026:10:39:40 +0000] "POST /xmlrpc.php HTTP/1.1" 403 441 "-" "Jetpack by WordPress.com"',
    ])

    def fake_privileged(command, helper_args=None, check=False, fallback=None, **kwargs):
        assert command == "waf-access-log-read"
        assert helper_args == ["5000", "example.com"]
        return waf.CommandResult(command, 0, payload, "")

    monkeypatch.setattr(waf.shell, "privileged", fake_privileged)

    report = waf.access_log_report(["example.com"], domain="example.com", verdict="block", limit=10)

    assert report["total"] == 1
    assert report["entries"][0]["path"] == "/xmlrpc.php"
    assert report["entries"][0]["reason"] == "Block WordPress XML-RPC"


def test_modsec_base_conf_enables_body_access_for_phase2_rules():
    """libmodsecurity3 skips all of phase 2 when SecRequestBodyAccess is off.
    With it off, the wp2shell block (1000001/1000002, phase:2), the ?rest_route
    smuggling block and author enumeration never fired on any site -- verified
    against a live vhost: 404 with it off, 403 with it on.

    The limits have to come after the distro modsecurity.conf include, which
    ships SecRequestBodyLimitAction Reject at 13 MB and would break large
    media/plugin uploads.
    """
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    base = helper.split("write_modsec_base_conf()", 1)[1].split("\n}", 1)[0]

    assert 'echo "SecRequestBodyAccess On"' in base
    assert 'echo "SecRequestBodyAccess Off"' not in helper
    assert 'echo "SecRequestBodyLimitAction ProcessPartial"' in base
    assert 'echo "SecRequestBodyNoFilesLimit 1048576"' in base

    include_at = base.index("/etc/modsecurity/modsecurity.conf")
    limit_at = base.index("SecRequestBodyLimit 134217728")
    assert include_at < limit_at, "opanel-ent limits must override the distro include"


def test_wp2shell_and_arg_rules_are_phase2():
    """Guards the reason the fix above matters: these rules are phase:2, so
    they are exactly the ones body access being off switched off."""
    by_id = {r["id"]: r["rules"] for r in waf.DEFAULT_RULES}
    assert "id:1000001,phase:2" in by_id["wordpress-wp2shell"]
    assert "id:1000002,phase:2" in by_id["wordpress-wp2shell"]
    assert "id:1001103,phase:2" in by_id["wordpress-xmlrpc-author-scan"]


# --- bad bot blocking ------------------------------------------------------
class _Site:
    """Minimal stand-in for a Website row."""
    def __init__(self, **kw):
        self.domain = kw.pop("domain", "example.test")
        self.waf_default_rules = kw.pop("waf_default_rules", "[]")
        self.waf_custom_rules = kw.pop("waf_custom_rules", "")
        self.waf_bot_enabled = kw.pop("waf_bot_enabled", True)
        self.waf_bot_extra = kw.pop("waf_bot_extra", "")
        for k, v in kw.items():
            setattr(self, k, v)


def test_empty_bot_list_emits_no_rule_at_all():
    """The shipped list is empty, so nothing may change on any site until an
    admin fills it in."""
    out = waf.render_site_rules("example.test", [], "", global_bots=[])
    assert "bad bot" not in out.lower()
    assert waf.BAD_BOT_RULE_ID not in out


def test_bad_bot_rule_blocks_the_pattern_and_is_phase_1():
    out = waf.render_site_rules("example.test", [], "", global_bots=["AhrefsBot", "SemrushBot"])
    assert "AhrefsBot|SemrushBot" in out
    assert f"id:{waf.BAD_BOT_RULE_ID},phase:1,deny,status:403" in out
    assert "REQUEST_HEADERS:User-Agent" in out


def test_the_list_is_taken_literally_including_well_known_crawlers():
    """No allow-list and no protected set: whatever the admin lists is blocked.
    Unblocking is done by removing the entry, not by a second opposing list."""
    out = waf.render_bad_bot_rules(["AhrefsBot", "Googlebot"])
    assert "AhrefsBot|Googlebot" in out
    assert "!@rx" not in out          # no exception rule at all
    assert ",chain" not in out


def test_the_bot_block_is_a_single_flat_rule():
    out = waf.render_bad_bot_rules(["AhrefsBot"])
    assert sum(1 for line in out.splitlines() if line.strip().startswith("SecRule")) == 1


def test_patterns_are_escaped_so_a_stray_metacharacter_cannot_break_the_config():
    out = waf.render_bad_bot_rules(["evil.bot", "a+b", "x(y"])
    assert r"evil\.bot" in out
    assert r"a\+b" in out
    assert r"x\(y" in out


def test_pattern_validation_rejects_what_would_corrupt_the_rule_file():
    for bad in ['say"hi', "back" + chr(92) + "slash", "ctrl" + chr(1) + "char"]:
        with pytest.raises(ValueError):
            waf.normalize_bot_patterns([bad])
    with pytest.raises(ValueError):
        waf.normalize_bot_patterns(["x" * (waf.MAX_BOT_PATTERN_LEN + 1)])
    with pytest.raises(ValueError):
        waf.normalize_bot_patterns([f"bot{i}" for i in range(waf.MAX_BOT_PATTERNS + 1)])


def test_pattern_parsing_handles_blobs_comments_and_duplicates():
    got = waf.normalize_bot_patterns("AhrefsBot\n# a comment\n\nSemrushBot, ahrefsbot\n")
    assert got == ["AhrefsBot", "SemrushBot"]  # comment dropped, case-insensitive dedupe


def test_a_site_can_turn_bot_blocking_off_or_add_its_own():
    off = waf.render_site_rules("example.test", [], "", bot_blocking=False, global_bots=["AhrefsBot"])
    assert waf.BAD_BOT_RULE_ID not in off

    extra = waf.render_site_rules(
        "example.test", [], "", global_bots=["AhrefsBot"], bot_extra=["ScrapyBot"]
    )
    assert "AhrefsBot" in extra and "ScrapyBot" in extra


def test_website_bot_settings_read_the_row():
    site = _Site(waf_bot_enabled=False, waf_bot_extra="A, B")
    assert waf.website_bot_settings(site) == {
        "bot_blocking_enabled": False,
        "bot_extra": ["A", "B"],
    }
