import pytest
from pydantic import ValidationError

from app.schemas.schemas import ReuseSslRequest, WebsiteAliasCreate, WildcardSslRequest


def test_website_alias_create_accepts_redirect_mode():
    payload = WebsiteAliasCreate(domain="Alias.Example.Test", mode="redirect")

    assert payload.domain == "alias.example.test"
    assert payload.mode == "redirect"


def test_website_alias_create_rejects_unknown_mode():
    with pytest.raises(ValidationError):
        WebsiteAliasCreate(domain="alias.example.test", mode="mirror")


def test_wildcard_ssl_request_accepts_token_or_email_only():
    assert WildcardSslRequest(api_token="cf-token-abcdefghij").email is None
    assert WildcardSslRequest(email="admin@example.com").api_token is None
    with pytest.raises(ValidationError):
        WildcardSslRequest(api_token="has spaces and short")


@pytest.mark.parametrize("name", ["letsencrypt:example.com", "manual:foo.bar.com"])
def test_reuse_ssl_request_accepts_valid_names(name):
    assert ReuseSslRequest(name=name).name == name


@pytest.mark.parametrize("name", ["example.com", "other:example.com", "letsencrypt:", "letsencrypt:../x"])
def test_reuse_ssl_request_rejects_bad_names(name):
    with pytest.raises(ValidationError):
        ReuseSslRequest(name=name)
