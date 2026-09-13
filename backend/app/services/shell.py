"""
Shell execution layer.

Two trust levels:
- shell.run([...]):  runs as the API user (non-root after install).
- shell.privileged([...]): runs the opanel-ent-helper trampoline through sudo,
  so only whitelisted operations (defined in /usr/local/sbin/opanel-ent-helper)
  can ever execute as root.

Setting opanel_ent_USE_HELPER=false in dev/test makes privileged() fall back to
direct execution, so local development without the helper still works.
"""

import os
import shlex
import subprocess
from dataclasses import dataclass
from typing import List, Optional

from app.core.config import settings


HELPER_PATH = "/usr/local/sbin/opanel-ent-helper"


def _use_helper() -> bool:
    flag = os.environ.get("opanel_ent_USE_HELPER")
    if flag is not None:
        return flag.lower() in {"1", "true", "yes", "on"}
    # Default: use helper when running as a non-root system user in production.
    geteuid = getattr(os, "geteuid", None)
    return bool(geteuid and geteuid() != 0 and os.path.exists(HELPER_PATH))


@dataclass
class CommandResult:
    command: str
    returncode: int
    stdout: str
    stderr: str


def _redact_output(value: str) -> str:
    if not value:
        return ""
    blocked = ("password", "identified by", "user_pass", "db_password")
    return "\n".join(line for line in value.splitlines() if not any(token in line.lower() for token in blocked))


class ShellRunner:
    def run(
        self,
        args: List[str],
        check: bool = True,
        input: Optional[str] = None,
        sensitive: bool = False,
    ) -> CommandResult:
        """Run a subprocess as the current API user (non-root in production)."""
        return self._exec(list(args), check=check, input=input, sensitive=sensitive)

    def privileged(
        self,
        helper_command: str,
        helper_args: Optional[List[str]] = None,
        check: bool = True,
        input: Optional[str] = None,
        sensitive: bool = False,
        fallback: Optional[List[str]] = None,
    ) -> CommandResult:
        """Run a privileged operation through the opanel-ent-helper sudo trampoline.

        helper_command: subcommand defined in /usr/local/sbin/opanel-ent-helper
        helper_args:    optional arguments passed to that subcommand
        fallback:       alternate command to run when the helper is not
                        installed (development convenience). Only used if
                        opanel_ent_USE_HELPER is false / unset and helper missing.
        """
        helper_args = list(helper_args or [])
        if _use_helper():
            argv = ["sudo", "-n", HELPER_PATH, helper_command, *helper_args]
        elif fallback is not None:
            argv = list(fallback)
        else:
            raise RuntimeError(
                f"opanel-ent-helper is not available and no fallback was provided "
                f"for privileged operation '{helper_command}'"
            )
        return self._exec(argv, check=check, input=input, sensitive=sensitive)

    def privileged_popen(
        self,
        helper_command: str,
        helper_args: Optional[List[str]] = None,
        fallback: Optional[List[str]] = None,
    ) -> subprocess.Popen:
        """Start a privileged operation and stream its stdout line by line.

        privileged() waits for the command to finish, which is fine for the
        short operations that make up most of the panel. A full-filesystem
        malware scan runs for hours, so its progress has to be read while it
        is still running.
        """
        helper_args = list(helper_args or [])
        if _use_helper():
            argv = ["sudo", "-n", HELPER_PATH, helper_command, *helper_args]
        elif fallback is not None:
            argv = list(fallback)
        else:
            raise RuntimeError(
                f"opanel-ent-helper is not available and no fallback was provided "
                f"for privileged operation '{helper_command}'"
            )
        return subprocess.Popen(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )

    def _exec(
        self,
        argv: List[str],
        *,
        check: bool,
        input: Optional[str],
        sensitive: bool,
    ) -> CommandResult:
        quoted = " ".join(shlex.quote(arg) for arg in argv)
        log_command = "[redacted]" if sensitive else quoted
        if settings.command_dry_run:
            return CommandResult(command=log_command, returncode=0, stdout=f"DRY RUN: {log_command}", stderr="")
        completed = subprocess.run(
            argv,
            capture_output=True,
            text=True,
            check=False,
            input=input,
        )
        stdout = _redact_output(completed.stdout) if sensitive else completed.stdout
        stderr = _redact_output(completed.stderr) if sensitive else completed.stderr
        result = CommandResult(log_command, completed.returncode, stdout, stderr)
        if check and completed.returncode != 0:
            raise RuntimeError(f"Command failed: {log_command}\n{stderr}")
        return result


shell = ShellRunner()
