"""per-site bad bot blocking

The server-wide bot list lives in panel-settings.json; these columns are the
per-site part: an on/off switch (on by default) plus extra patterns and
allow-list entries for that site alone.

Revision ID: 0024_waf_bad_bots
Revises: 0023_user_email_not_unique
Create Date: 2026-09-08
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "0024_waf_bad_bots"
down_revision: Union[str, None] = "0023_user_email_not_unique"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    with op.batch_alter_table("websites") as batch_op:
        batch_op.add_column(
            sa.Column("waf_bot_enabled", sa.Boolean(), nullable=False, server_default=sa.text("1"))
        )
        batch_op.add_column(
            sa.Column("waf_bot_extra", sa.Text(), nullable=False, server_default="")
        )
        batch_op.add_column(
            sa.Column("waf_bot_allow", sa.Text(), nullable=False, server_default="")
        )


def downgrade() -> None:
    with op.batch_alter_table("websites") as batch_op:
        batch_op.drop_column("waf_bot_allow")
        batch_op.drop_column("waf_bot_extra")
        batch_op.drop_column("waf_bot_enabled")
