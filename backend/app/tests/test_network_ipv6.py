import pytest

from pathlib import Path

from app.services import network, panel_settings
from app.services.shell import CommandResult


IP_OUTPUT = "\n".join(
    [
        "1: lo    inet 127.0.0.1/8 scope host lo",
        "2: eth0    inet 203.0.113.10/24 brd 203.0.113.255 scope global eth0",
        "2: eth0    inet 203.0.113.11/24 brd 203.0.113.255 scope global secondary eth0",
        "2: eth0    inet6 2001:db8:1234::10/64 scope global",
        "2: eth0    inet6 fe80::5054:ff:fe12:3456/64 scope link",
    ]
)


def test_detect_addresses_keeps_only_routable_addresses(monkeypatch):
    """Loopback and link-local answer no traffic from the internet, so listing
    them next to the real address would just invite an admin to publish one."""
    monkeypatch.setattr(
        network.shell, "run", lambda *a, **kw: CommandResult("ip", 0, IP_OUTPUT, "")
    )

    addresses = network.detect_addresses()

    assert addresses["ipv4"] == ["203.0.113.10", "203.0.113.11"]
    assert addresses["ipv6"] == ["2001:db8:1234::10"]


def test_detect_addresses_falls_back_to_proc_when_ip_is_missing(monkeypatch, tmp_path):
    """Some minimal images have no iproute2; the kernel still lists IPv6."""
    if_inet6 = tmp_path / "if_inet6"
    if_inet6.write_text(
        "\n".join(
            [
                "20010db8123400000000000000000010 02 40 00 00     eth0",
                "fe800000000000005054fffffe123456 02 40 20 80     eth0",
                "00000000000000000000000000000001 01 80 10 80       lo",
            ]
        ),
        encoding="utf-8",
    )
    monkeypatch.setattr(network, "_ip_output", lambda: "")
    monkeypatch.setattr(network, "_routed_source_address", lambda family, probe: "")
    monkeypatch.setattr(network, "IF_INET6", if_inet6)

    assert network.detect_addresses()["ipv6"] == ["2001:db8:1234::10"]


def _isolate_settings(monkeypatch, tmp_path):
    monkeypatch.setattr(panel_settings, "SETTINGS_DIR", tmp_path)
    monkeypatch.setattr(panel_settings, "SETTINGS_FILE", tmp_path / "panel-settings.json")


def test_ipv6_defaults_to_on_when_the_server_has_an_address(monkeypatch, tmp_path):
    """A server built with IPv6 should serve it without anyone opting in."""
    _isolate_settings(monkeypatch, tmp_path)
    monkeypatch.setattr(
        network, "detect_addresses", lambda: {"ipv4": ["203.0.113.10"], "ipv6": ["2001:db8::10"]}
    )

    status = panel_settings.network_status()

    assert status["ipv6_available"] is True
    assert status["ipv6_enabled"] is True
    assert status["ipv6_configured"] is False


def test_ipv6_defaults_to_off_without_an_address(monkeypatch, tmp_path):
    _isolate_settings(monkeypatch, tmp_path)
    monkeypatch.setattr(network, "detect_addresses", lambda: {"ipv4": ["203.0.113.10"], "ipv6": []})

    status = panel_settings.network_status()

    assert (status["ipv6_available"], status["ipv6_enabled"]) == (False, False)


def test_an_address_added_later_is_picked_up(monkeypatch, tmp_path):
    """Detection is live: a block added at the provider months after install
    turns the switch on by itself, because nobody ever chose otherwise."""
    _isolate_settings(monkeypatch, tmp_path)
    monkeypatch.setattr(network, "detect_addresses", lambda: {"ipv4": [], "ipv6": []})
    assert panel_settings.network_status()["ipv6_enabled"] is False

    monkeypatch.setattr(network, "detect_addresses", lambda: {"ipv4": [], "ipv6": ["2001:db8::10"]})
    assert panel_settings.network_status()["ipv6_enabled"] is True


def test_an_explicit_choice_survives_a_still_present_address(monkeypatch, tmp_path):
    _isolate_settings(monkeypatch, tmp_path)
    monkeypatch.setattr(network, "detect_addresses", lambda: {"ipv4": [], "ipv6": ["2001:db8::10"]})
    monkeypatch.setattr(network, "apply_ipv6", lambda enabled: "applied")

    panel_settings.set_ipv6(False)
    status = panel_settings.network_status()

    assert status["ipv6_enabled"] is False
    assert status["ipv6_configured"] is True


def test_enabling_ipv6_without_an_address_is_refused(monkeypatch, tmp_path):
    _isolate_settings(monkeypatch, tmp_path)
    monkeypatch.setattr(network, "detect_addresses", lambda: {"ipv4": ["203.0.113.10"], "ipv6": []})

    with pytest.raises(ValueError, match="no global IPv6"):
        panel_settings.set_ipv6(True)


def test_a_failed_apply_rolls_the_stored_flag_back(monkeypatch, tmp_path):
    """The listeners are what actually serve IPv6. If they could not be
    rebuilt, a stored flag saying otherwise would misreport the server."""
    _isolate_settings(monkeypatch, tmp_path)
    monkeypatch.setattr(network, "detect_addresses", lambda: {"ipv4": [], "ipv6": ["2001:db8::10"]})

    def boom(enabled):
        raise RuntimeError("lshttpd restart failed")

    monkeypatch.setattr(network, "apply_ipv6", boom)

    with pytest.raises(RuntimeError):
        panel_settings.set_ipv6(False)

    assert panel_settings.network_status()["ipv6_configured"] is False
    assert panel_settings.network_status()["ipv6_enabled"] is True


def test_panel_falls_back_to_ipv4_when_the_ipv6_bind_cannot_work(monkeypatch):
    """PANEL_BIND_HOST=:: outlives the address family it was written for --
    a rebuilt VPS, ipv6.disable=1 -- and the panel is the tool used to fix
    that, so it has to start anyway."""
    from app import server

    monkeypatch.setattr(server.socket, "has_ipv6", False)

    assert server._bindable_host("::") == "0.0.0.0"
    assert server._bindable_host("0.0.0.0") == "0.0.0.0"


def test_ipv6_bind_still_serves_ipv4_clients():
    """asyncio sets IPV6_V6ONLY on the sockets it creates, so letting uvicorn
    bind :: answered IPv6 and refused every IPv4 client -- including the admin
    trying to switch IPv6 back off. The panel opens the socket itself."""
    import socket as socket_module

    from app import server

    sock = server.listen_socket("::", 0)
    try:
        assert sock.family == socket_module.AF_INET6
        assert sock.getsockopt(socket_module.IPPROTO_IPV6, socket_module.IPV6_V6ONLY) == 0
    finally:
        sock.close()


def test_ipv6_bind_falls_back_to_ipv4_when_the_family_is_gone(monkeypatch):
    from app import server

    real_socket = server.socket.socket

    def fail_on_ipv6(family, *args, **kwargs):
        if family == server.socket.AF_INET6:
            raise OSError("Address family not supported by protocol")
        return real_socket(family, *args, **kwargs)

    monkeypatch.setattr(server.socket, "socket", fail_on_ipv6)

    sock = server.listen_socket("::", 0)
    try:
        assert sock.family == server.socket.AF_INET
    finally:
        sock.close()


def test_settings_accept_the_installer_written_bind_host(monkeypatch):
    """The installer writes PANEL_BIND_HOST into .env, and Settings forbids
    unknown keys -- so a field it does not declare stops the whole backend from
    importing, taking migrations and the API down with it."""
    from app.core.config import Settings

    monkeypatch.setenv("PANEL_BIND_HOST", "::")

    assert Settings(_env_file=None).panel_bind_host == "::"


def test_every_env_key_the_installer_writes_is_declared():
    """Guard for the whole class of failure above: any variable the installer
    puts in .env that Settings does not declare kills every process that
    imports the config, so the panel cannot even be used to undo it."""
    import re

    from app.core.config import Settings

    install_sh = Path(__file__).resolve().parents[3] / "installer" / "install.sh"
    template = install_sh.read_text(encoding="utf-8").split("cat > .env <<ENV", 1)[1]
    env_block = template.split("\nENV\n", 1)[0]
    written = {match.group(1) for match in re.finditer(r"(?m)^([A-Z][A-Z0-9_]+)=", env_block)}
    declared = {name.upper() for name in Settings.model_fields}

    assert written, "could not read the .env template out of install.sh"
    assert written <= declared, f"undeclared in Settings: {sorted(written - declared)}"
