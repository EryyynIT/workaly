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
from urllib.parse import quote
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


def response_flow():
    """The candidate side: respond, one active response, withdraw, respond again."""
    # An author with a confirmed email publishes a vacancy.
    author_email, author, _ = register_account("smoke-author2")
    code, _, draft = request("/vacancies", vacancy_fields("Смоук-вакансия для откликов"), cookie=author)
    assert code == 201, (code, draft)
    vacancy_id = draft["id"]
    assert request("/verify-email", {"token": dev_verify_token(author_email)})[0] == 200
    code, _, body = request(f"/vacancies/{vacancy_id}/publish", cookie=author, method="POST")
    assert code == 200 and body["status"] == "published", (code, body)

    # Responding without a session or without a confirmed email is refused.
    code, _, body = request(f"/vacancies/{vacancy_id}/responses", {"cover_letter": "Здравствуйте!"})
    assert code == 401, (code, body)
    candidate_email, candidate, candidate_id = register_account("smoke-candidate")
    code, _, body = request(f"/vacancies/{vacancy_id}/responses", {"cover_letter": "Здравствуйте!"}, cookie=candidate)
    assert code == 403 and body == {"error": "email_not_verified"}, (code, body)
    assert request("/verify-email", {"token": dev_verify_token(candidate_email)})[0] == 200

    # A draft is not open for responses and its existence stays undisclosed.
    code, _, other_draft = request("/vacancies", vacancy_fields("Черновик без откликов"), cookie=author)
    assert code == 201, (code, other_draft)
    code, _, body = request(f"/vacancies/{other_draft['id']}/responses", {"cover_letter": "hi"}, cookie=candidate)
    assert code == 404, (code, body)
    # The author can never respond to their own vacancy.
    code, _, body = request(f"/vacancies/{vacancy_id}/responses", {"cover_letter": "hi"}, cookie=author)
    assert code == 400 and "own vacancy" in body["error"], (code, body)

    # The candidate responds with a contact channel; a repeat is a 409.
    reply = {"cover_letter": "Пять лет Go, профиль по ссылке.", "contact": "@smoke-candidate"}
    code, _, resp = request(f"/vacancies/{vacancy_id}/responses", reply, cookie=candidate)
    assert code == 201 and resp["status"] == "active" and resp["contact"] == "@smoke-candidate", (code, resp)
    assert resp["candidate_user_id"] == candidate_id, resp
    code, _, body = request(f"/vacancies/{vacancy_id}/responses", reply, cookie=candidate)
    assert code == 409 and "active response" in body["error"], (code, body)

    # The author reads the responses including the contact; a stranger gets 404.
    code, _, body = request(f"/vacancies/{vacancy_id}/responses", cookie=author)
    assert code == 200 and len(body["responses"]) == 1, (code, body)
    assert body["responses"][0]["contact"] == "@smoke-candidate", body
    _, stranger, _ = register_account("smoke-stranger2")
    assert request(f"/vacancies/{vacancy_id}/responses", cookie=stranger)[0] == 404
    # No leak through the public vacancy view or the anonymous surface.
    assert "contact" not in json.dumps(request(f"/vacancies/{vacancy_id}")[2])
    assert request(f"/vacancies/{vacancy_id}/responses")[0] == 401

    # The candidate reads their own response; the author has none of their own.
    code, _, mine = request(f"/vacancies/{vacancy_id}/responses/mine", cookie=candidate)
    assert code == 200 and mine["id"] == resp["id"], (code, mine)
    assert request(f"/vacancies/{vacancy_id}/responses/mine", cookie=author)[0] == 404

    # Withdraw: stranger 404, double withdraw 409, afterwards responding again works.
    assert request(f"/vacancies/{vacancy_id}/responses/{resp['id']}/withdraw", cookie=stranger, method="POST")[0] == 404
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{resp['id']}/withdraw", cookie=candidate, method="POST")
    assert code == 200 and body["status"] == "withdrawn" and body["withdrawn_at"], (code, body)
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{resp['id']}/withdraw", cookie=candidate, method="POST")
    assert code == 409 and "already withdrawn" in body["error"], (code, body)
    assert request(f"/vacancies/{vacancy_id}/responses/mine", cookie=candidate)[0] == 404
    code, _, again = request(f"/vacancies/{vacancy_id}/responses", reply, cookie=candidate)
    assert code == 201 and again["status"] == "active" and again["id"] != resp["id"], (code, again)
    code, _, body = request(f"/vacancies/{vacancy_id}/responses", cookie=author)
    assert code == 200 and len(body["responses"]) == 2, (code, body)

    print("PASS: vacancy response flow — email gate, contact to author only, one active response, withdraw, respond again")


def hiring_flow():
    """The S3 hiring pipeline: reject, offer cycle, hired, close cascade."""
    author_email, author, _ = register_account("smoke-hire-author")
    code, _, draft = request("/vacancies", vacancy_fields("Вакансия с наймом"), cookie=author)
    assert code == 201, (code, draft)
    vacancy_id = draft["id"]
    assert request("/verify-email", {"token": dev_verify_token(author_email)})[0] == 200
    assert request(f"/vacancies/{vacancy_id}/publish", cookie=author, method="POST")[0] == 200

    candidate_email, candidate, _ = register_account("smoke-hire-candidate")
    assert request("/verify-email", {"token": dev_verify_token(candidate_email)})[0] == 200
    other_email, other, _ = register_account("smoke-hire-other")
    assert request("/verify-email", {"token": dev_verify_token(other_email)})[0] == 200
    _, stranger, _ = register_account("smoke-hire-stranger")

    reply = {"cover_letter": "Готов пройти все этапы.", "contact": "@hire-me"}
    code, _, resp = request(f"/vacancies/{vacancy_id}/responses", reply, cookie=candidate)
    assert code == 201, (code, resp)
    rid = resp["id"]

    # The author cannot send an offer before reading: a stranger gets 404.
    assert request(f"/vacancies/{vacancy_id}/responses/{rid}/offer", cookie=stranger, method="POST")[0] == 404
    # The candidate cannot steer the author's side.
    assert request(f"/vacancies/{vacancy_id}/responses/{rid}/offer", cookie=candidate, method="POST")[0] == 404
    assert request(f"/vacancies/{vacancy_id}/responses/{rid}/reject", {}, cookie=candidate, method="POST")[0] == 404

    # Offer -> decline with a reason; the author sees it, the candidate sees it too.
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{rid}/offer", cookie=author, method="POST")
    assert code == 200 and body["status"] == "offer_sent", (code, body)
    code, _, mine = request(f"/vacancies/{vacancy_id}/responses/mine", cookie=candidate)
    assert code == 200 and mine["status"] == "offer_sent", (code, mine)
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{rid}/offer", cookie=author, method="POST")
    assert code == 409 and "already offer_sent" in body["error"], (code, body)
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{rid}/offer/decline",
                            {"reason": "низкая ставка"}, cookie=candidate, method="POST")
    assert code == 200 and body["status"] == "offer_declined" and body["reason"] == "низкая ставка", (code, body)
    assert body["decided_at"], body
    code, _, body = request(f"/vacancies/{vacancy_id}/responses", cookie=author)
    assert code == 200 and body["responses"][0]["status"] == "offer_declined", (code, body)
    assert body["responses"][0]["reason"] == "низкая ставка", body

    # A declined offer frees the slot but not the response: re-offer is a
    # conflict, the response is terminal, and withdrawal from a terminal
    # state is a conflict too (history stays untouched).
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{rid}/offer", cookie=author, method="POST")
    assert code == 409 and "requires active" in body["error"], (code, body)
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{rid}/withdraw", cookie=candidate, method="POST")
    assert code == 409 and "cannot be withdrawn" in body["error"], (code, body)

    # Fresh response -> offer -> withdraw offer -> reject with a reason.
    code, _, resp = request(f"/vacancies/{vacancy_id}/responses", reply, cookie=candidate)
    assert code == 201, (code, resp)
    rid = resp["id"]
    assert request(f"/vacancies/{vacancy_id}/responses/{rid}/offer", cookie=author, method="POST")[0] == 200
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{rid}/offer/withdraw", cookie=author, method="POST")
    assert code == 200 and body["status"] == "active", (code, body)
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{rid}/reject",
                            {"reason": "профиль не совпал"}, cookie=author, method="POST")
    assert code == 200 and body["status"] == "rejected", (code, body)
    assert "reason" not in body, body  # the author's view never echoes their own reject reason
    code, _, mine = request(f"/vacancies/{vacancy_id}/responses/mine", cookie=candidate)
    assert code == 200 and mine["status"] == "rejected" and mine["reason"] == "профиль не совпал", (code, mine)
    # Rejected is terminal: no offer, no re-reject.
    assert request(f"/vacancies/{vacancy_id}/responses/{rid}/offer", cookie=author, method="POST")[0] == 409
    assert request(f"/vacancies/{vacancy_id}/responses/{rid}/reject", {}, cookie=author, method="POST")[0] == 409

    # Full cycle to hired: offer -> accept -> hired; hired is terminal.
    code, _, resp = request(f"/vacancies/{vacancy_id}/responses", reply, cookie=other)
    assert code == 201, (code, resp)
    other_rid = resp["id"]
    assert request(f"/vacancies/{vacancy_id}/responses/{other_rid}/offer", cookie=author, method="POST")[0] == 200
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{other_rid}/offer/accept", cookie=other, method="POST")
    assert code == 200 and body["status"] == "offer_accepted", (code, body)
    code, _, body = request(f"/vacancies/{vacancy_id}/responses/{other_rid}/hired", cookie=author, method="POST")
    assert code == 200 and body["status"] == "hired" and body["decided_at"], (code, body)
    # Hired is terminal for every side: no withdraw, no reject, no re-hire.
    assert request(f"/vacancies/{vacancy_id}/responses/{other_rid}/withdraw", cookie=other, method="POST")[0] == 409
    assert request(f"/vacancies/{vacancy_id}/responses/{other_rid}/reject", {}, cookie=author, method="POST")[0] == 409
    assert request(f"/vacancies/{vacancy_id}/responses/{other_rid}/hired", cookie=author, method="POST")[0] == 409

    print("PASS: hiring flow — 404s, offer/decline reasons, withdraw-offer, reject, hired terminal")

    # Closing a vacancy force-rejects the outstanding offer atomically.
    author2_email, author2, _ = register_account("smoke-hire-author2")
    code, _, draft2 = request("/vacancies", vacancy_fields("Вакансия с каскадом"), cookie=author2)
    assert code == 201, (code, draft2)
    vacancy2_id = draft2["id"]
    assert request("/verify-email", {"token": dev_verify_token(author2_email)})[0] == 200
    assert request(f"/vacancies/{vacancy2_id}/publish", cookie=author2, method="POST")[0] == 200
    cand2_email, cand2, _ = register_account("smoke-hire-cand2")
    assert request("/verify-email", {"token": dev_verify_token(cand2_email)})[0] == 200
    code, _, resp = request(f"/vacancies/{vacancy2_id}/responses", reply, cookie=cand2)
    assert code == 201, (code, resp)
    rid2 = resp["id"]
    assert request(f"/vacancies/{vacancy2_id}/responses/{rid2}/offer", cookie=author2, method="POST")[0] == 200
    code, _, body = request(f"/vacancies/{vacancy2_id}/close", cookie=author2, method="POST")
    assert code == 200 and body["status"] == "closed", (code, body)
    code, _, mine = request(f"/vacancies/{vacancy2_id}/responses/mine", cookie=cand2)
    assert code == 200 and mine["status"] == "rejected" and mine["reason"] == "vacancy_closed", (code, mine)

    print("PASS: hiring cascade — closing a vacancy force-rejects the outstanding offer")

    # A closed vacancy stops the pipeline: an active response left as history
    # accepts neither an offer nor a reject (409, never a silent transition).
    author3_email, author3, _ = register_account("smoke-hire-author3")
    code, _, draft3 = request("/vacancies", vacancy_fields("Вакансия с активным хвостом"), cookie=author3)
    assert code == 201, (code, draft3)
    vacancy3_id = draft3["id"]
    assert request("/verify-email", {"token": dev_verify_token(author3_email)})[0] == 200
    assert request(f"/vacancies/{vacancy3_id}/publish", cookie=author3, method="POST")[0] == 200
    cand3_email, cand3, _ = register_account("smoke-hire-cand3")
    assert request("/verify-email", {"token": dev_verify_token(cand3_email)})[0] == 200
    code, _, resp = request(f"/vacancies/{vacancy3_id}/responses", reply, cookie=cand3)
    assert code == 201, (code, resp)
    rid3 = resp["id"]
    assert request(f"/vacancies/{vacancy3_id}/close", cookie=author3, method="POST")[0] == 200
    code, _, body = request(f"/vacancies/{vacancy3_id}/responses/{rid3}/offer", cookie=author3, method="POST")
    assert code == 409 and "pipeline stopped" in body["error"], (code, body)
    code, _, body = request(f"/vacancies/{vacancy3_id}/responses/{rid3}/reject", {}, cookie=author3, method="POST")
    assert code == 409 and "pipeline stopped" in body["error"], (code, body)
    # Withdrawal from active stays possible after the close: the candidate
    # keeps control of their own record.
    code, _, body = request(f"/vacancies/{vacancy3_id}/responses/{rid3}/withdraw", cookie=cand3, method="POST")
    assert code == 200 and body["status"] == "withdrawn", (code, body)

    print("PASS: hiring pipeline stops on a closed vacancy; withdrawal stays possible")


def order_fields(title):
    return {
        "title": title,
        "description": "Одностраничный сайт с формой заказа и оплатой.",
        "budget_type": "fixed",
        "budget_amount": "120000",
        "currency": "RUB",
        "deadline": "2026-12-31",
    }


def order_flow():
    """The order side: author CRUD and the executor's proposal flow."""
    customer_email, customer, _ = register_account("smoke-customer")
    code, _, draft = request("/orders", order_fields("Лендинг для кофейни"), cookie=customer)
    assert code == 201 and draft["status"] == "draft" and draft["budget_amount"] == "120000.00", (code, draft)
    order_id = draft["id"]

    # Validation stays precise; publishing requires a confirmed email.
    code, _, body = request("/orders", {**order_fields("x"), "budget_amount": "0"}, cookie=customer)
    assert code == 400 and "budget_amount" in body["error"], (code, body)
    code, _, body = request(f"/orders/{order_id}/publish", cookie=customer, method="POST")
    assert code == 403 and body == {"error": "email_not_verified"}, (code, body)
    assert request("/verify-email", {"token": dev_verify_token(customer_email)})[0] == 200

    # Optimistic version, then publish; drafts stay invisible to the public.
    stale = {**order_fields("Обновлённый заказ"), "version": 99}
    code, _, body = request(f"/orders/{order_id}", stale, cookie=customer, method="PATCH")
    assert code == 409 and "changed since you last read it" in body["error"], (code, body)
    code, _, body = request(f"/orders/{order_id}", {**order_fields("Обновлённый заказ"), "version": 1}, cookie=customer, method="PATCH")
    assert code == 200 and body["version"] == 2, (code, body)
    assert request(f"/orders/{order_id}")[0] == 404, "draft must not be public"
    code, _, body = request(f"/orders/{order_id}/publish", cookie=customer, method="POST")
    assert code == 200 and body["status"] == "published" and body["published_at"], (code, body)
    assert request(f"/orders/{order_id}")[0] == 200, "published order must be public"

    # The executor proposes; email gate, then one active proposal per order.
    executor_email, executor, executor_id = register_account("smoke-executor")
    proposal = {"message": "Сделаю за неделю, примеры в профиле.", "proposed_amount": "90000", "currency": "RUB"}
    code, _, body = request(f"/orders/{order_id}/proposals", proposal, cookie=executor)
    assert code == 403 and body == {"error": "email_not_verified"}, (code, body)
    assert request("/verify-email", {"token": dev_verify_token(executor_email)})[0] == 200
    code, _, prop = request(f"/orders/{order_id}/proposals", proposal, cookie=executor)
    assert code == 201 and prop["status"] == "active" and prop["proposed_amount"] == "90000.00", (code, prop)
    assert prop["executor_user_id"] == executor_id, prop
    code, _, body = request(f"/orders/{order_id}/proposals", proposal, cookie=executor)
    assert code == 409 and "active proposal" in body["error"], (code, body)

    # The author can never propose for their own order; a stranger and the
    # anonymous get 404/401 on the proposal list.
    code, _, body = request(f"/orders/{order_id}/proposals", proposal, cookie=customer)
    assert code == 400 and "own order" in body["error"], (code, body)
    code, _, body = request(f"/orders/{order_id}/proposals", cookie=customer)
    assert code == 200 and len(body["proposals"]) == 1, (code, body)
    _, stranger, _ = register_account("smoke-stranger3")
    assert request(f"/orders/{order_id}/proposals", cookie=stranger)[0] == 404
    assert request(f"/orders/{order_id}/proposals")[0] == 401
    # Proposals never leak into the public order view.
    assert "proposals" not in json.dumps(request(f"/orders/{order_id}")[2])

    # Withdraw: stranger 404, double withdraw 409, afterwards proposing again works.
    assert request(f"/orders/{order_id}/proposals/{prop['id']}/withdraw", cookie=stranger, method="POST")[0] == 404
    code, _, body = request(f"/orders/{order_id}/proposals/{prop['id']}/withdraw", cookie=executor, method="POST")
    assert code == 200 and body["status"] == "withdrawn" and body["withdrawn_at"], (code, body)
    code, _, body = request(f"/orders/{order_id}/proposals/{prop['id']}/withdraw", cookie=executor, method="POST")
    assert code == 409 and "already withdrawn" in body["error"], (code, body)
    assert request(f"/orders/{order_id}/proposals/mine", cookie=executor)[0] == 404
    code, _, again = request(f"/orders/{order_id}/proposals", proposal, cookie=executor)
    assert code == 201 and again["id"] != prop["id"], (code, again)

    # Close is terminal: the order leaves the public view and stops accepting
    # proposals (the same undisclosing 404 as a draft).
    code, _, body = request(f"/orders/{order_id}/close", cookie=customer, method="POST")
    assert code == 200 and body["status"] == "closed", (code, body)
    assert request(f"/orders/{order_id}")[0] == 404, "closed order must not be public"
    code, _, body = request(f"/orders/{order_id}/proposals", proposal, cookie=executor)
    assert code == 404, (code, body)
    code, _, body = request("/my/orders", cookie=customer)
    assert code == 200 and [o["id"] for o in body["orders"]] == [order_id], (code, body)

    print("PASS: order flow — author CRUD, email gate, optimistic version, one active proposal, withdraw, close")


def catalog_flow():
    """The public catalog: anonymous listing, filters, keyset pagination."""
    # Publish three vacancies with distinct attributes to filter by. The
    # catalog is shared with every earlier smoke run, so all assertions
    # match against the ids of this run instead of counting rows.
    email, author, _ = register_account("smoke-catalog")
    assert request("/verify-email", {"token": dev_verify_token(email)})[0] == 200
    vacancies = []
    for title, et, wf, lo, hi in [
        ("Go-разработчик платёжного шлюза", "full_time", "remote", "100000", "200000"),
        ("Python backend для маркетплейса", "part_time", "hybrid", "80000", "120000"),
        ("DevOps в офис, релизы", "full_time", "onsite", "", ""),
    ]:
        fields = vacancy_fields(title)
        fields["employment_type"] = et
        fields["work_format"] = wf
        fields["salary_min"] = lo
        fields["salary_max"] = hi
        if not lo:
            fields.pop("salary_min")
            fields.pop("salary_max")
            fields.pop("currency")
        code, _, draft = request("/vacancies", fields, cookie=author)
        assert code == 201, (code, draft)
        code, _, body = request(f"/vacancies/{draft['id']}/publish", cookie=author, method="POST")
        assert code == 200, (code, body)
        vacancies.append(draft["id"])
    go_vacancy, python_vacancy, devops_vacancy = vacancies

    def listed_ids(query=""):
        """Collects all pages of the public catalog and returns the ids."""
        ids, token = [], ""
        for _ in range(100):  # guard against a broken next token
            code, _, body = request("/vacancies?page_size=50&page_token=" + token + query)
            assert code == 200, (code, body)
            ids.extend(v["id"] for v in body["vacancies"])
            token = body["next_page_token"]
            if not token:
                return ids
        raise AssertionError("pagination did not terminate in 100 pages")

    # The public list contains only published vacancies. published_at has
    # second-level resolution and identical timestamps tie-break by random
    # UUID, so the relative order of same-run rows is not asserted.
    all_ids = listed_ids()
    assert go_vacancy in all_ids and python_vacancy in all_ids and devops_vacancy in all_ids, all_ids
    assert len(all_ids) == len(set(all_ids)), "a vacancy must never appear twice across pages"

    # Filters narrow the list as expected; other runs' rows are filtered out
    # by id-membership instead of a global count.
    full_time = listed_ids("&employment_type=full_time")
    assert go_vacancy in full_time and devops_vacancy in full_time and python_vacancy not in full_time, full_time
    onsite = listed_ids("&work_format=onsite")
    assert devops_vacancy in onsite and go_vacancy not in onsite, onsite
    titled = listed_ids("&title_contains=Python")
    assert python_vacancy in titled and go_vacancy not in titled, titled
    salaried = listed_ids("&salary_min_from=90000&salary_max_to=210000")
    # Only the Go vacancy fits; Python's max is too low, DevOps has no
    # salary at all (NULL comparisons fail).
    assert go_vacancy in salaried and python_vacancy not in salaried and devops_vacancy not in salaried, salaried

    # Invalid filters and page_size are rejected with precise 400s.
    for query, fragment in [
        ("?employment_type=shift", "employment_type"),
        ("?page_size=0", "between 1 and 50"),
        ("?page_size=51", "between 1 and 50"),
        ("?page_size=abc", "between 1 and 50"),
        ("?page_token=garbage", "page_token"),
    ]:
        code, _, body = request("/vacancies" + query)
        assert code == 400 and fragment in body["error"], (query, code, body)

    # ILIKE metacharacters in the search term are literal, not wildcards:
    # "%" alone would match every title if it reached ILIKE unescaped.
    pct_hits = listed_ids("&title_contains=%25")  # URL-encoded "%"
    assert go_vacancy not in pct_hits and python_vacancy not in pct_hits, pct_hits

    # Keyset pagination actually pages: page 2 starts strictly after page 1.
    code, _, page1 = request("/vacancies?page_size=1")
    assert code == 200 and len(page1["vacancies"]) == 1 and page1["next_page_token"], page1
    code, _, page2 = request(f"/vacancies?page_size=1&page_token={page1['next_page_token']}")
    assert code == 200 and len(page2["vacancies"]) == 1, page2
    assert page2["vacancies"][0]["id"] != page1["vacancies"][0]["id"], (page1, page2)

    # Publish one order and check the mirrored public catalog.
    email2, customer, _ = register_account("smoke-catalog-order")
    assert request("/verify-email", {"token": dev_verify_token(email2)})[0] == 200
    code, _, draft = request("/orders", order_fields("Сайт для каталога"), cookie=customer)
    assert code == 201, (code, draft)
    order_id = draft["id"]
    code, _, body = request(f"/orders/{order_id}/publish", cookie=customer, method="POST")
    assert code == 200, (code, body)

    def listed_order_ids(query=""):
        code, _, body = request("/orders" + query)
        assert code == 200, (code, body)
        return [o["id"] for o in body["orders"]]

    assert order_id in listed_order_ids(), "published order must be listed"
    assert order_id not in listed_order_ids("?budget_type=hourly")
    assert order_id in listed_order_ids("?budget_min=50000&budget_max=200000")
    assert order_id in listed_order_ids("?title_contains=" + quote("каталога"))
    code, _, body = request("/orders?budget_type=daily")
    assert code == 400 and "budget_type" in body["error"], (code, body)

    print("PASS: public catalog — anonymous listing, filters, pagination")


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
    response_flow()
    hiring_flow()
    order_flow()
    catalog_flow()

    print("PASS: readiness, registration, stable identity, cookie, duplicate, login, invalid credentials, validation, public profile, own sessions, refresh, logout")


if __name__ == "__main__":
    main()
