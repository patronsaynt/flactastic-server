# FLACtastic Server Protocol v2.0

How a FLACtastic client talks to a FLACtastic Server: signing in, browsing a
library, streaming, uploading, editing tags, keeping offline copies, and
keeping playlists, preferences and listening history in step across a user's
devices.

This document is the contract. The Go server in this repository is the
reference implementation, and clients (macOS, iOS, Windows, Linux) are built
from this document alone. `openapi.yaml` next to it describes the same
endpoints in machine-readable form; where they disagree, this document wins.

**Any change to a value in this document is a protocol change.** Additions
that old clients can ignore bump the minor version; anything else bumps the
major version and the path prefix (`/v3`).

v2 reuses the data model of [Sync Protocol v1](https://github.com/patronsaynt/flactastic-macos/blob/revisions/docs/sync-protocol-v1.md)
(track identity, `TrackManifestEntry`, `tagFingerprint`, `SyncDiff`,
`SyncFilter`, playlist JSON). Those definitions are repeated here so this
document stands alone.

---

## 1. Design constraints

- **A server holds the library; devices hold copies.** Each user has one
  library on the server. A device streams from it, or keeps offline copies of
  any part of it. The server never pushes: every transfer is a client request.
- **The receiver decides.** A device computes for itself what to download, and
  verifies every byte against the server's `contentHash` before it trusts it.
  The server recomputes any plan a client asks it to apply (§12) rather than
  acting on a plan the client computed.
- **Users are isolated.** Nothing a request can name (an ID, a path, a token)
  reaches another user's files or data.
- **Lossless by default.** Streams are the original bytes unless the client
  asks for a transcode.
- **Last write wins.** Tag edits, playlist and preference writes apply in the
  order the server receives them. Clients show the user what an edit replaces
  before sending it, as v1 did.
- **Personal data stays between a user and their own server.** Listening
  history, preferences and playlists are stored per user on the server so all
  of a user's devices agree. They are never shared with other users, and no
  credential other than the device token ever crosses the wire after sign-in.

### What carries over from v1, and what is replaced

| v1 | v2 |
|----|----|
| Track identity: `com.flactastic.trackID` UUID, `contentHash` fallback | **Same** |
| `TrackManifestEntry`, `tagFingerprint`, `SyncDiff`, `planHash`, `SyncFilter`, playlist JSON | **Same**, byte for byte where hashed |
| Path safety rules | **Same**, applied by the server to everything it writes |
| Resumable transfer, staging, digest check, atomic install | **Same ideas**, over HTTP ranges (download) and offset uploads |
| Bonjour `_flactastic._tcp`, TLS-PSK, pairing codes | **Replaced** by `_flactastic-srv._tcp`, a pinned self-signed certificate, and username/password sign-in |
| Framed control messages over one TCP connection | **Replaced** by HTTPS + JSON |
| Listening history never crosses the wire | **Changed**: it travels between a user and their own server only |

---

## 2. Discovery

The server advertises DNS-SD service type **`_flactastic-srv._tcp`** in the
`local.` domain via avahi. It is a different type from v1's
`_flactastic._tcp` so v1 peers never mistake a server for a pairing peer.

The SRV record gives host and port. TXT keys, the complete set:

| Key | Meaning | Example |
|-----|---------|---------|
| `v`  | Protocol version, `major.minor` | `2.0` |
| `id` | Server ID, a random UUID | `5C0F…` |
| `n`  | Display name, ≤ 63 UTF-8 bytes | `FLACtastic on nas` |

Discovery is a convenience, never trust. A client must also accept a typed
`host:port` (default port **4410**). Strip control and bidirectional-override
characters (`U+202A…U+202E`, `U+2066…U+2069`) from `n` before display.

On iOS and macOS the app needs `NSLocalNetworkUsageDescription` and
`_flactastic-srv._tcp` in `NSBonjourServices`; without them discovery
silently returns nothing.

---

## 3. Transport and trust

HTTPS only, on TCP port 4410. **TLS 1.3 only.** HTTP/1.1 and HTTP/2 (ALPN).

The server generates one ECDSA P-256 key and self-signed certificate on first
start (valid 100 years; it never rotates by itself). Trust is a **pin on the
public key**, not a CA chain and not a hostname, so the same pin works over a
LAN address, a VPN (WireGuard, Tailscale) or a forwarded port.

### Fingerprint

```
spki        = DER-encoded SubjectPublicKeyInfo of the server certificate
digest      = SHA-256(spki)                         // the pin: 32 bytes
fingerprint = base32(digest)[0..12]                 // RFC 4648 alphabet, uppercase, no padding
display     = fingerprint[0..4] "-" fingerprint[4..8] "-" fingerprint[8..12]   // e.g. K7F2-9QX4-M3PA
```

The display form carries 60 bits: an attacker would need about 2^60 key
generations to forge one, so a person comparing it by eye is enough.

### First connection

1. Connect without validating the chain, read the certificate, compute the
   fingerprint.
2. **Show the display fingerprint and ask the user to confirm it** matches what
   the server's installer printed (or `flactastic-server info` shows). Do this
   **before** sending a password.
3. On confirmation, store the full 32-byte `digest` with the server record.
4. Every later connection must present a certificate whose SPKI hashes to the
   stored digest. A mismatch is a hard failure with a clear message ("This
   server's identity changed"); never fall back to re-prompting silently.

### Timeouts

Clients should use: connect 20 s, idle read 60 s (streams and long-polls
excepted, §11). The server closes connections idle for 120 s and requires
request headers within 10 s.

---

## 4. Encoding rules

- Request and response bodies are JSON, UTF-8, `Content-Type:
  application/json`, unless an endpoint says otherwise.
- Dates: **ISO-8601 with fractional seconds**, UTC, millisecond precision
  (`2026-09-08T17:04:01.687Z`) — v1's rule. Parsers accept any number of
  fractional digits.
- Binary data inside JSON (`customArtwork`, `artwork`): base64 (RFC 4648
  standard alphabet, padded).
- UUIDs: the server writes **uppercase** with hyphens (`9F1C2B3A-…`), as
  Swift's `UUID` encodes. Readers accept any case. Anything hashed uses the
  uppercase form.
- Readers must not depend on key order or on byte equality of JSON; nothing
  in this protocol hashes JSON.
- Unknown object keys are ignored, in both directions. Missing optional keys
  mean "absent" (`null` and absent are equivalent unless an endpoint says a
  `null` clears something, §10).
- Integers fit in 53 bits. Sizes are bytes.
- Paths in URLs are percent-encoded; IDs in paths are UUIDs or lowercase hex.

### Versioning

`GET /v2/server` returns `protocol: "2.0"`. A client compares the major
version (must be 2) and may use features introduced in later minors only if
the server reports them. Servers keep serving `/v2` until a major bump.

---

## 5. Errors

Every non-2xx response with a body is:

```json
{ "error": { "code": "invalidPath", "message": "Rejected path." } }
```

`message` is for logs; clients show their own text keyed on `code`. Messages
never reveal another user's existence or data, and sign-in failures never say
which part was wrong.

| HTTP | `code` | Meaning |
|------|--------|---------|
| 400 | `badRequest` | Malformed JSON, missing field, value out of range |
| 400 | `unsupportedFormat` | File type not in the audio set |
| 401 | `unauthorized` | Missing, unknown or revoked token |
| 401 | `invalidCredentials` | Sign-in failed |
| 403 | `forbidden` | Admin-only endpoint |
| 404 | `notFound` | No such object **for this user** |
| 409 | `duplicate` | The library already has these bytes; `error.track` is the existing Track (§9.4) |
| 409 | `offsetMismatch` | Upload offset is not where the server is |
| 409 | `planStale` | The library changed since the plan was shown |
| 413 | `sizeExceeded` | Body or declared file over its limit |
| 422 | `hashMismatch` | Uploaded bytes do not match `contentHash` |
| 429 | `rateLimited` | Too many attempts; see `Retry-After` |
| 503 | `libraryOffline` | The user's music folder is unavailable (§9.5) |
| 503 | `transcodeUnavailable` | No transcoder, or all transcode slots busy; retry or stream the original |
| 507 | `insufficientStorage` | Quota or free-space headroom would be exceeded |
| 500 | `internalFailure` | Anything else |

---

## 6. Accounts and devices

Users are created by the server administrator with the `flactastic-server`
CLI; there is no self sign-up. Usernames match `^[a-z][a-z0-9_-]{0,31}$`;
clients lowercase what the user types. Passwords are 8–1024 bytes of UTF-8.

### Sign in

```
POST /v2/auth/login
{ "username": "alice", "password": "…", "deviceName": "Studio Mac", "deviceKind": "mac" }

200 { "token": "<43 chars>", "device": Device, "user": User }
```

`deviceKind` ∈ `mac`, `iPhone`, `iPad`, `windows`, `linux`, `other`.
`deviceName` ≤ 63 UTF-8 bytes. A token is 256 random bits, base64url without
padding. It does not expire; it ends when revoked. Store it in the platform
keychain (macOS/iOS: Keychain, `WhenUnlockedThisDeviceOnly`), never in
preferences or plain files.

Every other endpoint except `GET /v2/server` needs:

```
Authorization: Bearer <token>
```

```
Device = { "id": UUID, "name": String, "kind": String, "created": Date, "lastSeen": Date, "current": Bool }
User   = { "id": UUID, "name": String, "isAdmin": Bool }
```

### Rate limiting

Failed sign-ins are counted per client IP and per username. The first 5 are
free; after that each attempt must wait 1 s, doubling per failure to a cap of
15 minutes, answered with `429 rateLimited` and `Retry-After` (seconds). A
success clears the username's count. There is no permanent lockout, so an
attacker cannot lock a user out of their own server. Responses take the same
time whether or not the username exists. A request with an unknown token
counts as a failure for its IP, and a wrong `current` password on
`PUT /v2/me/password` counts against the username, so neither can be used to
guess faster than sign-in.

### Endpoints

| Method and path | Body → result |
|-----------------|---------------|
| `POST /v2/auth/logout` | → `204`; revokes the calling token |
| `GET /v2/me` | → `{ "user": User, "device": Device }` |
| `PUT /v2/me/password` | `{ "current", "new" }` → `204`; **revokes every other device of this user** |
| `GET /v2/devices` | → `{ "devices": [Device] }`, this user's only |
| `DELETE /v2/devices/{id}` | → `204`; revoke one of this user's devices |

---

## 7. The server and the library

### `GET /v2/server` (no auth)

```json
{ "id": "5C0F…", "name": "nas", "protocol": "2.0", "version": "beta-1", "transcoding": true }
```

`transcoding` is false when the server has no transcoder (§9.2).

### `GET /v2/library`

```json
{
  "libraryID": "…",          // the folder's .flactastic/library.json ID
  "state": "ready",          // "scanning" | "ready" | "offline"
  "seq": 4182,               // current change sequence (§11)
  "trackCount": 9120,
  "pendingHashes": 0,        // tracks not yet hashed; absent from manifests until done
  "usedBytes": 412000000000,
  "quotaBytes": null,        // null = no quota
  "freeBytes": 900000000000
}
```

### The Track object

```json
{
  "id": "UUID",                     // trackID
  "relativePath": "Daft Punk/Discovery/01 - One More Time.flac",
  "format": "flac",                 // flac | mp3 | wav | aiff | alac | aac
  "fileSize": 31234567,
  "contentHash": "hex sha-256",     // null until hashed
  "tagFingerprint": "hex sha-256",  // §8.2
  "title": "One More Time",         // never empty: falls back to the file name
  "artist": "Daft Punk",            // stored credit string (§8.3)
  "artistIsList": false,
  "albumArtist": "Daft Punk",
  "albumArtistIsList": false,
  "album": "Discovery",
  "trackNumber": 1, "discNumber": 1, "discTotal": 1,
  "year": 2001,
  "genre": "Electronic", "secondaryGenres": ["House"],
  "isCompilation": false, "isMixCompilation": false,
  "label": "Virgin",
  "duration": 320.4,                // seconds
  "sampleRate": 44100, "bitDepth": 16, "channels": 2, "bitrate": 1003,  // kbit/s
  "frames": 14129640,               // sample frames; exact for flac, wav, aiff
  "artworkID": "32 hex chars",      // null when the file has no picture
  "hasCueSheet": false,
  "dateAdded": "2026-01-02T03:04:05.000Z",
  "modified": "2026-01-02T03:04:05.000Z"
}
```

Optional values are omitted when absent. `bitDepth` is absent for mp3 and
aac. `GET /v2/tracks/{id}` returns the same object plus `"cueSheet"` and
`"lyrics"` (raw tag text, when present).

### Mirroring the track list

Clients keep a local mirror and fetch only what changed:

```
GET /v2/tracks?since=<seq>&limit=<1…5000, default 1000>

200 { "tracks": [Track], "deleted": [UUID], "seq": 4182, "more": false }
```

Returns every track created or changed, and every track deleted, after
`since`, in sequence order. Repeat with the returned `seq` while `more` is
true. `since=0` returns the whole library. Albums, artists and every other
view are built on the client from the mirror, exactly as the Mac's
`LibraryStore` builds them from its scan.

For clients that don't mirror:

| Method and path | Result |
|-----------------|--------|
| `GET /v2/albums` | `{ "albums": [Album] }`, grouped by the Mac's `LibraryStore` rule (§8.4) |
| `GET /v2/search?q=&limit=` | `{ "tracks": [Track] }`: case- and diacritic-insensitive substring match on title, artist, album artist and album; `limit` ≤ 200, default 50 |

```
Album = { "id": String, "name": String, "artist": String?, "albumArtist": String?, "year": Int?,
          "genre": String?, "secondaryGenres": [String], "label": String?, "artworkID": String?,
          "isCompilation": Bool, "isMixCompilation": Bool, "trackIDs": [UUID] }
```

---

## 8. Identity and metadata

### 8.1 Track identity

Every audio file carries a UUID: its **`trackID`**. On disk it lives in the
extended attribute `com.flactastic.trackID` (Linux: `user.com.flactastic.trackID`),
or, where the filesystem has no user xattrs (many NFS and SMB mounts), in
`<library>/.flactastic/track-ids.json` keyed by relative path, the Mac's own
sidecar format. A file that has neither gets a fresh UUID. Because the ID
lives with the file, rebuilding the server's index never changes it.

The server ignores symlinks and hidden names (a leading `.`) in a library,
as the Mac's scanner does, and never follows a symlink when it opens a file.

Two-tier matching, as v1: **`trackID`** first, then **`contentHash`**
(lowercase hex SHA-256 of the whole file). After a client downloads or
uploads a file, it adopts the server's `trackID` for it.

### 8.2 `tagFingerprint`

SHA-256 (lowercase hex) over these fields, in this order, joined by `U+001F`:

```
title, artist, albumArtist, album, trackNumber,
genre, secondaryGenres (sorted, comma-joined), year,
isCompilation ("1"/"0"),
"mix"  ← only when isMixCompilation is true
```

Each string is NFC-normalised and trimmed of surrounding Unicode whitespace
and newlines; absent is the empty string. Integers are base-10 with no
padding; absent is the empty string. The values are the **stored** forms of
§8.3 as the Mac's `LibraryScanner` reads them.

A new field must be appended at the end and contribute nothing in its
default state. `testdata/vectors.json` holds hundreds of cases generated by
running the Mac's own code; an implementation must reproduce all of them.

### 8.3 Multi-value tags

Artist, album artist and genre are read through one parser
(`MultiValueParser` in the Mac app), with this source priority:

1. The plural tag (`ARTISTS`, `ALBUMARTISTS`) as is.
2. Native multi-values (repeated Vorbis fields, NUL-separated ID3v2.4,
   repeated MP4 atoms) as is.
3. One string split on a safe separator: NUL, `;`, spaced ` / `, backslash.
4. One string with an ambiguous separator (`&`, `,`, `/`, ` x `, `feat.` …):
   split only when every part is already a standalone name in the library or
   the MusicBrainz artist ID count matches. Genres never use this rule.

Values are capped at 1024 grapheme clusters and 50 entries. The **stored**
credit (the `artist` string in a Track) is the singular tag exactly as tagged
when it is a single string, else the list joined with `" ; "`. The genre is
split into a primary and up to 3 secondaries.

### 8.4 Albums

Tracks are grouped by album name (lowercased; absent = "Unknown Album"), then
split by album artist when any track in the group has one. The album `id` is
`"<albumArtist or display artist>|<album name>"`, where the display artist is
the album artist, else the single lead credit shared by every track, else
`Various Artists`. This is the same `id` the Mac uses in `PlayEvent.albumID`.

---

## 9. Files

### 9.1 Original bytes

```
GET /v2/tracks/{id}/file
```

The file as stored, with full HTTP semantics: `Range` (single range),
`If-Range`, `If-None-Match`, `If-Modified-Since`, `206 Partial Content`,
`416`. `Cache-Control: private, no-cache`. `ETag` is `"sha256:<contentHash>"` once the file is hashed (a strong
validator), else `"m:<size>-<mtime ns>"`. `Accept-Ranges: bytes`.

| format | `Content-Type` |
|--------|----------------|
| flac | `audio/flac` |
| mp3 | `audio/mpeg` |
| wav | `audio/wav` |
| aiff | `audio/aiff` |
| alac, aac in `.m4a` | `audio/mp4` |
| aac in `.aac` | `audio/aac` |

All of these play through `AVPlayer` on iOS 11+ and macOS 10.13+ directly
from the URL (with the pin enforced in the URL session's delegate).

### 9.2 Streaming

```
GET /v2/tracks/{id}/stream?quality=<q>[&s=<sample offset>]
```

| `quality` | Result |
|-----------|--------|
| `original` (default) | Identical to `/file` |
| `high` | AAC-LC 256 kbit/s |
| `normal` | AAC-LC 160 kbit/s |
| `low` | AAC-LC 96 kbit/s |

`auto` is not a server value: a client set to auto measures its own
throughput and picks one of the above per track. The user's preferred mode is
stored on the server (§14) so it follows them between devices.

A transcoded response is `200`, `Content-Type: audio/aac` (ADTS), chunked,
`Accept-Ranges: none`, `Cache-Control: no-store`. Output rate: the source
rate when ≤ 48 kHz, else 44.1 kHz for the 44.1 family (88.2, 176.4, 352.8)
and 48 kHz otherwise; stereo or mono as the source (multichannel is
downmixed to stereo).

**Gapless.** An AAC encoder adds silence before the audio (priming) and pads
the last frame. A transcoded response carries:

| Header | Meaning |
|--------|---------|
| `FLACtastic-Sample-Rate` | Output sample rate |
| `FLACtastic-Priming` | Samples of encoder delay to discard from the start of the decoded output |
| `FLACtastic-Frames` | Samples of real audio to keep after the priming; discard everything after. Absent when the source file doesn't record its length (some encoders write 0); then keep everything after the priming |

A client that decodes the stream itself and honours these two numbers plays
transcoded tracks gaplessly. `FLACtastic-Frames` is exact for flac, wav and
aiff sources and within one millisecond for others. A client that hands the
URL to `AVPlayer` cannot trim and gets gapless playback on originals only.

**Seeking.** `s` is a position in **source** samples (`seconds ×
track.sampleRate`). The server starts the encode at exactly that sample, and
the headers describe the stream from there. Without `s` the stream starts at 0.

At most four transcodes per CPU core run at once per server (one the client
is not reading uses no CPU); beyond that the
server answers `503 transcodeUnavailable`. Stopping reading ends the encode.

### 9.3 Artwork

```
GET /v2/art/{artworkID}?size=300|1000|original
```

`artworkID` comes from a Track; `size` defaults to `original`. `300` and `1000` are JPEG thumbnails no
larger than that many pixels on the long edge (never upscaled), generated
once and cached; `original` is the embedded picture as stored (`image/jpeg`
or `image/png`). Responses are immutable: `Cache-Control: private,
max-age=31536000, immutable`, `ETag: "<artworkID>-<size>"`.

### 9.4 Uploads

Adds a file to the library, or replaces one. Resumable, with v1's staging
rules.

```
POST /v2/uploads
{ "filename": "One More Time.flac", "size": 31234567, "contentHash": "hex", "trackID": "UUID?" }

201 { "id": "UUID", "offset": 0 }
```

- `filename` supplies only the extension, which must be in the audio set
  (`flac mp3 wav wave aif aiff m4a aac`, any case). The server decides where
  the file goes (below).
- `size` ≤ 2 GiB. The server refuses (`507 insufficientStorage`) when the
  upload would exceed the user's quota or leave the volume with less than
  500 MB free.
- `trackID`, when it names a track already in this user's library, makes the
  upload **replace** that track: same ID, new bytes, refiled by the profile.
  An unknown `trackID` is adopted as the new track's ID. Absent: the new
  track gets a fresh ID.
- Re-posting the same `contentHash`, `size` and `trackID` (or both
  absent) while an upload is open returns that upload and its current
  offset, so a client that lost the `id` can resume.
- A `contentHash` that is not 64 hex digits is `400 badRequest`.
- Without a `trackID`, when the library already holds a track with the same
  `contentHash` and `size`, nothing is staged and the answer is `409
  duplicate`, with the existing track in the error:
  `{ "error": { "code": "duplicate", "message": "…", "track": Track } }`.
  Tracks the server has not hashed yet (`contentHash: null`) are not matched.

Send bytes:

```
PATCH /v2/uploads/{id}
Upload-Offset: <n>
Content-Type: application/offset+octet-stream
<bytes>

204 Upload-Offset: <n + bytes received>        while incomplete
200 { "track": Track }                          when the last byte arrives
```

`Upload-Offset` must equal the server's offset, else `409 offsetMismatch`
with the server's offset in the `Upload-Offset` response header. Bytes beyond
the declared size are refused (`413`) before they are written. A body may be
any length; clients typically send 8–64 MiB per request. `HEAD
/v2/uploads/{id}` returns the current `Upload-Offset`. `DELETE
/v2/uploads/{id}` abandons it.

On the last byte the server: verifies SHA-256 over the whole staged file
(`422 hashMismatch` deletes the staged file — the bytes are known-bad, so
resuming over them would only reproduce the failure); reads its tags; places
it with the user's Organizer profile (§12); writes the `trackID`; and
installs it with an atomic rename, so readers see the old file or the new
one, never a gap. The returned Track already carries the verified
`contentHash`. Staged uploads untouched for 7 days are deleted.

### 9.5 When the music folder is unavailable

While the server starts, before a user's library is open, every
authenticated endpoint answers `503 libraryOffline` for a few seconds.

If the library folder disappears (a network share drops) or its
`.flactastic/library.json` no longer carries the expected ID, the library is
**offline**: `GET /v2/library` reports `state: "offline"`, browsing keeps
working from the index, every endpoint that reads or writes files answers
`503 libraryOffline`, and nothing is removed from the index. Scanning resumes
when the folder returns.

---

## 10. Editing tags

```
PATCH /v2/tracks/{id}
PATCH /v2/tracks          { "ids": [UUID], "edit": Edit }      (an album edit; ≤ 1000 ids)
```

`PATCH /v2/tracks/{id}` takes an `Edit` as its body. Every field is
optional. **Absent leaves the tag alone, `null` clears it, a value sets it.**

```
Edit = {
  "title": String, "artist": String, "albumArtist": String, "album": String,
  "trackNumber": Int, "discNumber": Int, "discTotal": Int, "year": Int,
  "genre": String, "secondaryGenres": [String],
  "isCompilation": Bool, "isMixCompilation": Bool,
  "label": String, "lyrics": String, "cueSheet": String,
  "artwork": base64 JPEG or PNG (≤ 10 MiB) | null to remove
}
```

The server writes tags exactly as the Mac's `MetadataWriter` does:
`artist` and `albumArtist` are credit strings (`"A ; B"` for a list); a list
is written as the plural tag plus a display string joined with the user's
`artistSeparator` preference (§14); genres are written as native values;
`FLACTASTIC_TAGVERSION` becomes 2; turning on `isMixCompilation` clears
`lyrics` and `isCompilation` (and vice versa for `isCompilation`); FLAC keeps
`DISCTOTAL` separately while ID3 and MP4 store `n/total`.

The edit is made on a copy of the file which then replaces the original
atomically, so a stream in progress keeps reading the old bytes. The track is
then refiled through the user's default Organizer profile, which may change
its `relativePath`. The response is `200 { "track": Track }` (or
`{ "tracks": [Track] }`).

```
DELETE /v2/tracks/{id}         → 204
```

Moves the file into `<library>/.flactastic/trash/`, from which it is deleted
after 30 days. The track appears in `deleted` of the next `/v2/tracks` page.

---

## 11. Change signal

```
GET /v2/changes?since=<seq>&wait=<0…60>

200 { "seq": 4190, "changed": ["tracks", "playlists"] }
```

Every change to a user's data advances that user's `seq`. `changed` lists
which kinds changed after `since`: `tracks`, `playlists`, `prefs`,
`organizer`, `history`, `userdata`, `library` (state, quota or scan
progress). When
nothing has changed the server holds the request up to `wait` seconds
(default 0) and answers as soon as something does, or with an empty
`changed` at the deadline. Clients long-poll this while in the foreground and
re-fetch what changed.

---

## 12. Organizer

The Mac's Organizer profiles live on the server, per user, so every client
shows and edits the same set.

```
GET /v2/organizer           → { "profiles": [OrganizerProfile], "selectedID": UUID? }
PUT /v2/organizer           ← same shape → 204
```

`OrganizerProfile` is the Mac's JSON: `{ id, name, levels: [{ id, groupBy,
name, nameTemplate }], fileTemplate, usePrimaryArtistOnly,
deleteEmptyOriginals }`, with tokens `{artist} {albumArtist} {album} {title}
{track} {year} {decade} {genre} {format} {ext} {artistInitial}`. With no
profiles stored, the server uses the Mac's default, "Artist / Album"
(`{albumArtist}` / `{album} ({year})` / `{track} - {title}`).

The **selected** profile places every upload and every edited track
automatically. Reorganising files already in the library is explicit:

```
POST /v2/organizer/plan   { "profileID": UUID? }
200 { "planHash": "hex", "operations": [{ "trackID", "from", "to", "status" }] }

POST /v2/organizer/apply  { "profileID": UUID?, "planHash": "hex" }
200 { "moved": 812, "unchanged": 8300 }
```

`status` is `move`, `unchanged` or `conflict` (two tracks render to one path;
the second gets ` (1)`, ` (2)` … before the extension, as the Mac does).
`planHash` is SHA-256 over the sorted lines `<trackID>:<to>`. On `apply` the
server recomputes the plan and answers `409 planStale` if the hash differs,
so a library that changed while the user looked at the preview is never
reorganised against an approval they didn't give. With
`deleteEmptyOriginals`, folders left with no audio (cover art and other
leftovers included) are removed after organizing, refiling and deleting, never
above the library root. A scan removes folders that are completely empty,
whatever the setting. Rendered names follow the Mac's rules, with one
addition: a component longer than 255 bytes is shortened at a grapheme
boundary (keeping the extension), where the Mac would fail.

---

## 13. Offline copies

A device keeps copies of any part of its user's library and refreshes them
itself; the server keeps no per-device state.

```
POST /v2/manifest
{ "selection": { "albumIDs": [String]?, "playlistIDs": [UUID]?, "trackIDs": [UUID]? }?,
  "filter": SyncFilter? }

200 LibraryManifest
FLACtastic-Pending: <tracks in the selection not yet hashed>
```

- `selection` absent = the whole library; otherwise the **union** of the
  albums, the tracks of the playlists, and the tracks named. Playlists named
  in `playlistIDs` are included in the manifest's `playlists`; with no
  `playlistIDs`, every playlist is (subject to the filter).
- `filter` is v1's `SyncFilter` (§13.2), applied by the server when it
  builds the manifest. The client applies it again on receipt.
- Tracks not yet hashed are left out and counted in `FLACtastic-Pending`;
  ask again later (watch `library` on `/v2/changes`).

The client then:

1. Builds its own manifest of the copies it holds and runs v1 `SyncDiff`
   with the server's manifest as `incoming`, its own as `local`, direction
   `pull`, and its filter. New tracks and conflicts are what to download.
2. Downloads each with `GET /v2/tracks/{id}/file`, resuming with `Range` from
   the length of its partial file. A `.part` longer than `fileSize` is
   discarded, not spliced.
3. Verifies SHA-256 of the whole file against `contentHash`; a mismatch
   deletes the partial (never resume over known-bad bytes).
4. Installs atomically and writes the server's `trackID` to the file.
5. Fetches playlists with `GET /v2/playlists/{id}`.

Copies the manifest no longer lists are the client's to remove or keep.

### 13.1 `LibraryManifest`

```
LibraryManifest   = { "deviceID": UUID, "generatedAt": Date,
                      "tracks": [TrackManifestEntry], "playlists": [PlaylistManifestEntry] }
TrackManifestEntry = { "trackID": UUID, "relativePath": String, "fileSize": Int, "contentHash": String,
                       "format": String, "tagFingerprint": String,
                       "title": String, "artist": String?, "album": String?, "albumArtist": String? }
PlaylistManifestEntry = { "id": UUID, "name": String, "dateCreated": Date, "entryCount": Int, "contentHash": String }
```

`deviceID` is the library's ID. Playlist `contentHash` is SHA-256 of
`name + "\n" + lines` where each line is `<trackID or "-">|<relativePath>`
(uppercase UUIDs) joined by `"\n"`, entries in order.

### 13.2 `SyncFilter`

| Field | Meaning | Default |
|-------|---------|---------|
| `excludedFormats` | Formats to leave out | `[]` |
| `excludedArtistKeys` | Artist keys (case- and diacritic-folded, whitespace collapsed) of the album artist, else the artist | `[]` |
| `maxFileSizeBytes` | Skip larger files | none |
| `includePlaylists` | Whether playlists are included | `true` |
| `playlistIDAllowlist` | Only these playlists | all |
| `maxTotalTransferBytes` | Client-side ceiling for one refresh | none |

Every field may be absent.

### 13.3 The diff (v1, unchanged)

For each incoming track: known `trackID` with a different `contentHash` or
`tagFingerprint` → conflict (download, and tell the user what is replaced);
known `trackID`, all equal → skip; unknown `trackID` but known
`contentHash` → skip and adopt the ID; else → new. For each incoming
playlist: unknown `id` → new; different `contentHash` or `name` → conflict.

`planHash` is SHA-256 over the sorted, newline-joined lines `<direction>`,
`n:<trackID>:<contentHash>`, `c:<trackID>:<contentHash>`,
`p:<playlistID>:<contentHash>`, `q:<playlistID>:<contentHash>`. A client that
only pulls from a server does not need it, but it is part of the reused code.

---

## 14. Playlists and preferences

### Playlists

The Mac's `Playlist` JSON:

```
Playlist = { "id": UUID, "name": String, "entries": [{ "id": UUID, "trackID": UUID?, "relativePath": String }],
             "dateCreated": Date, "customArtwork": base64?, "description": String? }
```

| Method and path | Body → result |
|-----------------|---------------|
| `GET /v2/playlists` | → `{ "playlists": [Playlist] }` |
| `GET /v2/playlists/{id}` | → `Playlist` |
| `PUT /v2/playlists/{id}` | `Playlist` (its `id` must match) → `204`; creates or replaces |
| `DELETE /v2/playlists/{id}` | → `204` |

`name` ≤ 255 characters, `description` ≤ 200, ≤ 10 000 entries,
`customArtwork` ≤ 5 MiB; the whole body ≤ 8 MiB. The server stores entries
as given and never treats `relativePath` as a filesystem path.

### Preferences

```
GET   /v2/prefs   → Prefs
PATCH /v2/prefs   ← any subset of Prefs → Prefs
```

```
Prefs = {
  "streamQuality": "original",     // original | auto | high | normal | low
  "artistSeparator": ", ",         // one of ", " "; " " ; " " / " " & "
  "clients": { "<key>": <any JSON> }
}
```

`clients` holds per-client settings that should follow the user (e.g. key
`"mac"`), each ≤ 64 KiB. A `PATCH` replaces the keys it names; a `null`
under `clients` removes that key. Never store credentials here.

### User data

Customisation that is too big for preferences, such as artist profiles and
their pictures, follows the user as named files. The server stores the bytes
and never reads them.

```
GET    /v2/userdata          → { "items": [{ "name": String, "size": Int, "etag": String }] }
GET    /v2/userdata/{name}   → the bytes, with ETag; honours If-None-Match and Range
PUT    /v2/userdata/{name}   ← the bytes (≤ 8 MiB) → 204, with ETag
DELETE /v2/userdata/{name}   → 204
```

`name` is ≤ 200 bytes of `/`-separated segments. A segment uses
`A–Z a–z 0–9 . _ -` and does not start with a dot. `etag` is derived from
the bytes, so equal bytes have equal ETags on every server, and a `PUT` of
the bytes already stored changes nothing. Last write wins. Any other change
reports `userdata` on `/v2/changes`.

A client syncs by listing and comparing ETags. It remembers the ETag each
item had when it last matched the server, which tells the cases apart:

| Local | Server | Meaning |
|-------|--------|---------|
| changed since its remembered ETag | same as remembered | upload |
| unchanged | different ETag | download |
| never synced | absent | upload |
| unchanged | absent | deleted elsewhere: remove locally |
| deleted locally | same as remembered | `DELETE` |
| changed | different ETag | both changed: the server copy wins |

Names every client agrees on:

- `artists/<id>.json`: one artist's profile. `<id>` is the first 32 hex
  characters of SHA-256 over the UTF-8 artist key (the Mac's
  `ArtistResolver.key(for:)`).

  ```
  ArtistProfile = { "canonicalKey": String, "displayName": String?,
                    "bannerImage": base64 JPEG or PNG?, "profileImage": base64 JPEG or PNG? }
  ```

  This is the Mac's `ArtistOverride` JSON. Clients scale pictures so the
  file stays under the limit. A profile with nothing set is deleted, not
  stored empty.

Everything else is private to one kind of client and lives under that
client's prefix (`mac/…`, `linux/…`). Never store credentials here.

---

## 15. Listening history and stats

### History

```
POST /v2/history  { "events": [PlayEvent], "contexts": [RecentContext] }  → 200 { "accepted": n }
GET  /v2/history?since=<seq>&limit=<≤ 5000>
     → { "events": [PlayEvent], "contexts": [RecentContext], "seq": n, "more": Bool }
```

The Mac's JSON:

```
PlayEvent     = { "id": UUID, "date": Date, "trackID": UUID, "title": String, "artist": String?,
                  "albumID": String?, "album": String?, "genre": String?, "secondaryGenres": [String]?,
                  "secondsListened": Double, "counted": Bool }
RecentContext = { "kind": "album" | "playlist", "targetID": String, "title": String,
                  "subtitle": String, "date": Date }
```

Events are idempotent by `id`: posting one twice stores it once. A device
posts its plays as they happen (batched, ≤ 1000 per request) and mirrors
everyone else's with `GET /v2/history`. Contexts are kept per `(kind,
targetID)`, newest `date` wins, and only the 50 newest are kept; `GET`
always returns all of them.

### Stats

```
GET /v2/stats?tz=<IANA zone, default UTC>&since=<Date?>&limit=<≤ 50, default 10>
```

```json
{
  "totalSeconds": 51234.5, "tracksPlayed": 812, "albumsPlayed": 97, "sessions": 64,
  "streakDays": 5,
  "topArtists": [{ "name": "Daft Punk", "plays": 40, "minutes": 210.5 }],
  "topAlbums":  [{ "albumID": "Daft Punk|Discovery", "album": "Discovery", "artist": "Daft Punk", "plays": 30, "minutes": 150.2 }],
  "topGenre":   { "name": "House", "share": 0.31 },
  "onThisDay":  { "albumID": "…", "album": "…", "artist": "…", "headline": "A year ago today you played",
                  "plays": 3, "minutes": 14.1, "date": "…" }
}
```

Definitions are the Mac's `ListeningStore`: `totalSeconds`, `sessions` (runs
of plays separated by more than 30 minutes) and the rankings count every
event since `since`; `tracksPlayed` and `albumsPlayed` count `counted` plays;
rankings order by minutes, then plays, then name; `streakDays` counts
consecutive calendar days in `tz` ending today or yesterday; `onThisDay`
looks for the most-played album on this date 1, 2, … years ago, then the
week around this date last year, among albums still in the library. Absent
results are omitted.

---

## 16. Administration

`GET /v2/admin/status` (admin users only) → `{ "version", "uptime",
"users": [{ "name", "state", "trackCount", "pendingHashes", "usedBytes",
"quotaBytes" }] }`. Everything else (users, devices, quotas, scans,
backups) is done with the `flactastic-server` CLI on the server.

---

## 17. Limits

| Limit | Value |
|-------|-------|
| Request headers | 16 KiB, within 10 s |
| JSON body | 1 MiB; 8 MiB for playlists, manifest requests, preferences and history; 16 MiB for tag edits (base64 artwork) |
| Single file | 2 GiB |
| Free space kept on upload | 500 MB |
| Embedded artwork accepted in an edit | 10 MiB, ≤ 6000 px per side |
| Playlist | 10 000 entries, artwork 5 MiB |
| `clients` preference value | 64 KiB each |
| History batch | 1000 events and 1000 contexts |
| History page | 5000, default 1000 |
| Organizer profiles | 100 |
| `clients` preference keys | 64 keys of ≤ 64 bytes |
| User data | 8 MiB per item, 512 MiB and 20 000 items per user, name ≤ 200 bytes |
| Track page | 5000 |
| Batch edit | 1000 tracks |
| Path component / depth / whole path | 255 bytes / 32 / 3072 bytes |
| Tag value / values per field | 1024 graphemes / 50 |
| Sign-in back-off | 5 free, then 1 s doubling to 15 min |
| Long-poll wait | 60 s |
| Idle connection | 120 s |
| Concurrent transcodes | four per CPU core |
| Staged upload lifetime | 7 days |
| Trash | 30 days |

---

## 18. Conformance checklist (clients)

- [ ] The certificate's SPKI is pinned; the display fingerprint is confirmed by the user before the first password is sent; a changed key is a hard failure.
- [ ] The token is stored in the keychain, never in plain storage.
- [ ] Tracks are mirrored with `/v2/tracks?since=` and views are built locally.
- [ ] `tagFingerprint`, the multi-value parser and `SyncDiff` reproduce `testdata/vectors.json`.
- [ ] Downloads resume with `Range`, are verified against `contentHash`, discarded on mismatch, installed atomically, and adopt the server's `trackID`.
- [ ] Transcoded streams are trimmed by `FLACtastic-Priming` and `FLACtastic-Frames`; seeks use `s` in source samples.
- [ ] Uploads resume from the server's offset and never send past `size`.
- [ ] The user sees what a tag edit replaces before it is sent.
- [ ] History is posted with stable event IDs and merged from `GET /v2/history`.
- [ ] Error handling keys on `error.code`, never on `message`.
- [ ] Display names from TXT records are stripped of control and bidi-override characters.
