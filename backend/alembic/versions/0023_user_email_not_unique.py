"""users.email is no longer unique

Uniqueness is enforced on the username only. Several panel users may share a
contact address (a reseller with many accounts, DirectAdmin imports that share
one mailbox, ...). The index stays for lookup speed, just not UNIQUE.

Revision ID: 0023_user_email_not_unique
Revises: 0022_website_ssl_modes
Create Date: 2026-09-04
"""
from typing import Sequence, Union

from alembic import op


revision: str = "0023_user_email_not_unique"
down_revision: Union[str, None] = "0022_website_ssl_modes"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.drop_index("ix_users_email", table_name="users")
    op.create_index("ix_users_email", "users", ["email"], unique=False)


def downgrade() -> None:
    op.drop_index("ix_users_email", table_name="users")
    op.create_index("ix_users_email", "users", ["email"], unique=True)
