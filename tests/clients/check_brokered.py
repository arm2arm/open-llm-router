"""Brokered login: upstream-idp -> olr-dev -> router portal, and the required claim.

A member of the upstream realm (affiliation member@example.org) signs in through the broker,
creates a key in the portal and uses it. A guest without that affiliation is refused by the
router's OIDC_REQUIRED_CLAIM check after Keycloak has let them in.
"""

import os
import re
from html import unescape
from urllib.error import HTTPError
from urllib.parse import urlencode, urljoin
from urllib.request import Request

from check_portal import Browser, Form


def brokered_login(browser, username, password):
    """Return the final HTTP status of the router callback for an upstream user."""
    with browser.opener.open(browser.base + "/portal/login", timeout=15) as response:
        page, text = response.url, response.read().decode()
    link = re.search(r'href="([^"]*/broker/upstream-idp/login[^"]*)"', text)
    assert link, "the olr-dev login page offers no upstream-idp button"
    with browser.opener.open(urljoin(page, unescape(link.group(1))), timeout=15) as response:
        page, text = response.url, response.read().decode()
    assert "/realms/upstream-idp/" in page, "the broker did not redirect to the upstream realm"
    form = Form(text)
    form.fields.update(username=username, password=password)
    request = Request(urljoin(page, form.action), data=urlencode(form.fields).encode())
    try:
        with browser.opener.open(request, timeout=15) as response:
            response.read()
            final = response.url
    except HTTPError as error:
        assert "/portal/callback" in error.url, "login failed before reaching the router: " + error.url
        return error.code
    assert final == browser.base + "/portal/", "brokered login did not reach the portal: " + final
    return 200


def main():
    base = os.environ.get("OLR_BASE_URL", "http://localhost:8080").rstrip("/")

    member = Browser(base)
    assert brokered_login(member, "fed-member", "fed-member-dev-only") == 200
    me = member.request("/portal/api/me")
    assert me["user"]["username"] == "fed-member", me["user"]
    assert me["user"]["groups"] == ["students"], "the affiliation mapper did not grant the group"
    key = member.request("/portal/api/keys", "POST", {"label": "brokered"}, want=201)["key"]
    models = member.request("/v1/models", key=key)
    assert any(m["id"] == "chat-model" for m in models["data"]), models
    member.request(
        "/v1/chat/completions", "POST", {"model": "chat-model", "messages": [{"role": "user", "content": "hi"}]}, key=key
    )

    guest = Browser(base)
    status = brokered_login(guest, "fed-guest", "fed-guest-dev-only")
    assert status == 403, "a user without the required claim was let in: %s" % status
    guest.request("/portal/api/me", want=401)
    print("Brokered login, key use and required-claim refusal passed")


if __name__ == "__main__":
    main()
