"""HTTP flood protection is gone, and must not come back by halves.

Enabling it rendered an OLS `extprocessor` block pointing at 127.0.0.1:1 that no
directive referenced, so nothing was ever throttled while the panel reported
"Flood on". The columns, the endpoint, the helper command and the UI are all
removed; these assertions keep any stray half of it from being reintroduced.
"""
from pathlib import Path

from app.api import websites
from app.models.entities import Website
from app.schemas import schemas
from app.services import nginx, openlitespeed

PROJECT_ROOT = Path(__file__).resolve().parents[3]
SOURCES = [
    PROJECT_ROOT / "backend" / "app",
    PROJECT_ROOT / "installer",
    PROJECT_ROOT / "frontend" / "src",
]


def test_no_flood_columns_on_the_website_model():
    assert not hasattr(Website, "http_flood_enabled")
    assert not hasattr(Website, "http_flood_config")
    assert "http_flood_enabled" not in schemas.WebsiteOut.model_fields
    assert not hasattr(schemas, "WebsiteHttpFloodUpdate")


def test_no_flood_route_is_registered():
    paths = [route.path for route in websites.router.routes]
    assert not [p for p in paths if "flood" in p]


def test_webserver_services_expose_nothing_flood_shaped():
    for module in (openlitespeed, nginx):
        assert not [name for name in dir(module) if "flood" in name.lower()], module.__name__


def test_the_word_flood_is_gone_from_the_tree():
    offenders = []
    for root in SOURCES:
        for path in root.rglob("*"):
            # Guard tests name what they guard against, so they are exempt.
            if not path.is_file() or "__pycache__" in path.parts or "tests" in path.parts:
                continue
            if path.suffix.lower() in {".pyc", ".png", ".jpg", ".ico", ".woff", ".woff2"}:
                continue
            try:
                text = path.read_text(encoding="utf-8")
            except (UnicodeDecodeError, OSError):
                continue
            for number, line in enumerate(text.splitlines(), 1):
                if "flood" in line.lower():
                    offenders.append(f"{path.relative_to(PROJECT_ROOT)}:{number}")

    assert not offenders, offenders


def test_migration_drops_both_columns():
    migration = PROJECT_ROOT / "backend" / "alembic" / "versions" / "0026_drop_website_http_flood.py"
    content = migration.read_text(encoding="utf-8")

    assert 'down_revision: Union[str, None] = "0025_drop_waf_bot_allow"' in content
    assert 'batch_op.drop_column("http_flood_config")' in content
    assert 'batch_op.drop_column("http_flood_enabled")' in content
