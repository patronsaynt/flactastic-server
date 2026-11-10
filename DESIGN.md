# FLACtastic Server — design (draft for approval)

A multi-user lossless music server for Debian 13 (amd64, arm64). Clients stream from it, sync offline copies from it, upload to it and edit tags on it. One Go binary, `flactastic-server`, plus the hardened systemd unit and `install.sh`. Version `beta-1`, protocol `2.0`.

## Components

| Piece | What it does |
|---|---|
| `serve` | HTTPS API on `:4410` (TLS 1.3 only; HTTP/1.1 and HTTP/2) |
| Scanner | One goroutine per user. Uses inotify on local roots and a 10-minute poll on NFS/CIFS/FUSE (detected with `statfs`). A file is indexed only after its size and mtime hold for 2 s and TagLib opens it. Unreadable files are logged and retried when their mtime changes. A background hasher fills in `contentHash` after metadata. |
| Tag engine | TagLib **2.3.2** (the Mac's pin), vendored source verified by SHA-256 and linked statically through cgo. A C shim mirrors `tag_c` plus the Mac's `taglib_helpers.cpp`. Reads follow `LibraryScanner.parse` and writes follow `MetadataWriter`. The m4a codec (ALAC vs AAC) comes from TagLib's MP4 properties. |
| Ported logic | `MultiValueParser`, `GenreResolver`, `TagFingerprint`, `PathSanitizer`, `SyncFilter`, `ArtistResolver.key`, manifest and playlist hashing. Swift `String` semantics are matched explicitly: NFC, Unicode whitespace trim, grapheme-cluster counts for the 1024/50 caps. |
| Organizer | Ported from the Mac: `OrganizerProfile` (same JSON), `OrganizerTemplate.render`, `OrganizerPlanner`, `ImportCopy.sanitize`/`uniqueDestination`, and `ArtistResolver.split` for "primary artist only". Each user's profiles and selected default live on the server. Uploads and tag edits are filed through that default, and if none is set the Mac's own default ("Artist / Album") applies. A name component over 255 bytes is shortened at a grapheme boundary, keeping its extension. |
| Transcoder | `ffmpeg` (optional apt dependency) produces AAC-LC/ADTS at 256k, 160k or 96k. Hi-res sources drop to 44.1 or 48 kHz, staying in the same rate family. Responses carry `FLACtastic-Priming` (encoder delay, in samples) and `FLACtastic-Frames` (exact audio length at the output rate), and the client trims both ends, so playback stays gapless. A seek restarts the encode at the exact sample given in `?s=<sample offset>`. At most one encode per CPU, and nothing is cached. Without ffmpeg the server offers only `original`. |
| CLI | `user add/del/list/passwd/quota/root/admin`, `device list/revoke`, `scan`, `info` (address and TLS fingerprint), `backup`, `rebuild`, `version`. It writes SQLite directly. The server re-reads users and tokens per request (indexed lookup) and watches `PRAGMA data_version`, so no admin socket is needed. |
| Discovery | A static avahi service file written by `install.sh`: `_flactastic-srv._tcp`, TXT `v=2.0`, `id=<random UUID>`, `n=<name>`. Nothing in the binary. |

## Data model

- `/var/lib/flactastic-server/server.db` (SQLite WAL): `users` (id, name, argon2id hash, admin, root, libraryID, quota), `devices` (id, user, name, kind, sha256(token), created, lastSeen), `tls/` key and cert.
- `/var/lib/flactastic-server/users/<userID>/index.db`, one per user: `tracks` (trackID, relPath, size, mtimeNs, inode, contentHash, format, all tags, fingerprint, audio props, art key), `playlists` (the Mac's `Playlist` JSON), `history` (the Mac's `PlayEvent`), `prefs`, `changes` (seq, kind).
- `…/users/<userID>/art/` holds the 300 px and 1000 px JPEG thumbnails, keyed by picture hash.
- **In the music folder** (the server can now write there): `.flactastic/library.json` (same format as the Mac), the `user.com.flactastic.trackID` xattr, falling back to the Mac's `.flactastic/track-ids.json` sidecar where xattrs aren't supported, and `.flactastic/incoming/` for staging uploads. The IDs live with the files, so `rebuild` keeps every track ID, and therefore playlists and history.
- **Mount loss**: before each scan the server checks that the root exists and its `library.json` ID matches. If not, the library is *offline*: browsing works from the index, file endpoints return `503 libraryOffline`, and **no rows are deleted**.

## API outline (full contract goes in `docs/server-protocol-v2.md` + OpenAPI)

JSON keys are sorted, dates are ISO-8601 with fractional seconds, `Data` is base64: v1's rules, unchanged. Auth is `Authorization: Bearer <token>`.

- `GET /v2/server` (public: version, id, name) · `POST /v2/auth/login` → per-device token · `POST /v2/auth/logout` · `GET/DELETE /v2/devices[/{id}]` · `PUT /v2/me/password`
- `GET /v2/tracks?cursor=` · `/albums` · `/artists` · `/search?q=` · `GET/PATCH/DELETE /v2/tracks/{id}` (PATCH takes the Mac editor's field set; DELETE moves the file to `.flactastic/trash/`, which is swept after 30 days)
- `GET /v2/tracks/{id}/file`: the original bytes, with Range, If-Range, ETag and If-None-Match. `GET /v2/tracks/{id}/stream?quality=original|high|normal|low[&s=]` · `GET /v2/tracks/{id}/art?size=300|1000|original`
- Uploads (resumable, v1's staging rules): `POST /v2/uploads {filename,size,contentHash,trackID?}` → id and offset · `PATCH /v2/uploads/{id}` with `Upload-Offset` · `HEAD` returns the offset. On completion the server verifies the hash, reads the staged file's tags, places it with the user's default profile (`(1)` suffix on a clash, and an upload with a known `trackID` replaces that track), then installs atomically. The client never chooses the path, so the filename only supplies the extension.
- Organizer: `GET/PUT /v2/organizer` (profiles plus the selected ID) · `POST /v2/organizer/plan {profileID?}` → the moves, clashes and `planHash`, for the client's preview · `POST /v2/organizer/apply {planHash}`, which runs only if the server's recomputed plan hashes the same (v1's receiver-confirms rule). Honours `deleteEmptyOriginals`.
- `POST /v2/manifest {selection, filter}` → v1 `LibraryManifest`. The client runs v1 `SyncDiff` and pulls with `/file`. `GET /v2/changes?since=<seq>` long-polls (≤ 60 s).
- `GET/POST/PUT/DELETE /v2/playlists[/{id}]` · `GET/PUT /v2/prefs` · `POST /v2/history` (a batch of `PlayEvent`, idempotent by id) · `GET /v2/stats?tz=&period=` · `GET /v2/admin/status` (admin only)
- Every write is **last write wins**, as you chose.

## Security

- **Isolation**: a token resolves to exactly one user. That user's handlers see only that user's `index.db` handle and root. Every file open walks the path one component at a time with `openat(O_NOFOLLOW)` from the root's directory fd, so no symlink, `..` or race can leave the library. (The plan was `openat2(RESOLVE_BENEATH)`, but systemd's `RestrictSUIDSGID=` and many container runtimes refuse `openat2`.) Roots may not overlap, which is checked after resolving symlinks.
- **Passwords**: Argon2id (19 MiB, t=2, p=1), with a dummy hash for unknown names so timing is the same. Failed logins back off exponentially per IP and per username (5 free, cap 15 min, no hard lockout). A token is 256 random bits, stored as SHA-256 and compared in constant time. Tokens don't expire, are revocable, and changing a password revokes the user's other devices.
- **TLS**: an ECDSA P-256 self-signed cert made at install. The fingerprint is 12 base32 characters (60 bits) of SHA-256(SPKI), e.g. `K7F2-9QX4-M3PA`. The installer and `info` print it, and the client shows it once before sending a password, then pins the full hash.
- **Limits**: 16 KiB headers, 1 MiB JSON (8 MiB for playlists and manifest requests), 2 GiB per file, 10 MiB artwork, 64 KiB prefs, 1000 events per history batch. Timeouts: 10 s to read headers, 120 s idle. Uploads are refused if they would break the quota or leave under 500 MB free.
- **Threats considered**: a stolen token (revoke it), cross-user ID/path guessing, hostile upload paths and tag text, symlinks planted in a share, NFS dropping mid-scan, half-written files, a decompression bomb in artwork (image size is checked before decoding), and a MITM on first connect (the fingerprint check).

## Dependencies

| Dependency | Why |
|---|---|
| Go 1.24 standard library | HTTP with Range/ETag, TLS, crypto, image/jpeg+png |
| TagLib 2.3.2 (vendored) | Same tag engine and version as the Mac, so fingerprints match |
| `mattn/go-sqlite3` | SQLite. Cgo is already required for TagLib, and it is faster and lighter than the pure-Go driver |
| `golang.org/x/crypto` | Argon2id |
| `golang.org/x/text` | NFC normalisation to match `precomposedStringWithCanonicalMapping` |
| `golang.org/x/sys` | `openat2`, inotify, xattrs, statfs |
| `golang.org/x/image` | Quality thumbnail scaling |
| `rivo/uniseg` | Grapheme clusters, for Swift `String.count` parity in the tag caps |
| apt: `ffmpeg` (optional), `avahi-daemon` (optional) | Transcoding, discovery |

## Test vectors

`tools/vectors/` is a small Swift package that compiles the Mac files **unmodified** from a pinned `revisions` commit, with shims for CryptoKit (swift-crypto) and `os.Logger`. It emits `testdata/vectors.json`: fingerprints, parser cases, path accept/reject cases, manifest and plan hashes, and per-file results for the Mac's own `Tests/Fixtures/audio` files. `go test` fails on any disagreement, and CI runs it.

## Packaging and operations

`install.sh` plus a GitHub release workflow that builds natively on amd64 and arm64 runners, with actions pinned to SHAs and `SHA256SUMS` published. Logs go to journald only. `backup` uses SQLite online backup. `rebuild` re-indexes from the music folders, and it is always safe. Upgrading means re-running the installer, and schema migrations run on start.

## Client handoff

Written last, once the server and `docs/server-protocol-v2.md` are final: `MACOS_CLIENT_PROMPT.md`, a self-contained prompt for Claude Code on macOS. It works from the `revisions` branch and covers the build order, the protocol doc and test vectors as the source of truth, what to reuse from `Sync/` (Manifest, SyncDiff, SyncFilter, PathSanitizer, ContentHasher), the new pieces (server connection and fingerprint pinning, Keychain token, streaming decode with gapless trimming, uploads, server-side Organizer, playlists, history, prefs, offline copies) and the conformance checks to pass.

## Open choices I made for you (veto any)

- Port **4410**, and the code goes in the existing `flactastic_server/` folder.
- A tag edit refiles that track through the default profile right away, so the folders always follow the profile. The alternative is to move files only when the client runs the Organizer.
- Files already in the library are left where they are until the user applies a plan. Only new uploads and edited tracks move on their own.
- DELETE goes to a 30-day trash, never straight to unlink.
- Embedded CUE sheets are returned raw in the track detail, and clients parse them with their own `CueSheet`.
- Transcoded streams stay gapless only on clients that trim using the two headers. A plain `AVPlayer` client can't, so it gets gapless playback on original streams only. Joins between AAC tracks aren't bit-perfect, but this is the same method iTunes uses for gapless AAC.
- `sendfile` doesn't apply, because TLS always encrypts in user space. Memory per stream is bounded at 32 KiB.
