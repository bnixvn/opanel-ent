"""
Encryption helpers for at-rest secrets stored in the opanel-ent SQLite DB.

Used for: per-website MariaDB user passwords (DatabaseAccount.db_password),
SFTP backup target credentials, and TOTP secrets.

Key derivation: Fernet uses a 32-byte key derived via PBKDF2-HMAC-SHA256
(480 000 iterations, ~0.3 s on a 2024-era CPU) over settings.secret_key with
a static salt. This provides a brute-force work factor even if the key is
partially leaked.

Backward compatibility: decrypt() also tries the legacy SHA-256 derivation
(so existing ciphertexts continue to work after the PBKDF2 upgrade). Values
decrypted via the legacy path are transparently re-encrypted with PBKDF2 on
the next encrypt() call by the caller.

Rotating SECRET_KEY in production invalidates any previously stored
ciphertexts; do this only as a deliberate rekey operation. SECRET_KEY is
itself loaded from /opt/opanel-ent/backend/.env which is not world-readable.
"""

import base64
import hashlib
import logging
import os
from typing import Optional

from cryptography.fernet import Fernet, InvalidToken
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC

from app.core.config import settings


logger = logging.getLogger("opanel-ent.secrets")

_ENCRYPTED_PREFIX = "fernet:"
_PBKDF2_SALT = b"opanel-ent-fernet-v2-salt"
_PBKDF2_ITERATIONS = 480_000


def _derive_key_pbkdf2() -> bytes:
    """Derive a 32-byte Fernet key via PBKDF2-HMAC-SHA256."""
    kdf = PBKDF2HMAC(
        algorithm=hashes.SHA256(),
        length=32,
        salt=_PBKDF2_SALT,
        iterations=_PBKDF2_ITERATIONS,
    )
    raw = kdf.derive(settings.secret_key.encode("utf-8"))
    return base64.urlsafe_b64encode(raw)


def _derive_key_legacy() -> bytes:
    """Legacy SHA-256 derivation — kept only for decrypting old ciphertexts."""
    digest = hashlib.sha256(settings.secret_key.encode("utf-8")).digest()
    return base64.urlsafe_b64encode(digest)


_fernet = Fernet(_derive_key_pbkdf2())
_fernet_legacy = Fernet(_derive_key_legacy())


def encrypt(plaintext: str) -> str:
    if plaintext is None:
        return plaintext
    token = _fernet.encrypt(plaintext.encode("utf-8")).decode("utf-8")
    return _ENCRYPTED_PREFIX + token


def decrypt(stored: Optional[str]) -> str:
    """Decrypt a stored value.

    Behaviour:
    - Empty/None -> returns "".
    - Values prefixed with ``fernet:`` -> decrypted normally; tries PBKDF2
      first, then falls back to legacy SHA-256 derivation for old ciphertexts.
      Bad ciphertext raises ``RuntimeError`` (typically caused by SECRET_KEY
      rotation).
    - Values without the prefix are legacy plaintext from before encryption
      was introduced. We refuse to read them by default so any forgotten row
      surfaces immediately. Set ``STRICT_DECRYPT=false`` in the environment
      to temporarily allow passthrough during a migration window.
    """
    if not stored:
        return stored or ""
    if not stored.startswith(_ENCRYPTED_PREFIX):
        if not getattr(settings, "strict_decrypt", True):
            logger.warning(
                "secrets.decrypt(): legacy plaintext value detected (length=%d). "
                "Re-save the secret to encrypt it at rest.",
                len(stored),
            )
            return stored
        raise RuntimeError(
            "secrets.decrypt(): refusing to read legacy plaintext value. "
            "Re-save the affected record to encrypt it, or temporarily set "
            "STRICT_DECRYPT=false to migrate."
        )
    payload = stored[len(_ENCRYPTED_PREFIX):]
    raw_payload = payload.encode("utf-8")
    try:
        return _fernet.decrypt(raw_payload).decode("utf-8")
    except InvalidToken:
        pass
    try:
        return _fernet_legacy.decrypt(raw_payload).decode("utf-8")
    except InvalidToken as exc:
        raise RuntimeError("Cannot decrypt stored secret; SECRET_KEY may have been rotated") from exc


def is_encrypted(stored: Optional[str]) -> bool:
    """Return True if the stored value is a Fernet ciphertext written by us."""
    return bool(stored) and stored.startswith(_ENCRYPTED_PREFIX)