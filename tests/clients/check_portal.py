"""Exercise imported Keycloak users and the portal using real authorization-code logins."""

import json
import os
import sys
import time
from html.parser import HTMLParser
from http.cookiejar import CookieJar, DefaultCookiePolicy
from urllib.error import HTTPError
from urllib.parse import urlencode, urljoin, urlsplit
from urllib.request import HTTPCookieProcessor, Request, build_opener, urlopen


class Form(HTMLParser):
    def __init__(self, text):
        super().__init__(convert_charrefs=True)
        self.action = None
        self.fields = {}
        self.inside = False
        self.feed(text)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "form" and self.action is None and attrs.get("action"):
            self.action = attrs["action"]
            self.inside = True
        if tag == "input" and self.inside and attrs.get("name"):
            self.fields[attrs["name"]] = attrs.get("value", "")

    def handle_endtag(self, tag):
        if tag == "form":
            self.inside = False


class LocalhostPolicy(DefaultCookiePolicy):
    def return_ok_secure(self, cookie, request):
        # Match browsers' trusted-localhost exception for Keycloak's Secure
        # cookies. HTTP hosts other than literal localhost keep normal checks.
        if urlsplit(request.full_url).hostname == "localhost":
            return True
        return super().return_ok_secure(cookie, request)


class Browser:
    def __init__(self, base):
        self.base = base
        self.jar = CookieJar(policy=LocalhostPolicy())
        self.opener = build_opener(HTTPCookieProcessor(self.jar))

    def request(self, path, method="GET", body=None, origin=None, key=None, want=200):
        headers = {}
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
            headers["Origin"] = self.base if origin is None else origin
        if key:
            headers["Authorization"] = "Bearer " + key
        request = Request(self.base + path, data=data, headers=headers, method=method)
        try:
            response = self.opener.open(request, timeout=15)
        except HTTPError as error:
            response = error
        with response:
            assert response.status == want, "unexpected status on " + path
            text = response.read().decode()
            if path.startswith("/portal/"):
                assert response.headers["Cache-Control"] == "no-store"
            if path.startswith("/v1/"):
                assert not response.headers.get("Set-Cookie")
            return json.loads(text) if text else None

    def login(self, username, password):
        with self.opener.open(self.base + "/portal/login", timeout=15) as response:
            form = Form(response.read().decode())
            page = response.url
        if form.action is not None:
            form.fields.update(username=username, password=password)
            request = Request(urljoin(page, form.action), data=urlencode(form.fields).encode())
            with self.opener.open(request, timeout=15) as response:
                response.read()
                assert response.url == self.base + "/portal/", "login did not reach the portal"
        me = self.request("/portal/api/me")
        assert me["user"]["username"] == username
        sessions = [cookie for cookie in self.jar if cookie.name == "olr_session"]
        assert len(sessions) == 1 and sessions[0].path == "/portal"
        assert sessions[0].has_nonstandard_attr("HttpOnly")
        assert sessions[0].get_nonstandard_attr("SameSite") == "Lax"
        return me

    def logout(self):
        location = self.request("/portal/logout", "POST", {})["logout_url"]
        assert "id_token_hint" not in location
        self.request("/portal/api/me", want=401)
        with self.opener.open(location, timeout=15) as response:
            form = Form(response.read().decode())
            page = response.url
        if form.action:
            form.fields["confirmLogout"] = ""
            with self.opener.open(Request(urljoin(page, form.action), data=urlencode(form.fields).encode()), timeout=15) as response:
                response.read()
        with self.opener.open(self.base + "/portal/login", timeout=15) as response:
            form = Form(response.read().decode())
        assert "username" in form.fields, "identity-provider session survived logout"


def main():
    base = os.environ.get("OLR_BASE_URL", "http://localhost:8080").rstrip("/")
    issuer = os.environ.get("OIDC_ISSUER", "http://localhost:8081/realms/olr-dev").rstrip("/")
    anonymous = Browser(base)
    anonymous.request("/portal/api/me", want=401)
    with anonymous.opener.open(issuer + "/.well-known/openid-configuration", timeout=15) as response:
        assert json.load(response)["issuer"] == issuer
    browsers = {}
    for username, group in [("staff", "staff"), ("student", "students"), ("admin", "admins")]:
        browser = Browser(base)
        me = browser.login(username, username + "-dev-only")
        assert me["user"]["groups"] == [group], "imported group mapper is incorrect"
        browsers[username] = browser
    a, b = browsers["staff"], browsers["student"]
    key = a.request("/portal/api/keys", "POST", {"label": "portal-acceptance"}, want=201)["key"]
    keys = a.request("/portal/api/keys")["keys"]
    owned = next(item for item in keys if item["prefix"] == key[:12])
    assert key not in json.dumps(keys)
    assert owned["id"] not in json.dumps(b.request("/portal/api/keys"))
    b.request("/portal/api/keys/" + owned["id"], "DELETE", {}, want=404)
    for method, path, body in [("POST", "/portal/api/keys", {"label": "forbidden"}), ("DELETE", "/portal/api/keys/" + owned["id"], {})]:
        a.request(path, method, body, origin="https://evil.example", want=403)
    a.request("/v1/models", want=401)  # Cookies are not API credentials.
    a.request("/v1/chat/completions", "POST", {"model": "chat-model", "messages": [{"role": "user", "content": "hello private prompt"}], "max_tokens": 7}, key=key)
    deadline = time.monotonic() + 5
    while True:
        usage = a.request("/portal/api/usage")
        if usage["today"] and sum(row["input_tokens"] + row["output_tokens"] for row in usage["today"]) >= 10:
            break
        assert time.monotonic() < deadline, "accounting did not reach portal usage"
        time.sleep(0.05)
    models = a.request("/portal/api/models")
    assert models["base_url"] == base + "/v1" and any(item["id"] == "chat-model" for item in models["models"])
    a.request("/portal/api/keys/" + owned["id"], "DELETE", {}, want=204)
    a.request("/v1/models", key=key, want=401)
    # Change a real Keycloak membership, then perform a fresh authorization
    # round trip through its SSO session. Always restore the imported fixture.
    provider_origin = issuer.rsplit("/realms/", 1)[0]
    credentials = urlencode({"grant_type": "password", "client_id": "admin-cli", "username": os.environ.get("KEYCLOAK_ADMIN_USER", "admin"), "password": os.environ.get("KEYCLOAK_ADMIN_PASSWORD", "keycloak-admin-dev-only")}).encode()
    with urlopen(Request(provider_origin + "/realms/master/protocol/openid-connect/token", data=credentials), timeout=15) as response:
        admin_token = json.load(response)["access_token"]

    def admin(path, method="GET"):
        headers = {"Authorization": "Bearer " + admin_token}
        request = Request(provider_origin + "/admin/realms/olr-dev" + path, method=method, headers=headers)
        with urlopen(request, timeout=15) as response:
            data = response.read()
            return json.loads(data) if data else None

    user_id = admin("/users?username=staff&exact=true")[0]["id"]
    groups = {group["name"]: group["id"] for group in admin("/groups")}
    membership = "/users/" + user_id + "/groups/"
    try:
        admin(membership + groups["staff"], "DELETE")
        admin(membership + groups["students"], "PUT")
        assert a.login("staff", "staff-dev-only")["user"]["groups"] == ["students"], "relogin did not refresh Keycloak groups"
    finally:
        try:
            admin(membership + groups["students"], "DELETE")
        finally:
            admin(membership + groups["staff"], "PUT")
    assert a.login("staff", "staff-dev-only")["user"]["groups"] == ["staff"]
    for browser in browsers.values():
        browser.logout()
    print("Keycloak login, group refresh, portal isolation, usage, revocation and SSO logout passed")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print("Portal checks failed: " + str(error), file=sys.stderr)
        sys.exit(1)
