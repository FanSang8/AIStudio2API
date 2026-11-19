# Server Deployment

Server: `198.23.152.126`, SSH port `22`, user `root`.
Directory: `/opt/aistudio2api`.
Console: `https://ab1.tesaai.xyz`.
OpenAI base URL: `https://ab1.tesaai.xyz/v1`.
HTML preview: `https://ab1.tesaai.xyz:12050/shim.html`.

The container uses an image built from the user's repository and the
Camoufox WAA backend. Account and runtime data are stored in `data/`.
Existing local accounts and credentials are not copied to the server.

## Repository Deployment

Validate and commit locally, then push to `FanSang8/AIStudio2API` first.
The server must fetch the pushed commit and build it; do not SCP source files.
The source checkout lives in `/opt/aistudio2api/source`, separately from
the persistent `data/` directory and installed Compose files.

First deployment:

```sh
git clone https://github.com/FanSang8/AIStudio2API.git /opt/aistudio2api/source
bash /opt/aistudio2api/source/deploy/server/deploy.sh <full-pushed-commit-sha>
```

The script only deploys commits reachable from `origin/main`, checks that
the source checkout is clean, builds before replacing the container, records
the commit in an image label, and restores the previous image on a failed
health check. Existing Nginx and `data/` remain in place.

## R2 Images

Images generation and edit endpoints always return HTTP(S) URLs, including
streaming completion events. Until R2 is configured, images use the service's
local `/media/` directory (1 GB limit). R2 uploads use content hashes and the
final output encoding. Once R2 is configured, upload errors are returned to
the caller instead of falling back to local files or Base64.

Prepare a bucket-scoped **Object Read & Write** S3 API key and bind your
existing custom image domain to the intended R2 bucket. Then run:

```sh
python3 /opt/aistudio2api/configure-r2.py
```

The script asks for the S3 endpoint, bucket, Access Key ID, Secret Access Key,
your custom HTTPS domain, and an object prefix. Keys are entered without
echo. Settings are atomically saved to `data/r2.env` with mode 0600 and loaded
by Docker Compose as raw values; they are not committed or copied into images.
The custom domain is `R2_PUBLIC_BASE_URL`, separate from the S3 API
`R2_ENDPOINT`. `MEDIA_BASE_URL` continues to mean this application's address.

Before restarting the running container, the script checks an authenticated
upload and downloads that image anonymously through the custom domain.
It restores the previous settings if verification fails. This leaves one
small test image under the configured prefix. CORS is unnecessary for
server-side image URL editing, but browser-based tools may require bucket
CORS allowing your application origin to GET/HEAD images.

Subsequent configuration changes can reuse the command; Enter keeps existing
values. There is no need to send the keys in chat.

## Initial Access

The service listens on server loopback. Nginx provides public HTTPS access.
SSH forwarding is also available for direct access to the local listener.

Run this command on the Windows computer:

```powershell
ssh -i "$env:USERPROFILE\.ssh\codex_newapi" -p 22 -N -o ExitOnForwardFailure=yes -L 127.0.0.1:12048:127.0.0.1:12048 -L 127.0.0.1:12050:127.0.0.1:12050 root@198.23.152.126
```

Open `https://ab1.tesaai.xyz`. To configure or change your own credentials,
run this command on the server:

```sh
python3 /opt/aistudio2api/configure-access.py
```

The command prompts for the current credentials when authentication is
already enabled, then asks for the new username, password, and API key.
Secrets are not echoed. Settings are saved through the local management API.
It installs `compose.proxy.yml` as `compose.override.yml` and recreates the
container, retaining the loopback listeners used by Nginx.

Alternatively, configure credentials in the console settings or edit
`data/.env` directly, then restart the container.

## Accounts

After Camoufox is ready, use the console's browser sign-in option. The server
browser's display and input are available in the console's login window.
No desktop or separate VNC service is required.

Add accounts, then start the generation service from the console.
`AUTO_START=false` leaves generation stopped until it is explicitly started.

## Operations

```sh
cd /opt/aistudio2api
docker compose ps
docker compose logs --tail 100
docker compose restart
docker compose stop
```

## Public Access

Nginx site: `/etc/nginx/sites-available/ab1.tesaai.xyz.conf`, enabled through
the corresponding symlink in `/etc/nginx/sites-enabled/`.

The main backend is `127.0.0.1:12048`. Nginx handles HTTPS, WebSocket upgrades
for the login viewer, and unbuffered streaming responses.

The preview backend is `127.0.0.1:12050`. Nginx listens on the public address
`198.23.152.126:12050` with TLS to provide the separate preview origin expected
by the frontend.

The certificate is stored in `/etc/letsencrypt/live/ab1.tesaai.xyz/`.
HTTP-01 renewal uses `/var/www/letsencrypt`. The existing Certbot timer renews
the certificate, and the saved deploy hook reloads Nginx.
