"""Local-only HTTP -> gRPC -> PostgreSQL check. Creates one disposable account."""

import json
import os
import sys
import time
import uuid
from http.cookies import SimpleCookie
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


BASE = os.environ.get("BASE_URL", "http://127.0.0.1:8080").rstrip("/")


def request(path, fields=None, raw=None):
    data = raw if raw is not None else (None if fields is None else json.dumps(fields).encode())
    req = Request(BASE + path, data=data, headers={"Content-Type": "application/json"})
    try:
        response = urlopen(req, timeout=10)
    except HTTPError as error:
        response = error
    with response:
        body = response.read()
        return response.status, response.headers, json.loads(body) if body else {}


def wait_status(path, expected):
    deadline = time.monotonic() + 60
    while True:
        try:
            if request(path)[0] == expected:
                return
        except (URLError, TimeoutError):
            pass
        if time.monotonic() >= deadline:
            raise RuntimeError(f"{path} did not return {expected} within 60 seconds")
        time.sleep(1)


def main():
    if sys.argv[1:] == ["--expect-unavailable"]:
        wait_status("/readyz", 503)
        assert request("/healthz")[0] == 200
        code, _, body = request("/login", {"email": "outage@example.com", "password": "password"})
        assert code == 503 and body == {"error": "account service unavailable"}, (code, body)
        # Public reads must not depend on auth-db: unknown id resolves in user-db -> 404.
        assert request(f"/users/{uuid.uuid4()}")[0] == 404, "profile read broke during auth-db outage"
        print("PASS: dependency outage, readiness 503, liveness 200, safe login 503, public read unaffected")
        return
    if sys.argv[1:]:
        raise SystemExit("usage: python3 scripts/smoke.py [--expect-unavailable]")
    wait_status("/readyz", 200)

    email = "smoke-" + uuid.uuid4().hex + "@example.com"
    password = "smoke-test-password"
    credentials = {"email": email, "password": password}

    username = "smoke-" + uuid.uuid4().hex
    code, headers, body = request("/register", {**credentials, "name": "Smoke Test", "username": username})
    assert code == 200, (code, body)
    user_id = body["user_id"]
    assert len(user_id) == 36  # UUID
    cookie = SimpleCookie(headers.get("Set-Cookie", ""))["token"]
    assert cookie["httponly"] and cookie["samesite"] == "Strict"
    assert int(cookie["max-age"]) == 900  # 15 minutes

    # Duplicate email -> 200 with neutral message (anti-enumeration)
    code, _, body_dup = request("/register", {**credentials, "name": "Smoke Test", "username": "other-" + uuid.uuid4().hex})
    assert code == 200 and "verification link" in body_dup.get("message", ""), (code, body_dup)

    # Login with same identity
    code, _, body = request("/login", credentials)
    assert code == 200 and body["user_id"] == user_id, (code, body)

    # Wrong password and unknown email both return 401
    wrong = request("/login", {**credentials, "password": "wrong-password"})
    missing = request("/login", {"email": "missing-" + uuid.uuid4().hex + "@example.com", "password": "password"})
    assert wrong[0] == missing[0] == 401
    assert wrong[2] == missing[2] == {"error": "invalid credentials"}

    # Short password rejected
    assert request("/register", {**credentials, "password": "short", "name": "Smoke Test", "username": "u"})[0] == 400

    # Public profile: readable without a session, only public fields come back
    code, _, pub = request(f"/users/{user_id}")
    assert code == 200 and pub == {"user_id": user_id, "name": "Smoke Test", "username": username}, (code, pub)
    assert request(f"/users/{uuid.uuid4()}")[0] == 404, "unknown profile must 404"
    assert request("/users/not-a-uuid")[0] == 400, "malformed profile id must 400"

    # Refresh flow
    refresh_token = body["refresh_token"]
    code, _, body2 = request("/refresh", {"refresh_token": refresh_token})
    assert code == 200 and body2["user_id"] == user_id, (code, body2)
    new_refresh = body2["refresh_token"]
    assert new_refresh != refresh_token

    # Old refresh token no longer works
    code, _, _ = request("/refresh", {"refresh_token": refresh_token})
    assert code == 401, code

    # The replay of a rotated-out token must revoke the whole session family:
    # the current token of the same account must stop working too.
    code, _, _ = request("/refresh", {"refresh_token": new_refresh})
    assert code == 401, code

    # Logout invalidates the new refresh token
    code, _, _ = request("/logout", {"refresh_token": new_refresh})
    assert code == 200, code
    code, _, _ = request("/refresh", {"refresh_token": new_refresh})
    assert code == 401, code

    print("PASS: readiness, registration, stable identity, cookie, duplicate, login, invalid credentials, validation, public profile, refresh, logout")


if __name__ == "__main__":
    main()
