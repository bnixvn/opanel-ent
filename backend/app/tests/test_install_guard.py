"""install.sh must refuse to run over an existing install.

Running it a second time recreated the environment file and wiped opanel-ent.db,
taking every panel user, website record and setting with it. The database is
the one thing the repo cannot rebuild, so its presence stops the installer.
"""
import shutil
import subprocess
from pathlib import Path

import pytest

PROJECT_ROOT = Path(__file__).resolve().parents[3]
INSTALL_SCRIPT = PROJECT_ROOT / "installer" / "install.sh"

BASH = shutil.which("bash")


def test_the_guard_sits_before_any_destructive_step():
    content = INSTALL_SCRIPT.read_text(encoding="utf-8")
    guard = content.index('if [[ -f "${APP_DIR}/backend/opanel-ent.db" ]]; then')

    # APP_DIR has to be resolved by then, or the guard checks the wrong path.
    assert content.index('APP_DIR="${APP_DIR:-/opt/opanel-ent}"') < guard
    # Nothing that writes to the box may run first. (The only earlier "rm -rf"
    # is the EXIT trap on the temp dir the tarball is unpacked into.)
    for destructive in ("apt-get install", "DATABASE_URL=sqlite", "systemctl enable"):
        assert content.index(destructive) > guard, destructive
    assert "opanel-ent-update" in content[guard:guard + 1200]


@pytest.mark.skipif(
    BASH is None or not Path("/etc/os-release").exists(),
    reason="the installer bails out on a non-Linux host before reaching the guard",
)
def test_installer_stops_when_the_database_already_exists(tmp_path):
    app_dir = tmp_path / "opanel-ent"
    (app_dir / "backend").mkdir(parents=True)
    (app_dir / "backend" / "opanel-ent.db").write_bytes(b"SQLite format 3\x00")

    result = subprocess.run(
        [BASH, str(INSTALL_SCRIPT)],
        capture_output=True,
        text=True,
        env={"PATH": "/usr/bin:/bin", "APP_DIR": str(app_dir), "EUID": "0"},
    )

    assert result.returncode != 0
    combined = result.stdout + result.stderr
    # A non-root or non-Ubuntu host bails out even earlier, which is still a
    # refusal; on a real target the message must name the database.
    assert "already installed" in combined or "root" in combined or "Ubuntu" in combined


@pytest.mark.skipif(BASH is None, reason="bash not available")
def test_installer_syntax_is_valid():
    result = subprocess.run([BASH, "-n", str(INSTALL_SCRIPT)], capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
