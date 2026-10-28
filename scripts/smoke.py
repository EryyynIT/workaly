"""Local-only HTTP -> gRPC -> PostgreSQL check. Creates one disposable account."""

import json
import os
import re
import subprocess
import sys
import time
import uuid
from http.cookies import SimpleCookie
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


BASE = os.environ.get("BASE_URL", "http://127.0.0.1:8080").rstrip("/")


def request(path, fields=None, raw=None, cookie=None, method=None):
    headers = {"Content-Type": "application/json"}
    if cookie:
        headers["Cookie"] = cookie
    data = raw if raw is not None else (None if fields is None else json.dumps(fields).encode())
    if method is None:
        # A body means POST; a bare request is a read.
        method = "POST" if data is not None else "GET"
    elif data is None:
        data = b""
    req = Request(BASE + path, data=data, headers=headers, method=method)
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


def dev_verify_token(email):
    """Extracts the confirmation token of email from the auth-service dev log.

    The development mailer prints the token in plain text; a real provider
    replaces it. SMOKE_LOGS_CMD is the command whose output is the log, for
    example "docker compose logs auth-service" (default, CI) or
    "podman-compose -p <stand> -f compose.yaml logs auth-service" (local).
    """
    command = os.environ.get("SMOKE_LOGS_CMD", "docker compose logs auth-service")
    env = dict(os.environ)
    # compose tools interpolate ${JWT_SECRET:?} even for `logs`; the scrape
    # never touches auth, so any non-empty value satisfies the requirement.
    env.setdefault("JWT_SECRET", "smoke-log-scrape-interpolation-only")
    try:
        result = subprocess.run(command.split(), capture_output=True, text=True, timeout=60, env=env)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError(f"cannot run {command!r}: {error}")
    if result.returncode != 0:
        raise RuntimeError(f"{command!r} failed: {result.stderr.strip()}")
    logs = result.stdout
    pattern = (r'"to":"' + re.escape(email) + r'".*'
               r'Verification token \(valid 24 hours\): ([A-Za-z0-9_-]{43})')
    tokens = re.findall(pattern, logs)
    if not tokens:
        raise RuntimeError(f"no verification email for {email} in {command!r} output")
    return tokens[-1]


def vacancy_fields(title):
    return {
        "title": title,
        "description": "Сопровождение платформы обработки платежей.",
        "employment_type": "full_time",
        "work_format": "remote",
        "salary_min": "150000",
        "salary_max": "250000.50",
        "currency": "RUB",
    }


def register_account(prefix):
    email = prefix + "-" + uuid.uuid4().hex + "@example.com"
    code, headers, body = request("/register", {
        "email": email,
        "password": "smoke-test-password",
        "name": "Smoke " + prefix,
        "username": prefix + "-" + uuid.uuid4().hex,
    })
    assert code == 200, (code, body)
    cookie = "token=" + SimpleCookie(headers.get("Set-Cookie", ""))["token"].value
    return email, cookie, body["user_id"]


def vacancy_flow():
    """The author side of the vacancy catalog through the HTTP edge."""
    # A draft needs no confirmed email.
    email, author, _ = register_account("smoke-author")
    code, _, draft = request("/vacancies", vacancy_fields("Разработчик платформы"), cookie=author)
    assert code == 201 and draft["status"] == "draft" and draft["version"] == 1, (code, draft)
    vacancy_id = draft["id"]

    # Field validation lives in the catalog service and stays precise.
    code, _, body = request("/vacancies", {**vacancy_fields("x"), "employment_type": "shift"}, cookie=author)
    assert code == 400 and "employment_type" in body["error"], (code, body)

    # Publishing without a confirmed email is refused before the RPC.
    code, _, body = request(f"/vacancies/{vacancy_id}/publish", cookie=author, method="POST")
    assert code == 403 and body == {"error": "email_not_verified"}, (code, body)

    code, _, body = request("/verify-email", {"token": dev_verify_token(email)})
    assert code == 200, (code, body)

    # Optimistic version: a stale version conflicts, the current one applies.
    stale = {**vacancy_fields("Обновлённая вакансия"), "version": 99}
    code, _, body = request(f"/vacancies/{vacancy_id}", stale, cookie=author, method="PATCH")
    assert code == 409 and "changed since you last read it" in body["error"], (code, body)
    code, _, body = request(f"/vacancies/{vacancy_id}", {**vacancy_fields("Обновлённая вакансия"), "version": 1}, cookie=author, method="PATCH")
    assert code == 200 and body["version"] == 2 and body["title"] == "Обновлённая вакансия", (code, body)

    # A draft is invisible to the public and to strangers.
    assert request(f"/vacancies/{vacancy_id}")[0] == 404, "draft must not be public"
    code, _, body = request("/my/vacancies", cookie=author)
    assert code == 200 and [v["id"] for v in body["vacancies"]] == [vacancy_id], (code, body)

    # Verified author publishes; a second publish is a conflict.
    code, _, body = request(f"/vacancies/{vacancy_id}/publish", cookie=author, method="POST")
    assert code == 200 and body["status"] == "published" and body["published_at"], (code, body)
    code, _, body = request(f"/vacancies/{vacancy_id}/publish", cookie=author, method="POST")
    assert code == 409 and "already published" in body["error"], (code, body)

    # Published vacancy: public for everyone, but only its author sees drafts.
    code, _, pub = request(f"/vacancies/{vacancy_id}")
    assert code == 200 and pub["status"] == "published", (code, pub)
    _, other, _ = register_account("smoke-stranger")
    assert request(f"/vacancies/{vacancy_id}", cookie=other)[0] == 200
    code, _, foreign = request("/vacancies", vacancy_fields("Чужой черновик"), cookie=other)
    assert code == 201, (code, foreign)
    assert request(f"/vacancies/{foreign['id']}", cookie=author)[0] == 404, "foreign draft must 404"

    # Close is a one-way transition; closed vacancies leave the public view.
    code, _, body = request(f"/vacancies/{vacancy_id}/close", cookie=author, method="POST")
    assert code == 200 and body["status"] == "closed" and body["closed_at"], (code, body)
    assert request(f"/vacancies/{vacancy_id}")[0] == 404, "closed vacancy must not be public"
    code, _, body = request(f"/vacancies/{vacancy_id}/close", cookie=author, method="POST")
    assert code == 409 and "already closed" in body["error"], (code, body)

    # Published and closed vacancies are history; drafts can be deleted.
    code, _, body = request(f"/vacancies/{vacancy_id}", cookie=author, method="DELETE")
    assert code == 409 and "history" in body["error"], (code, body)
    code, _, _ = request(f"/vacancies/{foreign['id']}", cookie=other, method="DELETE")
    assert code == 200, code
    assert request(f"/vacancies/{foreign['id']}", cookie=other)[0] == 404, "deleted draft must 404"

    print("PASS: vacancy author flow — draft, validation, email gate, optimistic version, publish, visibility, close, delete")


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

    # Cross-account isolation: /sessions shows only the caller's sessions.
    # The first account has two own sessions (register + login); a freshly
    # registered second account must add nothing to that list.
    code, _, body_b = request("/register", {
        "email": "smoke-b-" + uuid.uuid4().hex + "@example.com",
        "password": password,
        "name": "Smoke B",
        "username": "smoke-b-" + uuid.uuid4().hex,
    })
    assert code == 200, (code, body_b)
    code, _, mine = request("/sessions", cookie="token=" + cookie.value)
    assert code == 200 and len(mine["sessions"]) == 2, (code, mine)
    assert body_b["user_id"] not in json.dumps(mine)

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

    vacancy_flow()

    print("PASS: readiness, registration, stable identity, cookie, duplicate, login, invalid credentials, validation, public profile, own sessions, refresh, logout")


if __name__ == "__main__":
    main()
