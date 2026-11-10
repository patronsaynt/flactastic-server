# Prompt: FLACtastic Server support in the macOS app

Give this to Claude Code, running in the macOS app's repository (`flactastic-macos`, branch `revisions`). Also give it a copy of the server repository (`flactastic_server/`), either at a path on the Mac or as a sibling checkout, and tell it where that is.

---

You are adding **FLACtastic Server** support to the FLACtastic macOS app. The server already exists: it is a self-hosted, multi-user Go server for Debian that holds each user's lossless library. The Mac app must be able to:
- sign in to it,
- browse and stream that library,
- upload to it and edit tags on it,
- keep playlists, preferences and listening history in step through it,
- keep offline copies of any part of it.

The server repository is at `<SERVER_REPO>`. Its contract is the source of truth. Read these first, in order, before designing anything:

1. `<SERVER_REPO>/docs/server-protocol-v2.md`: the protocol. It stands alone, and every rule in it is normative.
2. `<SERVER_REPO>/docs/openapi.yaml`: the same endpoints in machine-readable form. If the two disagree, the Markdown wins. Report any disagreement you find.
3. `<SERVER_REPO>/testdata/vectors.json` and `<SERVER_REPO>/tools/vectors/`: results produced by running this app's own Swift code, unmodified, on hundreds of inputs. They cover fingerprints, multi-value parsing, artist keys, paths, playlists, plans, the Organizer, and 33 real audio files. The server reproduces all of them.
4. `<SERVER_REPO>/DESIGN.md` and `<SERVER_REPO>/REVIEW.md`: why the server works as it does, and what was found when it was reviewed.

Then re-read the parts of this app the work touches:
- `docs/sync-protocol-v1.md`
- `Sources/flactastic/Sync/` (Manifest, SyncDiff, SyncFilter, PathSanitizer, ContentHasher, FileTransfer)
- `Library/` (Track, LibraryScanner, LibraryStore, MetadataWriter, MultiValueParser, Playlist)
- `Persistence/` (PlaylistStore, ListeningStore, TrackIDStore, Settings, OrganizerProfilesStore)
- `Audio/PlayerEngine.swift`
- `UI/Organizer/`
- `PRODUCT.md` and `DESIGN.md`

## Operating rules

- **Write the least code that works.** Reuse what exists: `TrackManifestEntry`, `SyncDiff`, `SyncFilter`, `ContentHasher`, `TagFingerprint`, `Playlist`, `PlayEvent`, `OrganizerProfile` are already the server's wire types. Add no abstraction until a second caller needs it.
- **Ask before you build.** Use AskUserQuestion, 1–4 questions per round, with the recommended option first. Don't ask what the code or the protocol already answers. After the interview, present a one-page design and wait for approval before writing code.
- **Never touch the git index.** Don't run `git add`, `git rm` or `--cached`, and make no commits unless the user asks. Leave changes uncommitted.
- **Keep copy brief and natural.** One-line subtitles, no explaining the app, no invented taglines. Follow `DESIGN.md` for every surface, in both light and dark themes.
- Keep the `beta-N` style in `version.txt`.
- Don't modify the server repository. If the protocol needs to change, stop and say so.

## First, confirm parity on macOS

The vectors were generated with Swift 6.4 on Linux, which uses swift-foundation. The Mac uses Apple's Foundation. Run the generator on this Mac:

```bash
cd <SERVER_REPO> && scripts/build-taglib.sh
```

```bash
cd <SERVER_REPO>/tools/vectors && ./fetch.sh && swift run -c release vectors fixtures ../../testdata/audio /tmp/vectors-macos.json
```

`build-taglib.sh` needs `cmake` (`brew install cmake`).

`fetch.sh` pins one `revisions` commit. On macOS the shim targets are not used: the real AVFoundation, os and CryptoKit are. If the build complains about the shims, build with `swift build --product vectors`. Diff `/tmp/vectors-macos.json` against `testdata/vectors.json`, ignoring `source.platform`. Any difference is a cross-platform fingerprint or parser divergence. Report it to the user before going on: it decides whether the Mac and the server agree on every track.

## Known issue to fix in this app

`PathSanitizer.sanitizeRelativePath` splits and compares whole `Character`s, so a `/` followed by a combining mark is not treated as a separator. Fuzzing the Go port showed that `"/\u{036F}0"` is accepted and comes out starting with `/`, and that `" ~"` becomes `"~"`. The server re-checks each `/`-separated piece of the result after the existing steps; make the same fix here and add both cases to `PathSanitizerTests`. It only ever rejects more.

## What to build, in this order

Each step should work and be testable on its own before the next starts.

1. **Server connection and trust (§2, §3).**
   - Discovery: browse `_flactastic-srv._tcp` (add it to `NSBonjourServices`) and read TXT `v`, `id` and `n`. Sanitize `n` as v1 does. Also allow a typed `host:port`, default port 4410.
   - TLS: one `URLSession` whose delegate pins the SHA-256 of the certificate's SubjectPublicKeyInfo, used for every server request, `AVPlayer` included.
   - First connection: compute the 12-character fingerprint exactly as §3 says. Show it and have the user confirm it before any password is sent. Store the pin with the server record.
   - A changed key is a hard failure with a clear message, never a silent re-prompt.
2. **Sign-in and devices (§6).**
   - Username and password produce a device token, stored in the Keychain with `WhenUnlockedThisDeviceOnly`. Never fall back to plain storage.
   - Sign out, list and revoke devices, change password. Note that changing the password signs out the other devices.
   - Handle every error by `error.code`, honouring `Retry-After` on `429`.
3. **The library mirror (§7, §11).**
   - Mirror tracks with `GET /v2/tracks?since=`, persisting `seq`. Map the server `Track` into the app's `Track`: `id` is the trackID, and the format comes from `format`.
   - Build albums and artists with the existing `LibraryStore` code, as if the tracks came from a scan.
   - Long-poll `/v2/changes` while in the foreground. Show `libraryOffline` and `scanning` states plainly.
   - Decide with the user how a server library sits alongside local folders in the sidebar and Collection.
4. **Playback (§9.1, §9.2).**
   - Originals: `/file` and `/stream?quality=original` are plain HTTP with Range. `PlayerEngine` decodes local files with `AVAudioFile`, which needs a file. Propose the smallest design that keeps gapless playback and the fixed-rate output path. Progressive caching to a local file with ranged requests is the likely answer; ask.
   - Transcodes: decode the ADTS stream yourself (`AudioFileStream` + `AudioConverter`). Discard `FLACtastic-Priming` samples at the start, then keep exactly `FLACtastic-Frames` samples, or everything after the priming when that header is absent. That makes transcoded playback gapless.
   - Seeking uses `s`, in the track's source samples.
   - Quality is `original`, `high`, `normal` or `low`. Store the user's mode, including `auto`, in `/v2/prefs` as `streamQuality`. In `auto`, measure throughput and pick per track. Never transcode when an offline copy exists.
5. **Artwork (§9.3).** Use `/v2/art/{id}?size=300|1000|original` with the existing image cache. The responses are immutable, so cache them by `artworkID`.
6. **Organizer on the server (§12).**
   - `GET` and `PUT /v2/organizer` replace `OrganizerProfilesStore` for a server library; offer to migrate the local profiles once.
   - The preview comes from `POST /v2/organizer/plan`, and applying sends back its `planHash`. On `409 planStale`, show the new plan.
   - Note that the server has no saved artist review decisions, so "primary artist only" can split differently from the Mac when a credit was decided by hand. Ask the user whether to add decisions to the protocol later.
7. **Uploads (§9.4).**
   - Send downloaded and imported files to the server library with resumable uploads: hash the file, `POST`, `PATCH` in 8–64 MiB chunks, resume from `HEAD` or `409 offsetMismatch`.
   - The server files each upload through the selected profile, so show the returned `relativePath`.
   - Handle `507 insufficientStorage` and quotas clearly.
8. **Tag edits (§10).**
   - The metadata editors send a `PATCH` with only the fields the user changed. Remember the three-state rule: absent, `null` or a value.
   - Album edits use `PATCH /v2/tracks`.
   - Before sending, show what will be replaced (last write wins on the server). Apply the returned `Track`; it may have moved.
9. **Playlists (§14).** For a server library, `PlaylistStore` reads and writes `/v2/playlists`, in the same JSON the app already uses. Last write wins: show what a save replaces when the server copy changed since it was read (watch `playlists` on `/v2/changes`).
10. **Preferences (§14).** Store `streamQuality`, `artistSeparator` (used by the server when it composes ARTIST strings) and a `clients.mac` object for Mac settings that should follow the user. Ask which settings belong there. Credentials never do.
11. **Listening history and stats (§15).**
    - Post `PlayEvent`s, batched and idempotent by `id`, plus `RecentContext`s.
    - Merge other devices' plays from `GET /v2/history?since=` into `ListeningStore`.
    - Home can use `/v2/stats?tz=` or keep computing locally from the merged history. Ask which.
    - Decide with the user whether existing local history is uploaded once.
12. **Offline copies (§13).**
    - The user picks albums, playlists or the whole library, with an optional v1 `SyncFilter`.
    - `POST /v2/manifest`, then run the existing `SyncDiff` with the server manifest as `incoming`, the copies' manifest as `local` and direction `pull`.
    - Download with ranged GETs, resuming partials, verify `contentHash`, install atomically, and write the server's `trackID` xattr.
    - Refresh when `/v2/changes` reports `tracks` or `playlists`. Playback prefers a verified offline copy.

## Testing

- **Run a real server.** The server only builds for Linux. Run it in a Debian 13 VM or container on the Mac (UTM, OrbStack, or Docker with `debian:trixie`):
  - either `sudo bash install.sh --binary <built binary>`,
  - or build it with `scripts/build-taglib.sh && go build`, then `flactastic-server user add …` and `flactastic-server serve`.
- **Unit tests** for:
  - fingerprint pinning (the certificate from a test server must produce the fingerprint `flactastic-server info` prints),
  - token storage,
  - mapping server `Track`s to app `Track`s,
  - gapless trimming (decode a transcoded stream and check that the sample count after trimming equals `FLACtastic-Frames`),
  - upload resume,
  - the offline-copy diff.
- **Integration tests** against the running server for each step above, with two users, to prove one device never sees another user's data.
- Every item in §18 of the protocol, the client conformance checklist, must be ticked.
- Report what you verified in the running app and what you could not.
