"""website SSL modes: Cloudflare DNS-01 wildcard, and reuse an existing cert

Revision ID: 0022_website_ssl_modes
Revises: 0021_provisioning
Create Date: 2026-08-31
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "0022_website_ssl_modes"
down_revision: Union[str, None] = "0021_provisioning"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    # Wildcard certificate issued over the Cloudflare DNS-01 challenge.
    op.add_column(
        "websites",
        sa.Column("ssl_wildcard", sa.Boolean(), nullable=False, server_default=sa.false()),
    )
    op.add_column(
        "websites",
        sa.Column("ssl_dns_provider", sa.String(length=32), nullable=False, server_default=""),
    )
    # Fernet ciphertext of the DNS provider API token, kept for renewal / re-issue.
    op.add_column("websites", sa.Column("ssl_dns_api_token", sa.Text(), nullable=True))
    # ssl_mode == "reuse": this website is served with an existing certificate
    # already on the box, identified as "<source>:<name>"
    # (e.g. "letsencrypt:example.com" or "manual:foo.com").
    op.add_column("websites", sa.Column("ssl_reuse_name", sa.String(length=300), nullable=True))


def downgrade() -> None:
    op.drop_column("websites", "ssl_reuse_name")
    op.drop_column("websites", "ssl_dns_api_token")
    op.drop_column("websites", "ssl_dns_provider")
    op.drop_column("websites", "ssl_wildcard")
