"""Terminal service - executes commands as website user.

Commands are executed through the opanel-ent-helper runuser trampoline for
per-user isolation. We split commands into argv locally and pass them to the
helper without invoking a shell, so separators, pipes, and globs are treated as
ordinary arguments instead of shell syntax.
"""

import os
import re
import shlex
from dataclasses import dataclass
from pathlib import Path
from typing import Optional, Set

from app.services import site_users
from app.services.shell import shell

# Whitelist of allowed commands for terminal access
ALLOWED_COMMANDS: Set[str] = {
    "php",
    "composer",
    "artisan",
    "wp",
    "node",
    "npm",
    "npx",
    "yarn",
    "git",
    "phpunit",
    "ls",
    "cat",
    "mkdir",
    "rm",
    "cp",
    "mv",
    "chmod",
    "chown",
    "pwd",
    "echo",
    "cd",
    "clear",
    "touch",
    "grep",
    "find",
    "tar",
    "zip",
    "unzip",
    "curl",
    "wget",
    "diff",
    "head",
    "tail",
    "less",
    "du",
    "df",
    "date",
    "whoami",
    "which",
}

# Maximum command line length. This keeps accidental paste storms out of the
# helper boundary while still leaving plenty of room for Composer/NPM flags.
MAX_COMMAND_CHARS = 4096

# Maximum output size in bytes (1MB)
MAX_OUTPUT_BYTES = 1024 * 1024

# Default command timeout in seconds
DEFAULT_TIMEOUT = 30

# Maximum timeout for long-running commands
MAX_TIMEOUT = 120

_PHP_VERSION_RE = re.compile(r"^\d+\.\d+$")


@dataclass
class CommandResult:
    """Result of a terminal command execution."""

    exit_code: int
    stdout: str
    stderr: str


def split_command(command: str) -> list[str]:
    """Split a user command into argv without invoking a shell."""
    command = (command or "").strip()
    if not command:
        raise ValueError("Empty command")
    if len(command) > MAX_COMMAND_CHARS:
        raise ValueError(f"Command is too long; max {MAX_COMMAND_CHARS} characters")
    try:
        parts = shlex.split(command)
    except ValueError as exc:
        raise ValueError(f"Invalid command syntax: {exc}") from exc
    if not parts:
        raise ValueError("Empty command")
    return parts


def is_command_allowed(command: str) -> bool:
    """Check if the command's main executable is in the whitelist.

    Args:
        command: Full command string (e.g., "php artisan migrate")

    Returns:
        True if the command is allowed, False otherwise.
    """
    try:
        parts = split_command(command)
    except ValueError:
        return False
    return parts[0] in ALLOWED_COMMANDS


def resolve_cwd(site_root: str, current_cwd: str, target: str) -> str:
    """Resolve a cd target, keeping the session inside one website root."""
    root = Path(site_root).resolve(strict=False)
    current = Path(current_cwd or site_root).resolve(strict=False)
    target = (target or "").strip()
    if target in {"", "~"}:
        candidate = root
    else:
        raw = Path(target)
        candidate = raw if raw.is_absolute() else current / raw
    resolved = candidate.resolve(strict=False)
    try:
        common = os.path.commonpath([str(root), str(resolved)])
    except ValueError as exc:
        raise ValueError("Path is outside this website") from exc
    if common != str(root):
        raise ValueError("Path is outside this website")
    if not resolved.exists():
        raise ValueError(f"No such directory: {target}")
    if not resolved.is_dir():
        raise ValueError(f"Not a directory: {target}")
    return str(resolved)


def default_cwd(site_root: str) -> str:
    """Return the terminal start directory for a website.

    End-user web files live in public_html. Start there when it exists, while
    still allowing the session to cd back to the website root for framework
    commands like Composer or Artisan.
    """
    root = Path(site_root).resolve(strict=False)
    document_root = site_users.document_root(root).resolve(strict=False)
    if document_root.exists() and document_root.is_dir():
        return str(document_root)
    return str(root)


def _truncate_output(output: str, max_bytes: int = MAX_OUTPUT_BYTES) -> str:
    """Truncate output if it exceeds max_bytes."""
    if len(output.encode("utf-8")) <= max_bytes:
        return output
    # Truncate and add notice
    truncated = output.encode("utf-8")[:max_bytes].decode("utf-8", errors="ignore")
    return truncated + "\n... (output truncated)"


def exec_command(
    linux_user: str,
    command: str,
    cwd: Optional[str] = None,
    timeout: int = DEFAULT_TIMEOUT,
    php_version: Optional[str] = None,
) -> CommandResult:
    """Execute a command as the website user.

    Args:
        linux_user: The Linux username for the website.
        command: The command to execute (e.g., "php artisan migrate").
        cwd: Working directory (defaults to user's home).
        timeout: Maximum execution time in seconds.
        php_version: PHP version to use (e.g. "8.4").  When provided the helper
            will call the matching LSPHP binary (e.g. ``lsphp84/bin/php``) instead of the system default ``php`` binary so
            Composer platform checks pass for the correct version.

    Returns:
        CommandResult with exit_code, stdout, and stderr.

    Raises:
        RuntimeError: If the command is not allowed or execution fails.
    """
    if not linux_user:
        return CommandResult(
            exit_code=1,
            stdout="",
            stderr="Website has no runtime user configured",
        )

    try:
        argv = split_command(command)
    except ValueError as exc:
        return CommandResult(exit_code=2, stdout="", stderr=str(exc))

    if argv[0] not in ALLOWED_COMMANDS:
        return CommandResult(
            exit_code=126,
            stdout="",
            stderr=f"Command not allowed. Allowed commands: {', '.join(sorted(ALLOWED_COMMANDS))}",
        )

    if argv[0] == "cd":
        return CommandResult(exit_code=2, stdout="", stderr="cd is handled by the interactive terminal session")

    working_dir = cwd or f"/home/{linux_user}"

    # When a php_version is known, pass it so opanel-ent-helper can invoke the
    # correct LSPHP binary (lsphp84/bin/php) instead of the system default (php).
    extra_env: list[str] = []
    if php_version and _PHP_VERSION_RE.match(php_version):
        extra_env = [f"--php-version={php_version}"]

    result = shell.privileged(
        "terminal-exec",
        helper_args=[linux_user, working_dir, *extra_env, *argv],
        check=False,
    )

    stdout = _truncate_output(result.stdout)
    stderr = _truncate_output(result.stderr)

    return CommandResult(
        exit_code=result.returncode,
        stdout=stdout,
        stderr=stderr,
    )


def exec_batch(
    linux_user: str,
    commands: list[str],
    cwd: Optional[str] = None,
    timeout: int = DEFAULT_TIMEOUT,
    php_version: Optional[str] = None,
) -> list[CommandResult]:
    """Execute multiple commands sequentially.

    Args:
        linux_user: The Linux username for the website.
        commands: List of commands to execute.
        cwd: Working directory (defaults to user's home).
        timeout: Maximum execution time per command.
        php_version: PHP version to use (e.g. "8.4").

    Returns:
        List of CommandResult for each command.
    """
    results = []
    for cmd in commands:
        result = exec_command(linux_user, cmd, cwd=cwd, timeout=timeout, php_version=php_version)
        results.append(result)
        # Stop on first failure if non-interactive
        if result.exit_code != 0:
            break
    return results
