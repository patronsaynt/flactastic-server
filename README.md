# FLACtastic Server

A multi-user music server for FLACtastic clients.

- Debian 13 (trixie), amd64 or arm64. A Raspberry Pi 4 or 5 is enough.
- One self-contained binary. TagLib 2.3.2 and SQLite are built in.
- Clients speak [Server Protocol v2](docs/server-protocol-v2.md) ([OpenAPI](docs/openapi.yaml)).

## Install

Download the installer, read it, then run it:

```bash
curl -fsSLO https://raw.githubusercontent.com/patronsaynt/flactastic-server/main/install.sh
```

```bash
sudo bash install.sh
```

It checks the system, installs `ffmpeg` and `avahi-daemon`, verifies the server binary against the release's `SHA256SUMS`, sets up a hardened systemd service, and walks you through the first account. It ends with the server's address and its **fingerprint**: the first time a device connects, FLACtastic shows that fingerprint, and you confirm it matches.

Re-running the installer upgrades in place. `--uninstall` removes the server and keeps its data and your music; `--purge` also removes its data and asks before touching anything in a music folder. `sudo bash install.sh --help` lists the options for unattended installs.

## First run

The music folder needs to be readable and writable by the `flactastic` user. If it's a network share, mount it at boot (an `fstab` line with `_netdev`, or a systemd mount unit). While a share is missing the library shows as offline and nothing is lost.

Add more people with the CLI:

```bash
sudo flactastic-server user add sam --root /srv/music/sam
```

Each user has their own library folder. Folders can't overlap.

## CLI

```
flactastic-server info                       address, fingerprint, version
flactastic-server user add NAME --root PATH [--admin] [--quota 500G]
flactastic-server user list
flactastic-server user passwd NAME           also signs out NAME's devices
flactastic-server user set NAME [--root PATH] [--quota SIZE|none] [--admin=true|false]
flactastic-server user del NAME              never touches their music
flactastic-server device list [NAME]
flactastic-server device revoke ID
flactastic-server session [--user NAME]      live board of active streams
flactastic-server session log                streams that have finished
flactastic-server scan [NAME]                rescan now
flactastic-server rebuild [NAME]             re-read every file's tags
flactastic-server backup DIR                 copy every database, safe while running
flactastic-server version
```

Run these as root. Logs go to the journal: `journalctl -u flactastic-server`.

The index can always be rebuilt from the music folders. Track IDs live with the files (an extended attribute, or `.flactastic/track-ids.json`), so a rebuild keeps playlists and history pointing at the right tracks. Back up `/var/lib/flactastic-server` with `backup` to keep accounts, playlists, preferences and history.

## Building

Requires Go 1.27, a C++ compiler, `cmake` and `zlib1g-dev`.

```bash
scripts/build-taglib.sh
```

```bash
go test ./...
```

```bash
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" .
```

`build-taglib.sh` fetches TagLib 2.3.2, checks it against the same SHA-256 the Mac app pins, and builds it statically into `.deps/`.

## Layout

| Path | What it is |
|------|------------|
| `meta/` | Ported from the Mac app: tag fingerprint, multi-value parsing, paths, manifests, Organizer |
| `taglib/` | Tag reading and writing through TagLib, following the Mac's scanner and writer |
| `library/` | One user's folder and index: scanning, hashing, uploads, edits, history |
| `store/` | Users and device tokens |
| `api/` | The HTTP API |
| `testdata/vectors.json` | Results of the Mac's own code on hundreds of inputs; `go test` fails on any disagreement |
| `tools/vectors/` | Regenerates the vectors by compiling the Mac's sources unmodified (`fetch.sh`, then `swift run vectors fixtures ../../testdata/audio ../../testdata/vectors.json`) |
| `dist/` | The systemd unit and avahi service |

See [DESIGN.md](DESIGN.md) for the architecture and [REVIEW.md](REVIEW.md) for the security, performance and size review.
