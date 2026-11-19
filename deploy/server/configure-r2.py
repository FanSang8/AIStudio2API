#!/usr/bin/env python3
"""Configure R2 without exposing keys in shell history or the repository."""

import getpass
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import urllib.request
from urllib.parse import urlparse


ROOT = Path(__file__).resolve().parent
CONFIG = ROOT / "data" / "r2.env"
FIELDS = (
    "R2_ENDPOINT", "R2_BUCKET", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY",
    "R2_PUBLIC_BASE_URL", "R2_KEY_PREFIX",
)


def read_settings():
    if not CONFIG.exists():
        return {}
    return dict(
        line.split("=", 1) for line in CONFIG.read_text().splitlines()
        if line and not line.startswith("#") and "=" in line
    )


def write_settings(content):
    CONFIG.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=CONFIG.parent, delete=False) as temporary:
        path = Path(temporary.name)
        try:
            os.chmod(path, 0o600)
            temporary.write(content)
            temporary.flush()
            os.fsync(temporary.fileno())
        except BaseException:
            path.unlink(missing_ok=True)
            raise
    try:
        os.replace(path, CONFIG)
    finally:
        path.unlink(missing_ok=True)


def prompt(name, current):
    if name in ("R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY"):
        suffix = " [Enter keeps existing]" if current else ""
        value = getpass.getpass(name + suffix + ": ").strip() or current
    else:
        suffix = " [" + current + "]" if current else ""
        value = input(name + suffix + ": ").strip() or current
    if not value or any(character in value for character in "\r\n\x00"):
        raise ValueError(name + " must be nonempty and contain no newlines.")
    return value


def wait_healthy():
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deadline = time.monotonic() + 40
    while time.monotonic() < deadline:
        try:
            with client.open("http://127.0.0.1:12048/health", timeout=2) as response:
                if response.status == 200:
                    return
        except OSError:
            pass
        time.sleep(1)
    raise RuntimeError("The service did not become healthy.")


def main():
    if not (ROOT / "compose.yml").exists():
        raise RuntimeError("Run the installed script under /opt/aistudio2api.")
    subprocess.run(["docker", "compose", "config", "--quiet"], cwd=ROOT, check=True)
    current = read_settings()
    current.setdefault("R2_KEY_PREFIX", "aistudio/images")
    print("Use a bucket-scoped R2 Object Read & Write S3 API key.")
    print("R2_PUBLIC_BASE_URL is your bucket's custom HTTPS domain, not its S3 endpoint.")
    settings = {name: prompt(name, current.get(name, "")) for name in FIELDS}
    for name in ("R2_ENDPOINT", "R2_PUBLIC_BASE_URL"):
        parsed = urlparse(settings[name])
        if parsed.scheme != "https" or not parsed.hostname or parsed.query or parsed.fragment:
            raise ValueError(name + " must be an HTTPS URL without a query or fragment.")
    previous = CONFIG.read_bytes() if CONFIG.exists() else None
    content = "".join(name + "=" + settings[name] + "\n" for name in FIELDS).encode()
    write_settings(content)
    restarting = False
    try:
        # Check the proposed configuration before replacing the running container.
        subprocess.run(
            ["docker", "compose", "run", "--rm", "--no-deps", "-T",
             "aistudio2api", "aistudio2api", "r2-check"],
            cwd=ROOT, check=True,
        )
        restarting = True
        subprocess.run(
            ["docker", "compose", "up", "-d", "--no-deps", "--force-recreate", "aistudio2api"],
            cwd=ROOT, check=True,
        )
        wait_healthy()
    except BaseException:
        if previous is None:
            CONFIG.unlink(missing_ok=True)
        else:
            write_settings(previous)
        if restarting:
            subprocess.run(
                ["docker", "compose", "up", "-d", "--no-deps", "--force-recreate", "aistudio2api"],
                cwd=ROOT, check=False,
            )
        raise
    print("R2 configured. Upload and custom-domain image download both passed.")
    print("Image URL prefix: " + settings["R2_PUBLIC_BASE_URL"].rstrip("/") +
          "/" + settings["R2_KEY_PREFIX"].strip("/") + "/")
    print("The check leaves one small test image in the bucket.")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\nCancelled; previous settings restored.", file=sys.stderr)
        sys.exit(1)
    except (OSError, RuntimeError, ValueError, subprocess.CalledProcessError) as error:
        print("R2 configuration failed: " + str(error), file=sys.stderr)
        sys.exit(1)
