from pathlib import Path

import pytest
from fastapi import HTTPException

from app.api import firewall as firewall_api
from app.services import firewall
from app.services.shell import CommandResult


def test_add_blocklist_url_uses_privileged_helper(monkeypatch):
    calls = []

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return CommandResult(helper_command, 0, "Blocklist URL added", "")

    monkeypatch.setattr(firewall.shell, "privileged", fake_privileged)

    result = firewall.add_blocklist_url("https://example.test/list.txt")

    assert result.returncode == 0
    assert calls[0][0] == "iptables-blocklist-add"
    assert calls[0][1] == ["https://example.test/list.txt"]
    assert calls[0][2]["check"] is False


def test_delete_blocklist_url_uses_privileged_helper(monkeypatch):
    calls = []

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return CommandResult(helper_command, 0, "Blocklist URL removed", "")

    monkeypatch.setattr(firewall.shell, "privileged", fake_privileged)

    result = firewall.delete_blocklist_url("https://example.test/list.txt")

    assert result.returncode == 0
    assert calls[0][0] == "iptables-blocklist-delete"
    assert calls[0][1] == ["https://example.test/list.txt"]
    assert calls[0][2]["check"] is False


def test_firewall_api_result_raises_clear_error_on_command_failure():
    with pytest.raises(HTTPException) as exc:
        firewall_api._result(CommandResult("iptables-deny-ip", 1, "", "permission denied"))

    assert exc.value.status_code == 400
    assert exc.value.detail == "permission denied"


def test_block_ip_ensures_rule_store_before_writing(monkeypatch, tmp_path):
    calls = []
    rules_file = tmp_path / "firewall" / "rules.json"

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return CommandResult(helper_command, 0, "", "")

    monkeypatch.setattr(firewall, "RULES_FILE", rules_file)
    monkeypatch.setattr(firewall.shell, "privileged", fake_privileged)

    result = firewall.block_ip("198.51.100.10")

    assert result.returncode == 0
    assert calls[0][0] == "iptables-rules-store-ensure"
    assert any(call[0] == "iptables-run" for call in calls)
    assert rules_file.exists()


def test_blocklist_url_requires_http_url():
    with pytest.raises(ValueError, match="URL must start"):
        firewall.add_blocklist_url("file:///tmp/list.txt")

    with pytest.raises(ValueError, match="URL must start"):
        firewall.delete_blocklist_url("file:///tmp/list.txt")


def test_helper_blocklist_apply_ensures_iptables_jump():
    helper = Path(__file__).resolve().parents[3] / "installer" / "files" / "opanel-ent-helper.sh"
    content = helper.read_text(encoding="utf-8")
    start = content.index("firewall_blocklist_apply()")
    end = content.index("firewall_blocklist_status()", start)
    block = content[start:end]

    assert "iptables -N OPANEL_ENT_BLOCKLIST" in block
    assert "iptables -C INPUT -j OPANEL_ENT_BLOCKLIST" in block
    assert "ip6tables -C INPUT -j OPANEL_ENT_BLOCKLIST" in block
    assert "--match-set \"$BLOCKLIST_IPSET_V4\" src -j DROP" in block


def test_parse_iptables_status_and_open_ports(monkeypatch):
    monkeypatch.setattr(firewall.settings, "panel_port", 2222)

    rules = firewall.parse_numbered_rules(
        "Chain OPANEL_ENT_INPUT (1 references)\n"
        "num  target     prot opt source               destination\n"
        "1    ACCEPT     6    --  0.0.0.0/0            0.0.0.0/0            tcp dpt:22 /* opanel-ent:PanelZone */\n"
        "2    ACCEPT     6    --  0.0.0.0/0            0.0.0.0/0            tcp dpt:2222 /* opanel-ent:PanelZone */\n"
        "3    ACCEPT     6    --  0.0.0.0/0            0.0.0.0/0            multiport dports 465,587 /* opanel-ent:PanelZone */\n"
        "Chain OPANEL_ENT_USER (1 references)\n"
        "num  target     prot opt source               destination\n"
        "1    ACCEPT     17   --  203.0.113.10         0.0.0.0/0            udp dpt:53 /* opanel-ent:UserZone */\n"
        "2    DROP       tcp  --  198.51.100.0/24      0.0.0.0/0            tcp dpt:443 /* opanel-ent:UserZone */\n"
    )

    assert [rule["to"] for rule in rules] == ["22/tcp", "2222/tcp", "465,587/tcp", "53/udp", "443/tcp"]
    assert rules[0]["protected"] is True
    assert rules[3]["from"] == "203.0.113.10"

    open_ports = firewall.open_ports_from_rules(rules)

    assert [f"{item['port']}/{item['protocol']}" for item in open_ports] == [
        "22/tcp",
        "465/tcp",
        "587/tcp",
        "2222/tcp",
        "53/udp",
    ]
    assert open_ports[0]["zone"] == "PanelZone"
    assert open_ports[-1]["source"] == "203.0.113.10"


def test_allow_port_does_not_duplicate_a_default_port(monkeypatch, tmp_path):
    """22, 25, 80, 443, 465, 587 and the panel port are opened for every
    install. Adding one from the panel used to append a second rule for the
    same port, so the firewall list showed it twice with no way to tell which
    rule was the admin's."""
    monkeypatch.setattr(firewall, "RULES_FILE", tmp_path / "rules.json")
    calls = []
    monkeypatch.setattr(firewall, "_iptables", lambda *a, **kw: calls.append(a))
    monkeypatch.setattr(firewall, "_ip6tables", lambda *a, **kw: calls.append(a))

    result = firewall.allow_port(587)

    assert "already open by default" in result.stdout
    assert calls == []
    assert firewall._read_rules() == []


def test_allow_port_is_idempotent_for_a_custom_port(monkeypatch, tmp_path):
    monkeypatch.setattr(firewall, "RULES_FILE", tmp_path / "rules.json")
    monkeypatch.setattr(firewall, "_iptables", lambda *a, **kw: None)
    monkeypatch.setattr(firewall, "_ip6tables", lambda *a, **kw: None)

    first = firewall.allow_port(8443)
    second = firewall.allow_port(8443)

    assert first.stdout == "Port allowed"
    assert "already allowed" in second.stdout
    assert len(firewall._read_rules()) == 1
