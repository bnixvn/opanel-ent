import pytest

from app.models.entities import User
from app.services import storage_quota
from app.services.storage_quota import BYTES_PER_MB, StorageQuotaExceeded


class _FakeQuery:
    def filter(self, *a, **kw):
        return self

    def all(self):
        return []


class _FakeSession:
    def query(self, *a, **kw):
        return _FakeQuery()


def _user(role="end_user", storage_limit_mb=1024):
    return User(username="u", role=role, storage_limit_mb=storage_limit_mb)


def test_zero_means_unlimited_not_zero_bytes():
    """Regression: 0 is the panel's 'no cap' value, but it used to yield a
    0-byte limit, so any existing data tripped 'Storage quota exceeded'."""
    assert storage_quota.user_storage_limit_bytes(_user(storage_limit_mb=0)) is None


def test_admin_is_unlimited():
    assert storage_quota.user_storage_limit_bytes(_user(role="admin")) is None


def test_positive_limit_is_converted_to_bytes():
    assert storage_quota.user_storage_limit_bytes(_user(storage_limit_mb=2048)) == 2048 * BYTES_PER_MB


def test_enforce_allows_any_size_when_limit_is_zero(monkeypatch):
    monkeypatch.setattr(storage_quota, "user_storage_used_bytes", lambda db, user, **kw: 900 * BYTES_PER_MB)
    storage_quota.enforce_user_storage_quota(
        _FakeSession(), _user(storage_limit_mb=0), incoming_bytes=500 * BYTES_PER_MB
    )


def test_enforce_still_blocks_over_a_real_limit(monkeypatch):
    monkeypatch.setattr(storage_quota, "user_storage_used_bytes", lambda db, user, **kw: 900 * BYTES_PER_MB)
    with pytest.raises(StorageQuotaExceeded):
        storage_quota.enforce_user_storage_quota(
            _FakeSession(), _user(storage_limit_mb=1024), incoming_bytes=200 * BYTES_PER_MB
        )


def test_enforce_allows_within_a_real_limit(monkeypatch):
    monkeypatch.setattr(storage_quota, "user_storage_used_bytes", lambda db, user, **kw: 100 * BYTES_PER_MB)
    storage_quota.enforce_user_storage_quota(
        _FakeSession(), _user(storage_limit_mb=1024), incoming_bytes=200 * BYTES_PER_MB
    )


def test_summary_reports_unlimited_as_none(monkeypatch):
    """The frontend treats storage_limit_bytes === null as 'no limit', so an
    unlimited account must not report a 0-byte limit."""
    monkeypatch.setattr(storage_quota, "user_storage_used_bytes", lambda db, user, **kw: 42 * BYTES_PER_MB)
    summary = storage_quota.storage_usage_summary(_FakeSession(), _user(storage_limit_mb=0))
    assert summary["storage_limit_bytes"] is None
    assert summary["storage_percent"] == 0.0
    assert summary["storage_used_bytes"] == 42 * BYTES_PER_MB
