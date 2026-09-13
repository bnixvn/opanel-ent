"""drop websites.waf_bot_allow

The per-site "never block" list is gone: the bot list now blocks exactly what
the admin put in it, and you stop blocking something by removing it from the
list rather than adding a second, opposing list.

Revision ID: 0025_drop_waf_bot_allow
Revises: 0024_waf_bad_bots
Create Date: 2026-09-08
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "0025_drop_waf_bot_allow"
down_revision: Union[str, None] = "0024_waf_bad_bots"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    with op.batch_alter_table("websites") as batch_op:
        batch_op.drop_column("waf_bot_allow")


def downgrade() -> None:
    with op.batch_alter_table("websites") as batch_op:
        batch_op.add_column(
            sa.Column("waf_bot_allow", sa.Text(), nullable=False, server_default="")
        )
