#!/usr/bin/env bash
# Copies the Mac sources the generator compiles, unmodified, from one pinned
# commit of the revisions branch. Re-run after bumping COMMIT.
set -Eeuo pipefail

COMMIT="066c00d62368fa7f8d480f9da7b054a116e75732"
BASE="https://raw.githubusercontent.com/patronsaynt/flactastic-macos/$COMMIT"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

SWIFT=(
    Library/AudioFileFormat.swift Library/Track.swift Library/MultiValueParser.swift
    Library/GenreResolver.swift Library/ArtistResolver.swift Library/DiscLayout.swift
    Library/LibraryScanner.swift Library/Playlist.swift
    Persistence/TrackMetadataCache.swift
    Sync/Protocol/ContentHasher.swift Sync/Protocol/PathSanitizer.swift
    Sync/Protocol/SyncFilter.swift Sync/Protocol/Manifest.swift Sync/Protocol/SyncProtocol.swift
    Sync/Engine/SyncDiff.swift Sync/Engine/ManifestBuilder.swift Sync/Engine/ContentHashCache.swift
    UI/Import/ImportCopy.swift
    UI/Organizer/OrganizerProfile.swift UI/Organizer/OrganizerTemplate.swift UI/Organizer/OrganizerPlanner.swift
)
HELPER=(taglib_helpers.h taglib_helpers.cpp)
FIXTURES=(cur-aa-diff.mp3 cur-acdc.flac cur-comp1.m4a cur-comp2.m4a cur-dl.flac cur-dl-solo.flac
    cur-dnb.mp3 cur-ewf.flac cur-guest.flac cur-host1.flac cur-host2.flac cur-host3.mp3 cur-multi.flac
    cur-simon.mp3 cur-single.flac cur-tyler.m4a ext-backslash.flac ext-comma.flac ext-mbid.flac
    ext-native.flac ext-native.m4a ext-native.mp3 ext-plural.flac ext-plural.mp3 ext-semicolon.mp3
    ext-slash.flac)

rm -rf "$HERE/Sources/Mac" "$HERE/Sources/CTagLibHelper" "$HERE/fixtures"
mkdir -p "$HERE/Sources/Mac" "$HERE/Sources/CTagLibHelper" "$HERE/fixtures"
for f in "${SWIFT[@]}"; do curl -fsSL "$BASE/Sources/flactastic/$f" -o "$HERE/Sources/Mac/$(basename "$f")"; done
for f in "${HELPER[@]}"; do curl -fsSL "$BASE/Sources/CTagLibHelper/$f" -o "$HERE/Sources/CTagLibHelper/$f"; done
for f in "${FIXTURES[@]}"; do curl -fsSL "$BASE/Tests/Fixtures/audio/$f" -o "$HERE/fixtures/$f"; done
curl -fsSL "$BASE/Tests/Fixtures/metadata-parsing.json" -o "$HERE/fixtures/metadata-parsing.json"
echo "$COMMIT" > "$HERE/fixtures/COMMIT"
