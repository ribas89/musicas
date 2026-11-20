# Musicas

Musicas is a customized [Navidrome](https://github.com/navidrome/navidrome) music server with Authentik login, shared and private libraries, folder-based playlists, file management, native lyrics retrieval, and YouTube Music playlist synchronization through [MusicasYT](https://github.com/ribas89/musicasyt).

It retains Navidrome's web player, music scanner, Subsonic/OpenSubsonic API, and normal playback features. The additions are integrated into the Navidrome backend and web interface, except for YouTube downloads, which are handled by the optional MusicasYT worker.

## Requirements

- Docker with Docker Compose
- An Authentik OAuth2/OIDC provider for user sign-in
- Persistent directories for application data and music
- A reverse proxy for HTTPS access (the included Compose example uses Traefik)
- An existing Docker network shared with the reverse proxy
- Optional: a running [MusicasYT](https://github.com/ribas89/musicasyt) worker for YouTube Music synchronization

The container runs as **UID:GID `1000:1000`**. Its `/data` directory must be writable; `/music` must be readable, and private libraries must be writable for uploads, lyrics, and playlist downloads.

The Dockerfile builds the React interface and Go backend (Go **1.27**). You do not need a local Go or Node.js installation when building with Docker.

## Configuration

The repository includes `.env.example` and `docker-compose.oidc.yml`. One possible directory layout is:

```text
musicas/
├── .env                     Private deployment settings
├── build/                   Musicas source and Docker Compose file
├── data/                    Persistent Navidrome data and SQLite database
└── music/
    ├── public/              Shared music library
    └── users/               Private music libraries
        └── alice/
```

From the parent `musicas/` directory, create the required folders and copy the environment template:

```bash
mkdir -p data music/public music/users
cp build/.env.example .env
chmod 600 .env
```

Make the directories writable by the container user where needed; on a Linux host, for example:

```bash
sudo chown -R 1000:1000 data music/users
```

Configure an **OAuth2/OIDC confidential provider** in Authentik using the authorization-code flow, PKCE, and RS256 signing. Register your exact public callback URL:

```text
https://music.example.com/auth/oidc/callback
```

Set its issuer URL, client ID, and client secret in `.env`. The OIDC `preferred_username` claim identifies the corresponding private library. Use a stable, filesystem-safe username; changing it later requires an explicit library migration. Do not enable a separate forward-auth layer for Musicas.

On first Authentik login, Musicas provisions the Navidrome user and private library and asks for a **separate Navidrome password** for Subsonic clients. The local bootstrap administrator is configured with `ADMIN_USER` and `ADMIN_PASSWORD`.

## Environment example

```dotenv
# Authentik OAuth2/OIDC
OIDC_ISSUER_URL=https://auth.example.com/application/o/musicas/
OIDC_CLIENT_ID=replace-with-client-id
OIDC_CLIENT_SECRET=replace-with-client-secret
OIDC_REDIRECT_URL=https://music.example.com/auth/oidc/callback

# Initial local administrator
ADMIN_USER=admin
ADMIN_PASSWORD=replace-with-a-strong-password

# Navidrome and music directories
ND_PORT=4553
ND_MUSICFOLDER=/music/public
MUSIC_USERS_ROOT=/music/users
ND_AUTOIMPORTPLAYLISTS=false

# Native LRCLIB-compatible lyrics retrieval
LYRICS_URL=https://lrclib.net
LYRICS_INTERVAL=24h

# Optional YouTube Music worker
MUSICASYT_URL=https://youtube-worker.example.com
MUSICASYT_API_KEY=replace-with-the-worker-shared-key
```

`OIDC_REDIRECT_URL` must exactly match the callback registered in Authentik and the public host routed by Traefik. The issuer must match Authentik's discovery metadata. Keep the credentials and API key in the server-side `.env`, not in the React application or Git.

`MUSIC_USERS_ROOT`, `LYRICS_URL`, `LYRICS_INTERVAL`, and `MUSICASYT_*` are Musicas-specific settings. `MUSICAS_HOST`, `TRAEFIK_NETWORK`, and `MUSICAS_ROOT` are used by the Docker Compose example.

`LYRICS_INTERVAL` accepts Go durations such as `6h`, `12h`, or `24h`; use `0` to disable scheduled lyrics scans. The YouTube page requires both `MUSICASYT_URL` and `MUSICASYT_API_KEY` and is otherwise unavailable for synchronization.

## Docker Compose

The included `docker-compose.oidc.yml` builds from the current source, mounts persistent data, and uses an external reverse-proxy network. Its essential configuration is:

```yaml
services:
  musica:
    image: musicas:local
    build:
      context: .
    user: "1000:1000"
    env_file:
      - ../.env
    volumes:
      - ../data:/data
      - ../music:/music
    restart: unless-stopped
    networks:
      - reverse_proxy
    labels:
      - traefik.enable=true
      - traefik.docker.network=traefik
      - traefik.http.routers.musica.rule=Host(`music.example.com`)
      - traefik.http.routers.musica.service=musica
      - traefik.http.services.musica.loadbalancer.server.port=4553

networks:
  reverse_proxy:
    external: true
    name: traefik
```

This is an illustration of the deployment settings; use the repository's `docker-compose.oidc.yml` to start the service. The external network must exist and Traefik must be connected to it. Replace all example hostnames and secrets before deploying.

From `build/`, run:

```bash
docker compose --env-file ../.env -f docker-compose.oidc.yml up -d --build
```

View the logs with:

```bash
docker compose --env-file ../.env -f docker-compose.oidc.yml logs -f musica
```

After a source update, rebuild and recreate the container, then hard-refresh the browser. **Keep the existing `/data` and `/music` directories when updating Musicas.** Do not run multiple Musicas instances against the same SQLite database.

## Music libraries and playlists

Musicas uses Navidrome's native library system:

- `/music/public` is the shared **Public** library.
- `/music/users/<username>` is the private library associated with an Authentik user.
- A user can access the shared library and their own private library; administrator permissions may provide wider access.
- Each direct child folder in a private library becomes a **private native smart playlist** after scanning. Nested files are included in that playlist.

For example, adding `music/users/alice/Road Trip/track.mp3` and scanning Alice's library creates a private **Road Trip** playlist. Public folders are not converted into managed playlists.

These playlists live in Navidrome's normal repository, not in generated `.nsp` or `.m3u` files. `ND_AUTOIMPORTPLAYLISTS=false` disables the separate file-based playlist importer.

## Files

The **Files** page manages only the signed-in user's private library. It supports folder browsing and creation, file or folder uploads, ZIP extraction, deletion, targeted library scans, and folder ZIP downloads (including available transcoding options).

Uploaded music must be indexed by a library scan before it appears in the player or indexed folder downloads. Deletion is permanent. A local administrator without a provisioned private library may not have access to this page.

## Lyrics

The **Lyrics** page is part of Musicas itself; it does not require another service, container, job queue, or lyrics database. Musicas contacts an LRCLIB-compatible server using `LYRICS_URL`.

- **Automatic scans:** scan one indexed song at a time, skipping songs with existing supported lyrics. Manual folder scans include subfolders; **Scan all folders** covers the signed-in user's private library. Scheduled scans cover all libraries.
- **One active scan:** scheduled and manual scans share a single in-memory lock. The page shows the scan status and, for the user's own scan, progress information.
- **Manual search:** search LRCLIB by title or artist, preview the results, find a song in your private library, and explicitly attach the selected lyrics. Manual selection can replace an existing sidecar and remains available during a scan.
- **Storage:** synchronized lyrics are saved as `.lrc`; plain lyrics as `.txt`, beside the audio file with the same basename. The audio file is not renamed or rewritten.
- **Playback:** the player uses Navidrome's native lyrics support, including the Subsonic `getLyricsBySongId` endpoint and its configured `LyricsPriority`.

For example:

```text
/music/users/alice/Road Trip/
├── Song.mp3
└── Song.lrc
```

Existing supported sidecars are not overwritten by automatic scans. `LYRICS_INTERVAL=0` disables the scheduled scan, but manual search and scans remain available.

## YouTube Music

The **YouTube** page can synchronize supported YouTube Music playlists using the optional, separate [MusicasYT](https://github.com/ribas89/musicasyt) worker. **YouTube downloads are not performed by the Navidrome process.**

Musicas authenticates the user, sends synchronization requests from the Go backend using the shared API key, and displays job progress. The worker downloads into the same per-user music directories mounted in both containers; Musicas requests a private-library scan when a job completes.

The worker sees those folders at `/users/<username>`, while Musicas sees them at `/music/users/<username>`. Both mounts must reference the **same host directory**. Playlist state is stored in each user's `musicasyt.json`; the YouTube page reads it without needing to start a synchronization job. Unavailable and failed tracks in playlist cards link to their YouTube Music videos when a video ID is recorded.

Configure `MUSICASYT_API_KEY` with the **same secret** in Musicas and MusicasYT. Generate one with:

```bash
openssl rand -hex 32
```

The worker's full installation and API instructions are in the [MusicasYT repository](https://github.com/ribas89/musicasyt).

## APIs

Musicas retains Navidrome's Subsonic/OpenSubsonic API for playback and client integrations. Custom routes are authenticated and operate within the signed-in user's library permissions:

| Route | Purpose |
| --- | --- |
| `/api/files/*` | Private file management and library scans |
| `/api/lyrics/status`, `/api/lyrics/folders`, `/api/lyrics/scan` | Lyrics scan status and controls |
| `/api/lyrics/search`, `/api/lyrics/songs`, `/api/lyrics/apply` | Manual lyrics search and attachment |
| `/api/youtube/status`, `/api/youtube/playlists` | Worker status and user playlist snapshots |
| `/api/youtube/sync`, `/api/youtube/jobs/{playlistID}` | Start and monitor playlist synchronization |
| `DELETE /api/youtube/playlists/{playlistID}` | Delete a synchronized playlist and its files |

## Troubleshooting

| Problem | Check |
| --- | --- |
| Authentik login fails | Verify issuer, RS256 provider, `preferred_username`, client credentials, and the exact HTTPS callback URL. |
| Files cannot create or upload | Verify UID:GID `1000:1000` has write access to the private library. |
| A private playlist is missing | Confirm the folder is directly under the user's private library and run a library scan. |
| Lyrics do not appear | Confirm a matching sidecar exists, check `LyricsPriority`, and inspect `getLyricsBySongId` in browser Network tools. |
| YouTube synchronization is unavailable | Verify worker reachability, the matching API key, and the shared user-library mount. |
| Changes do not appear after deployment | Rebuild the Docker image and hard-refresh the browser. |

For backend diagnostics, set `ND_LOGLEVEL=debug` temporarily and inspect the container logs. Debug logs may include music filenames and filesystem paths; do not publish full logs without reviewing them.

## Upstream and license

Musicas is a fork of [Navidrome Music Server](https://github.com/navidrome/navidrome), not an official Navidrome release. The original upstream license, notices, and project documentation remain in the source tree. See [Navidrome documentation](https://www.navidrome.org/docs/) for standard player, scanner, and Subsonic functionality.
