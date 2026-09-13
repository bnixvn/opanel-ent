import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from app.core.database import Base
from app.models.entities import User
from app.services import da_import


@pytest.fixture
def db():
    engine = create_engine("sqlite://")
    Base.metadata.create_all(engine)
    session = sessionmaker(bind=engine)()
    try:
        yield session
    finally:
        session.close()


def _add(db, username, email):
    db.add(User(username=username, email=email, hashed_password="x", role="end_user"))
    db.flush()


def test_free_email_is_used_as_is(db):
    email, warning = da_import._available_email(db, "babatap", "owner@example.com")
    assert email == "owner@example.com"
    assert warning is None


def test_missing_email_falls_back_to_import_local(db):
    email, warning = da_import._available_email(db, "babatap", "")
    assert email == "babatap@import.local"
    assert warning is None


def test_a_shared_contact_mailbox_is_kept(db):
    """DirectAdmin accounts often share one contact mailbox. Since users.email
    is no longer unique, the address is kept as-is -- no rename, no warning."""
    _add(db, "duchonotes", "shared@gmail.com")

    email, warning = da_import._available_email(db, "babatap", "shared@gmail.com")

    assert email == "shared@gmail.com"
    assert warning is None
    _add(db, "babatap", email)  # and it inserts fine


def test_two_users_can_share_an_email(db):
    _add(db, "duchonotes", "shared@gmail.com")
    _add(db, "babatap", "shared@gmail.com")  # no IntegrityError
    assert db.query(User).filter(User.email == "shared@gmail.com").count() == 2
