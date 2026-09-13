"""drop websites.http_flood_enabled / http_flood_config

HTTP flood protection never did anything. Turning it on rendered an OLS
`extprocessor` block pointing at 127.0.0.1:1 and no directive anywhere
referenced it, so the maxConns value was inert and every request went through
untouched. A toggle that reports "Enabled" while protecting nothing is worse
than no toggle, so the feature is gone rather than rebuilt.

Revision ID: 0026_drop_website_http_flood
Revises: 0025_drop_waf_bot_allow
Create Date: 2026-09-12
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "0026_drop_website_http_flood"
down_revision: Union[str, None] = "0025_drop_waf_bot_allow"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    with op.batch_alter_table("websites") as batch_op:
        batch_op.drop_column("http_flood_config")
        batch_op.drop_column("http_flood_enabled")


def downgrade() -> None:
    with op.batch_alter_table("websites") as batch_op:
        batch_op.add_column(
            sa.Column("http_flood_enabled", sa.Boolean(), nullable=False, server_default=sa.false())
        )
        batch_op.add_column(
            sa.Column("http_flood_config", sa.Text(), nullable=False, server_default="")
        )
