"""Terminal API endpoints."""

import json
from pathlib import Path
from urllib.parse import urlparse

from fastapi import APIRouter, Depends, HTTPException, WebSocket, WebSocketDisconnect
from jose import JWTError, jwt
from pydantic import BaseModel
from sqlalchemy.orm import Session

from app.api.deps import get_current_user
from app.core.config import settings
from app.core.database import get_db
from app.core.permissions import Role, ensure_role, is_admin_role
from app.core.security import ALGORITHM
from app.models.entities import RevokedToken, User, Website
from app.services import terminal

router = APIRouter(prefix="/terminal", tags=["terminal"])


def _origin_allowed(websocket: WebSocket) -> bool:
    origin = (websocket.headers.get("origin") or "").rstrip("/")
    if not origin:
        return True
    origin_host = (urlparse(origin).netloc or "").lower()
    request_host = (websocket.headers.get("host") or "").lower()
    if origin_host and request_host and origin_host == request_host:
        return True
    allowed = {item.rstrip("/") for item in settings.cors_origins}
    if settings.panel_url:
        allowed.add(settings.panel_url.rstrip("/"))
    return bool(allowed) and origin in allowed


def _current_user_from_session_cookie(websocket: WebSocket, db: Session) -> User | None:
    token = websocket.cookies.get("opanel_ent_session")
    if not token:
        return None
    try:
        payload = jwt.decode(token, settings.secret_key, algorithms=[ALGORITHM])
        username = payload.get("sub")
        token_version = int(payload.get("tv", 0))
        jti = payload.get("jti")
        if not username:
            return None
    except (JWTError, ValueError):
        return None
    user = db.query(User).filter(User.username == username).first()
    if user is None or not user.is_active:
        return None
    if (user.token_version or 0) != token_version:
        return None
    if jti and db.query(RevokedToken.id).filter(RevokedToken.jti == jti).first():
        return None
    return user


async def get_user_website(
    website_id: int,
    db: Session,
    current_user: User,
) -> Website:
    """Get website and verify ownership."""
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    # Check ownership or admin role
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    return website


class TerminalExecRequest(BaseModel):
    """Request model for terminal command execution."""

    command: str


class TerminalExecResponse(BaseModel):
    """Response model for terminal command execution."""

    exit_code: int
    stdout: str
    stderr: str


@router.post("/exec/{website_id}", response_model=TerminalExecResponse)
async def exec_command(
    website_id: int,
    request: TerminalExecRequest,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    """Execute a single command as the website user.

    This endpoint is suitable for non-interactive commands like:
    - php artisan migrate
    - composer install
    - npm run build
    """
    website = await get_user_website(website_id, db, current_user)

    result = terminal.exec_command(
        website.linux_user,
        request.command,
        cwd=terminal.default_cwd(website.root_path),
        php_version=getattr(website, "php_version", None),
    )

    return TerminalExecResponse(
        exit_code=result.exit_code,
        stdout=result.stdout,
        stderr=result.stderr,
    )


@router.get("/allowed-commands")
async def list_allowed_commands(
    current_user: User = Depends(get_current_user),
):
    """List all allowed commands for terminal access."""
    return {"commands": sorted(terminal.ALLOWED_COMMANDS)}


@router.websocket("/ws/{website_id}")
async def terminal_websocket(
    websocket: WebSocket,
    website_id: int,
    db: Session = Depends(get_db),
):
    """WebSocket endpoint for interactive terminal sessions.

    Auth is handled via cookies (opanel_ent_session and opanel_ent_csrf) passed
    automatically by the browser.

    Messages:
    - Client -> Server: {"type": "input", "data": "command\n"}
    - Client -> Server: {"type": "resize", "cols": 80, "rows": 24}
    - Client -> Server: {"type": "ping"}
    - Server -> Client: {"type": "output", "data": "output text"}
    - Server -> Client: {"type": "exit", "code": 0}
    - Server -> Client: {"type": "pong"}
    """
    if not _origin_allowed(websocket):
        await websocket.close(code=4003, reason="Origin not allowed")
        return

    # Get token and validate step by step for better error messages
    token = websocket.cookies.get("opanel_ent_session")
    if not token:
        await websocket.close(code=4001, reason="No session cookie")
        return

    # Decode and validate JWT
    try:
        payload = jwt.decode(token, settings.secret_key, algorithms=[ALGORITHM])
        username = payload.get("sub")
        token_version = int(payload.get("tv", 0))
        jti = payload.get("jti")
        if not username:
            await websocket.close(code=4001, reason="Invalid token: missing username")
            return
    except jwt.ExpiredSignatureError:
        await websocket.close(code=4001, reason="Session expired")
        return
    except jwt.JWTClaimsError:
        await websocket.close(code=4001, reason="Invalid token: claims error")
        return
    except JWTError:
        await websocket.close(code=4001, reason="Invalid token")
        return

    # Check user exists and is active
    user = db.query(User).filter(User.username == username).first()
    if user is None:
        await websocket.close(code=4001, reason="User not found")
        return
    if not user.is_active:
        await websocket.close(code=4001, reason="User account is disabled")
        return

    # Check token version
    if (user.token_version or 0) != token_version:
        await websocket.close(code=4001, reason="Session invalidated")
        return

    # Check if token is revoked
    if jti and db.query(RevokedToken.id).filter(RevokedToken.jti == jti).first():
        await websocket.close(code=4001, reason="Session revoked")
        return

    current_user = user

    # Get website
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        await websocket.close(code=4004, reason="Website not found")
        return

    # Check ownership
    if website.owner_id != current_user.id and not is_admin_role(current_user.role):
        await websocket.close(code=4003, reason="Access denied")
        return

    if not website.linux_user:
        await websocket.close(code=4004, reason="Website runtime user is missing")
        return

    root_path = Path(website.root_path)
    if not root_path.exists() or not root_path.is_dir():
        await websocket.close(code=4005, reason="Website root path does not exist or is not accessible")
        return

    await websocket.accept()
    cwd = terminal.default_cwd(website.root_path)
    await websocket.send_json({"type": "cwd", "data": cwd})

    try:
        while True:
            message = await websocket.receive_text()

            try:
                msg = json.loads(message)
            except json.JSONDecodeError:
                await websocket.send_json({
                    "type": "error",
                    "data": "Invalid JSON message"
                })
                continue

            msg_type = msg.get("type")

            if msg_type == "ping":
                await websocket.send_json({"type": "pong"})
            elif msg_type == "input":
                # A pasted block may contain several lines/commands. Split on
                # newlines and run each command sequentially so a multi-line
                # paste behaves like typing each line and pressing Enter.
                raw_input = msg.get("data", "") or ""
                lines = [ln.rstrip() for ln in raw_input.split("\n")]
                for command in lines:
                    command = command.strip()
                    if not command:
                        continue
                    if command in {"clear", "cls"}:
                        await websocket.send_json({"type": "clear"})
                        await websocket.send_json({"type": "exit", "code": 0})
                        continue

                    try:
                        argv = terminal.split_command(command)
                    except ValueError as exc:
                        await websocket.send_json({"type": "output", "data": f"{exc}\r\n"})
                        await websocket.send_json({"type": "exit", "code": 2})
                        continue

                    if argv[0] == "cd":
                        if len(argv) > 2:
                            await websocket.send_json({"type": "output", "data": "usage: cd [path]\r\n"})
                            await websocket.send_json({"type": "exit", "code": 2})
                            continue
                        try:
                            cwd = terminal.resolve_cwd(website.root_path, cwd, argv[1] if len(argv) == 2 else "")
                        except ValueError as exc:
                            await websocket.send_json({"type": "output", "data": f"{exc}\r\n"})
                            await websocket.send_json({"type": "exit", "code": 1})
                            continue
                        await websocket.send_json({"type": "cwd", "data": cwd})
                        await websocket.send_json({"type": "exit", "code": 0})
                        continue

                    result = terminal.exec_command(
                        website.linux_user,
                        command,
                        cwd=cwd,
                        php_version=getattr(website, "php_version", None),
                    )
                    await websocket.send_json({
                        "type": "output",
                        "data": result.stdout + result.stderr,
                    })
                    await websocket.send_json({
                        "type": "exit",
                        "code": result.exit_code,
                    })
            elif msg_type == "resize":
                # Terminal resize - can be used for PTY support in future
                pass
            else:
                await websocket.send_json({
                    "type": "error",
                    "data": f"Unknown message type: {msg_type}"
                })

    except WebSocketDisconnect:
        pass  # Client disconnected
    except Exception as e:
        try:
            await websocket.send_json({
                "type": "error",
                "data": f"Server error: {str(e)}"
            })
        except Exception:
            pass  # WebSocket already closed
