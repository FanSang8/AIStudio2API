#!/usr/bin/env python3
import argparse
import getpass
import http.cookiejar
import json
from pathlib import Path
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request


BASE_URL = "http://127.0.0.1:12048"
ROOT = Path(__file__).resolve().parent
CLIENT = urllib.request.build_opener(
    urllib.request.ProxyHandler({}),
    urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
)


def request(path, payload=None):
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    method = "GET" if data is None else "POST"
    if path == "/api/config" and data is not None:
        method = "PUT"
    req = urllib.request.Request(
        BASE_URL + path,
        data=data,
        headers={"Content-Type": "application/json"},
        method=method,
    )
    with CLIENT.open(req, timeout=30) as response:
        return json.load(response)


def secret(prompt):
    while True:
        value = getpass.getpass(prompt)
        if value.strip():
            return value
        print("A nonempty value is required.")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    if request("/health").get("status") != "ok":
        raise RuntimeError("The service is not healthy.")
    for filename in ("compose.yml", "compose.public.yml"):
        if not (ROOT / filename).is_file():
            raise RuntimeError("Missing deployment file: " + filename)
    subprocess.run(["docker", "compose", "config", "--quiet"], cwd=ROOT, check=True)
    if args.check:
        print("Configuration command is ready. No settings were changed.")
        return

    session = request("/api/auth/session")
    if session["enabled"]:
        current_user = input("Current admin username [admin]: ").strip() or "admin"
        request("/api/auth/login", {
            "username": current_user,
            "password": secret("Current admin password: "),
        })
    config = request("/api/config")
    default_user = config.get("admin_username") or "admin"
    username = input("Admin username [" + default_user + "]: ").strip() or default_user
    password = secret("New admin password: ")
    if password != getpass.getpass("Confirm admin password: "):
        raise RuntimeError("Passwords do not match. No settings were changed.")
    api_key = secret("API key: ").strip()
    config.update(
        admin_auth_enabled=True,
        admin_username=username,
        admin_password=password,
        proxy_api_key=api_key,
    )
    request("/api/config", config)
    proxy_config = ROOT / "compose.proxy.yml"
    behind_proxy = proxy_config.is_file()
    override = proxy_config if behind_proxy else ROOT / "compose.public.yml"
    shutil.copyfile(override, ROOT / "compose.override.yml")
    subprocess.run(
        ["docker", "compose", "up", "-d", "--force-recreate"],
        cwd=ROOT,
        check=True,
    )
    deadline = time.monotonic() + 30
    while True:
        try:
            if request("/api/auth/session")["enabled"]:
                break
        except (OSError, urllib.error.URLError):
            pass
        if time.monotonic() >= deadline:
            raise RuntimeError("Restart did not become ready. Check docker compose logs.")
        time.sleep(1)
    print("Credentials saved and admin authentication enabled.")
    console_url = "https://ab1.tesaai.xyz" if behind_proxy else "http://198.23.152.126:12048"
    print("Console: " + console_url)
    print("Camoufox remains enabled. Add an account, then start generation.")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\nCancelled.", file=sys.stderr)
        sys.exit(1)
    except (OSError, RuntimeError, ValueError, subprocess.CalledProcessError) as error:
        print("Configuration failed: " + str(error), file=sys.stderr)
        sys.exit(1)
