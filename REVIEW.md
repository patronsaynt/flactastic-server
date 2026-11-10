# Review

Three passes over the finished server: security, performance, bloat. Every number below was measured on this project's machine: x86-64, 16 cores, Debian 13, kernel 7.1, music on tmpfs unless stated. Nothing was measured on a Raspberry Pi, on arm64 or over NFS, and the end of each section says what wasn't covered.

## 1. Security

### Found and fixed

| # | Finding | Severity | Fix |
|---|---------|----------|-----|
| 1 | Inside the hardened unit, `openat2` failed with ENOSYS. systemd's `RestrictSUIDSGID=` can't inspect `openat2`'s `how.mode`, so it blocks the call. Every library showed as offline in production while all tests passed outside systemd. Many container runtimes refuse `openat2` too. | High (availability) | Confinement now walks each path component with `openat(O_NOFOLLOW)`, relative to the previous directory's fd. It is equally race-free, needs no special kernel or seccomp policy, and refuses every symlink. Verified inside the real unit. |
| 2 | `isCompilation` and `isMixCompilation` in tag edits were silently ignored, because the Go field names didn't match the JSON keys. | Medium (integrity) | JSON tags added. A test now edits the flag and reads it back. |
| 3 | The sign-in rate limiter keyed on the raw username, so a stream of 1 MiB usernames could grow the map without bound. | Medium (DoS) | Usernames that don't match `^[a-z][a-z0-9_-]{0,31}$` are dropped before they reach the limiter. |
| 4 | A 10,000 × 10,000 PNG cover would decode to about 400 MB, too much for a Pi. | Medium (DoS) | Pictures are capped at 6,000 px per side, checked with `DecodeConfig` before decoding. Thumbnails are generated one at a time. |
| 5 | Data race: the user manager rewrote a running library's `User` while that library's goroutines read it. | Medium | A library's user record is now immutable; any change restarts the library. `go test -race ./...` is clean. |
| 6 | The Mac's `PathSanitizer` hole: it splits on whole characters, so a `/` followed by a combining mark isn't a separator to it. `"/ͯ0"` is accepted and lands at an absolute path; `" ~"` becomes `"~"`. Found by fuzzing the Go port. | Medium, on the Mac | The Go port re-checks each `/`-separated piece after the Swift-identical steps, so it can only reject more than the Mac. **The Mac app should get the same fix.** |
| 7 | A symlink planted inside a library was answered with a logged `500`. | Low | It is now a `404 notFound`, and the symlink is never followed. Covered by a test. |
| 8 | Upload IDs were case-sensitive, and a malformed `contentHash` returned `422` instead of `400`. | Low (contract) | Both fixed to match the protocol. |

### Checked, no change needed

- **Auth bypass.** Every route except `GET /v2/server` and `POST /v2/auth/login` passes through one function. It resolves the token to exactly one user and hands handlers only that user's library. Tokens are 256 random bits, stored as SHA-256 and looked up by hash.
- **Cross-user access.** The isolation suite covers this: track, file, stream, artwork, edit, delete, playlists, uploads (including another user's upload ID), device revocation, admin status, and an upload claiming another user's track ID. Every attempt gets `404` or `403`, and the other user's files are left untouched.
- **Timing.** Unknown usernames are verified against a dummy Argon2id hash, and token lookup is an index match on a hash.
- **Path traversal and symlinks.** Clients never choose a path: an upload contributes only its extension, and placement comes from the Organizer and the sanitizer. The confinement tests cover `..`, absolute paths, symlinked files and directories, renames out of the root, and no-replace semantics. The fuzz target `FuzzSanitizeRelativePath` ran 1.3M inputs with no failures (after fix 6).
- **Tag readers.** `FuzzRead` sent 30.8M mutated FLAC, MP3, M4A, WAV and AIFF inputs through TagLib in 5 minutes with no crash. The parsers also run inside the systemd sandbox.
- **Request size.** Headers are capped at 16 KiB. Every JSON body has a limit (1, 8 or 16 MiB). Upload bytes past the declared size are refused before they are written. Staged uploads count against quota and free space.
- **Rate limits.** Sign-in failures, unknown tokens and wrong current passwords all feed the per-IP and per-username back-off.
- **TLS.** TLS 1.3 only, with an ECDSA P-256 key. Trust comes from the SPKI pin confirmed at first connection, not from a CA.
- **Injection.**
  - SQL: every query is parameterized; the one dynamic column name comes from code.
  - HTTP headers: they only ever carry integers and hashes.
  - ffmpeg: its arguments are only numbers, and its input is an inherited fd, never a path.
- **Web UI, CSRF, XSS, CSP.** Not applicable: there is no web UI, and auth uses bearer tokens with no cookies.
- **systemd sandbox.**
  - `systemd-analyze security` scores **1.3 OK**.
  - `ProtectSystem=strict`; the only writable paths are the data dir and the library roots.
  - `NoNewPrivileges`, no capabilities, `@system-service` minus `@privileged @resources`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`.
- **Installer.**
  - It is shellcheck-clean and uses `set -Eeuo pipefail` and private `mktemp -d` dirs.
  - Every download is checked against `SHA256SUMS`, and only listed flag names can set variables.
  - Passwords never appear in argv, the environment or files: a `bash -x` trace contains the password 0 times.
  - A purge never deletes music.
- **Dependencies.** `govulncheck` finds no vulnerability in code we call.

### Not covered

- **HTTP range parsing** is Go's `http.ServeContent`, which is fuzzed upstream. It is exercised by tests here, not fuzzed.
- **Rate limiting on expensive endpoints.** Authenticated endpoints that touch the whole library (manifest, albums, Organizer plan) have no per-user request limit. Only a signed-in user can reach them.
- **Shared IPs.** Users behind one NAT share the per-IP sign-in back-off.

## 2. Performance

Test library: 100,001 tracks (1,000 folders × 100 files, plus one 300 s, 32.8 MB FLAC), 0.99 GB in total.

| Measure | Before | After | What changed |
|---|---|---|---|
| First scan, until browsable | 24.5 s | 24.3 s | – |
| First scan, until fully hashed | 61.7 s | **33.8 s** | The hasher reuses one 1 MiB buffer instead of allocating one per file |
| Peak RSS during first scan | 193 MB | **98 MB** | Same as above, plus memory is returned to the OS after each scan and when hashing finishes |
| Idle RSS after first scan | 173 MB | **63 MB** | Same |
| Cold start (restart, 100k unchanged), until ready | 1.5 s | 1.35 s | – |
| Idle RSS after cold start | 93 MB | **36 MB** | Memory returned after each scan |
| Organizer plan, 100k tracks | 6.7 s | **2.5 s** | Pooled collators (one was built per comparison); split results memoized per plan. The planner alone went from 1.59 s to 0.85 s per op |

Measured once and left alone (no problem found):

| Measure | Result |
|---|---|
| Mirror the whole track list (`/v2/tracks`, 21 pages of 5,000) | 1.8 s |
| `/v2/albums` / `/v2/search` / whole-library manifest | 0.60 s (3.9 MB) / 0.10 s / 0.76 s (38.6 MB) |
| 2,000 ranged GETs of 1 MiB, 200 concurrent, over TLS | 968 MiB/s; time to first byte p50 4.7 ms, p99 10.9 ms; total p99 14.7 ms; all `206`; peak RSS 65 MB |
| 100 full-file streams throttled to 256 KB/s | +2.4 MB RSS in total (about 24 KB per connection); file descriptors back to baseline afterwards |
| 20 concurrent transcodes | 16 served (time to first byte average 280 ms, max 365 ms), 4 answered `503 transcodeUnavailable` as designed; a single transcode starts in 108 ms |
| Gapless transcode (pink noise, 96 kHz → 48 kHz AAC) | Encoder delay measured at exactly 1024 samples. Seeks land on the requested sample (within half an output sample when a 96 kHz position is odd) |

### Not covered

- **NFS.** Mounting needs root, which wasn't available. The server polls network filesystems every 10 minutes instead of using inotify, and an incremental pass costs one `stat` per file, so expect cold-start time to scale with the share's `stat` latency.
- **Raspberry Pi and arm64.** These weren't available. The memory figures above are the most relevant guide.

## 3. Bloat

| | Before | After |
|---|---|---|
| Go source, excluding tests | 6,800 lines | **6,593** |
| Tests | 1,338 lines | 1,316 |
| Release binary (stripped) | 13,569,432 bytes | 13,569,400 bytes |
| Direct dependencies | 6 | 6 |

Removed:
- the Go port of `SyncDiff` (`Diff`, `SyncPlan.Hash`, `Restricted`, `TotalTransferBytes`, `differingFields`) and its plan types. Clients run the diff, and the server never called it; `deadcode` confirmed this. The plan vectors stay in `vectors.json` for clients to check against.
- `SyncFilter.ExceedsTotalCap`
- the `invalidPath` error, which no endpoint returns, from the code and both docs
- an unused string comparer, a hand-rolled context type and a custom integer formatter
- one loop that `staticcheck` simplified

`deadcode` and `staticcheck` now report nothing.

Kept, with reasons:
- **x/text collation tables (about 1.2 MB):** they reproduce the Mac's `localizedStandardCompare` order, which the Organizer vectors check.
- **uniseg (about 0.3 MB):** it provides grapheme clusters for the tag-value caps.
- **x/image:** it gives thumbnails better quality than a hand-written scaler.
- **The 33 MB `.bss`:** this is Go's FIPS DRBG reservation, which is virtual and never resident.

Every dependency is justified in [DESIGN.md](DESIGN.md#dependencies).
