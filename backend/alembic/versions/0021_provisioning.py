"""provisioning: api_tokens, hosting_plans, hosting_accounts, provisioning_jobs, panel_sso_tokens

Revision ID: 0021_provisioning
Revises: 0020_website_aliases
Create Date: 2026-08-09
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa

revision: str = "0021_provisioning"
down_revision: Union[str, None] = "0020_website_aliases"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    # --- API Tokens ---
    op.create_table(
        "api_tokens",
        sa.Column("id", sa.Integer(), nullable=False),
        sa.Column("name", sa.String(length=128), nullable=False),
        sa.Column("token_hash", sa.String(length=64), nullable=False),
        sa.Column("scopes", sa.Text(), nullable=False, server_default="[]"),
        sa.Column("expires_at", sa.DateTime(), nullable=True),
        sa.Column("revoked_at", sa.DateTime(), nullable=True),
        sa.Column("last_used_at", sa.DateTime(), nullable=True),
        sa.Column("ip_allowlist", sa.Text(), nullable=True),
        sa.Column("created_at", sa.DateTime(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_api_tokens_token_hash", "api_tokens", ["token_hash"], unique=True)

    # --- Hosting Plans ---
    op.create_table(
        "hosting_plans",
        sa.Column("id", sa.Integer(), nullable=False),
        sa.Column("slug", sa.String(length=64), nullable=False),
        sa.Column("name", sa.String(length=128), nullable=False),
        sa.Column("website_limit", sa.Integer(), nullable=False, server_default="1"),
        sa.Column("storage_limit_mb", sa.Integer(), nullable=False, server_default="1024"),
        sa.Column("php_version", sa.String(length=16), nullable=False, server_default="8.4"),
        sa.Column("app_type", sa.String(length=32), nullable=False, server_default="php"),
        sa.Column("auto_ssl", sa.Boolean(), nullable=False, server_default=sa.false()),
        sa.Column("active", sa.Boolean(), nullable=False, server_default=sa.true()),
        sa.Column("created_at", sa.DateTime(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_hosting_plans_slug", "hosting_plans", ["slug"], unique=True)

    # --- Hosting Accounts (billing link) ---
    op.create_table(
        "hosting_accounts",
        sa.Column("id", sa.Integer(), nullable=False),
        sa.Column("external_id", sa.String(length=128), nullable=False),
        sa.Column("user_id", sa.Integer(), sa.ForeignKey("users.id"), nullable=False),
        sa.Column("primary_website_id", sa.Integer(), sa.ForeignKey("websites.id"), nullable=True),
        sa.Column("plan_id", sa.Integer(), sa.ForeignKey("hosting_plans.id"), nullable=True),
        sa.Column("status", sa.String(length=32), nullable=False, server_default="active"),
        sa.Column("created_at", sa.DateTime(), nullable=True),
        sa.Column("updated_at", sa.DateTime(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_hosting_accounts_external_id", "hosting_accounts", ["external_id"], unique=True)
    op.create_index("ix_hosting_accounts_user_id", "hosting_accounts", ["user_id"])

    # --- Provisioning Jobs ---
    op.create_table(
        "provisioning_jobs",
        sa.Column("id", sa.Integer(), nullable=False),
        sa.Column("external_id", sa.String(length=128), nullable=False),
        sa.Column("action", sa.String(length=32), nullable=False),
        sa.Column("status", sa.String(length=32), nullable=False, server_default="pending"),
        sa.Column("error", sa.Text(), nullable=True),
        sa.Column("retry_count", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("started_at", sa.DateTime(), nullable=True),
        sa.Column("finished_at", sa.DateTime(), nullable=True),
        sa.Column("created_at", sa.DateTime(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_provisioning_jobs_external_id", "provisioning_jobs", ["external_id"])

    # --- Panel SSO Tokens ---
    op.create_table(
        "panel_sso_tokens",
        sa.Column("id", sa.Integer(), nullable=False),
        sa.Column("user_id", sa.Integer(), sa.ForeignKey("users.id"), nullable=False),
        sa.Column("token_hash", sa.String(length=64), nullable=False),
        sa.Column("expires_at", sa.DateTime(), nullable=False),
        sa.Column("used_at", sa.DateTime(), nullable=True),
        sa.Column("created_at", sa.DateTime(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_panel_sso_tokens_token_hash", "panel_sso_tokens", ["token_hash"], unique=True)
    op.create_index("ix_panel_sso_tokens_user_id", "panel_sso_tokens", ["user_id"])

    # Seed default plan
    op.execute(
        "INSERT INTO hosting_plans (slug, name, website_limit, storage_limit_mb, php_version, app_type, auto_ssl, active, created_at) "
        "VALUES ('default', 'Default Plan', 5, 1024, '8.4', 'php', 0, 1, datetime('now'))"
    )


def downgrade() -> None:
    op.drop_table("panel_sso_tokens")
    op.drop_table("provisioning_jobs")
    op.drop_table("hosting_accounts")
    op.drop_table("hosting_plans")
    op.drop_table("api_tokens")
