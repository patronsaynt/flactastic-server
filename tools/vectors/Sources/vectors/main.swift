// Runs the Mac's own code over fixed and seeded-random inputs and writes what
// it produced. The server's tests replay every case and must agree exactly.
//
// usage: vectors <mac-fixtures-dir> <audio-dir> <out.json>
// <audio-dir> is the server's testdata/audio, which holds copies of the Mac's
// own fixture files alongside the server's.
import Foundation

let args = CommandLine.arguments
guard args.count == 4 else { fatalError("usage: vectors <mac-fixtures> <extra-audio> <out.json>") }
let macFixtures = URL(fileURLWithPath: args[1])
let extraAudio = URL(fileURLWithPath: args[2])

struct SplitMix: RandomNumberGenerator {
    var state: UInt64
    mutating func next() -> UInt64 {
        state &+= 0x9E37_79B9_7F4A_7C15
        var z = state
        z = (z ^ (z >> 30)) &* 0xBF58_476D_1CE4_E5B9
        z = (z ^ (z >> 27)) &* 0x94D0_49BB_1331_11EB
        return z ^ (z >> 31)
    }
}
var rng = SplitMix(state: 0xF1AC_7A57_1C)

@MainActor func pick<T>(_ xs: [T]) -> T { xs.randomElement(using: &rng)! }
@MainActor func chance(_ p: Double) -> Bool { Double.random(in: 0..<1, using: &rng) < p }
@MainActor func maybe<T>(_ x: @autoclosure () -> T, _ p: Double = 0.7) -> T? { chance(p) ? x() : nil }
@MainActor func json(_ x: Any?) -> Any { x ?? NSNull() }

/// Strings chosen to disagree between naive ports and Foundation: Unicode
/// whitespace, decomposed forms, case folding, grapheme clusters, separators.
let pool: [String] = [
    "", " ", "Artist", "  padded  ", "\u{00A0}nbsp\u{00A0}", "\u{3000}ideographic", "line\u{2028}sep",
    "\nnewline\n", "\ttab", "\u{85}nel", "\u{200B}zero width", "\u{FEFF}bom", "Caf\u{00E9}", "Cafe\u{0301}",
    "\u{212B}ngstrom", "\u{FB01}ne", "\u{017F}tra\u{00DF}e", "\u{212A}elvin", "\u{0130}stanbul", "\u{0131}", "Stra\u{00DF}e",
    "\u{1F468}\u{200D}\u{1F469}\u{200D}\u{1F467}", "\u{1F1EF}\u{1F1F5}", "A & B", "A&B", "A feat. B", "A FEAT. B",
    "A Feat B", "A ft. B", "A x B", "A X B", "Ax B", "A ; B", "A;B", ";", " ; ", "A / B", "AC/DC", "A\\B", "A\\\\B",
    "A\u{0}B", "Earth, Wind & Fire", "Drum & Bass", "R&B/Soul", "a ft b", "A vs. B", "A VS B", "A with B",
    "A Featuring B", "x\u{1F}y", "\u{01C5}", "\u{216B}", "\u{0401}\u{0436}", "\u{03A3}\u{03AF}\u{03C3}\u{03C5}\u{03C6}\u{03BF}\u{03C2}",
    "\u{202E}rtl", "Bj\u{00F6}rk", "Bjo\u{0308}rk", "\u{00C6}ther", "\u{00D8}resund", "Tyler, The Creator",
    "Simon & Garfunkel", "Host & Guest", "Host", "Guest", "Deadmau5; Kaskade", "a ; A", "Rock", "rock", "Pop",
    " \u{0301}lead mark", ":\u{0301}", "a:b*c?d\"e<f>g|h", ".hidden", "..", "...", "x\u{3000}", "\u{1D400}math",
]

@MainActor func randomTrack() -> (title: String, artist: String?, albumArtist: String?, album: String?, trackNumber: Int?,
                       genre: String?, secondary: [String], year: Int?, comp: Bool, mix: Bool?) {
    (pick(pool), maybe(pick(pool)), maybe(pick(pool), 0.4), maybe(pick(pool)), maybe(pick([0, 1, 7, 12, 99, 120, -3])),
     maybe(pick(pool)), (0..<Int.random(in: 0...3, using: &rng)).map { _ in pick(pool) },
     maybe(pick([0, 999, 1968, 2009, 2024])), chance(0.3), pick([nil, false, true]))
}

// MARK: Fingerprints

var fingerprints: [[String: Any]] = []
@MainActor func addFingerprint(_ t: (title: String, artist: String?, albumArtist: String?, album: String?, trackNumber: Int?,
                          genre: String?, secondary: [String], year: Int?, comp: Bool, mix: Bool?)) {
    let fp = TagFingerprint.compute(title: t.title, artist: t.artist, albumArtist: t.albumArtist, album: t.album,
                                    trackNumber: t.trackNumber, genre: t.genre, secondaryGenres: t.secondary,
                                    year: t.year, isCompilation: t.comp, isMixCompilation: t.mix)
    fingerprints.append([
        "title": t.title, "artist": json(t.artist), "albumArtist": json(t.albumArtist), "album": json(t.album),
        "trackNumber": json(t.trackNumber), "genre": json(t.genre), "secondaryGenres": t.secondary,
        "year": json(t.year), "isCompilation": t.comp, "isMixCompilation": json(t.mix), "expected": fp,
    ])
}
addFingerprint(("Song", "Artist", nil, "Album", 1, "Electronic", [], 2002, false, nil))
addFingerprint(("Song", "Artist", nil, "Album", 1, "Electronic", [], 2002, false, false))
addFingerprint(("Song", "Artist", nil, "Album", 1, "Electronic", [], 2002, false, true))
addFingerprint(("  Song\n", "Artist ", "", "Album", 1, "Electronic", ["IDM", "Ambient"], 2002, true, nil))
addFingerprint(("Song", nil, nil, nil, nil, nil, [], nil, false, nil))
for _ in 0..<400 { addFingerprint(randomTrack()) }

// MARK: Multi-value parsing

struct FixtureCase: Decodable {
    struct Generated: Decodable { var kind: String; var length: Int?; var count: Int? }
    enum DecisionValue: Decodable {
        case keep, split([String])
        init(from decoder: Decoder) throws {
            let c = try decoder.singleValueContainer()
            if let parts = try? c.decode([String].self) { self = .split(parts) } else { self = .keep }
        }
        var decision: MultiValueParser.Decision {
            switch self { case .keep: .keep; case .split(let p): .split(p) }
        }
    }
    var name: String
    var field: String
    var native: [String]?
    var plural: [String]?
    var known: [String]?
    var mbidCount: Int?
    var decision: DecisionValue?
    var generated: Generated?
}

@MainActor func encodeOutcome(_ o: MultiValueParser.Outcome) -> Any {
    switch o {
    case .values(let v): return ["values": v]
    case .review(let raw): return ["review": raw]
    }
}

var multiValue: [[String: Any]] = []
@MainActor func addMultiValue(name: String, field: MultiValueParser.Field, plural: [String], native: [String],
                   mbidCount: Int, known: [String], decision: MultiValueParser.Decision?) {
    let knownKeys = Set(known.map { ArtistResolver.key(for: $0) })
    let outcome = MultiValueParser.resolve(field: field, plural: plural, native: native, mbidCount: mbidCount,
                                           isKnown: { knownKeys.contains(ArtistResolver.key(for: $0)) }, decision: decision)
    let credit = MultiValueParser.storedCredit(plural: plural, native: native, mbidCount: mbidCount)
    let genres = MultiValueParser.storedGenres(native: native)
    let first = native.first ?? ""
    var decisionJSON: Any = NSNull()
    switch decision {
    case .keep?: decisionJSON = "keep"
    case .split(let parts)?: decisionJSON = parts
    case nil: break
    }
    multiValue.append([
        "name": name, "field": field.rawValue, "plural": plural, "native": native, "mbidCount": mbidCount,
        "known": known, "decision": decisionJSON,
        "resolve": encodeOutcome(outcome),
        "storedCredit": ["value": json(credit.value), "isList": credit.isList],
        "storedGenres": ["primary": json(genres.primary), "secondary": genres.secondary],
        "genreSplit": { let g = GenreResolver.split(first); return ["primary": json(g.primary), "secondary": g.secondary] }(),
        "splitSafe": json(MultiValueParser.splitSafe(first)),
        "splitAmbiguous": MultiValueParser.splitAmbiguous(first),
        "hasAmbiguousSeparator": MultiValueParser.hasAmbiguousSeparator(first),
        "valuesOfStored": MultiValueParser.values(ofStored: first),
        "splitIdentifiers": MultiValueParser.splitIdentifiers(native),
    ])
}

let fixtureData = try Data(contentsOf: macFixtures.appendingPathComponent("metadata-parsing.json"))
struct FixtureFile: Decodable { var cases: [FixtureCase] }
for c in try JSONDecoder().decode(FixtureFile.self, from: fixtureData).cases {
    var native = c.native ?? []
    if let g = c.generated {
        native = g.kind == "longValue" ? [String(repeating: "x", count: g.length ?? 0)]
                                       : (1...(g.count ?? 1)).map { "Artist \($0)" }
    }
    addMultiValue(name: c.name, field: MultiValueParser.Field(rawValue: c.field)!, plural: c.plural ?? [],
                  native: native, mbidCount: c.mbidCount ?? 0, known: c.known ?? [], decision: c.decision?.decision)
}
let longPieces = ["e\u{0301}", "\u{1F468}\u{200D}\u{1F469}\u{200D}\u{1F467}", "x", "\u{1F1EF}\u{1F1F5}", "\r\n"]
for n in [1023, 1024, 1025, 3000] {
    let s = (0..<n).map { i in longPieces[i % longPieces.count] }.joined()
    addMultiValue(name: "long graphemes \(n)", field: .artist, plural: [], native: [s], mbidCount: 0, known: [], decision: nil)
    addMultiValue(name: "long plural \(n)", field: .artist, plural: [s, "B"], native: ["x"], mbidCount: 0, known: [], decision: nil)
}
addMultiValue(name: "many genres", field: .genre, plural: [], native: (1...70).map { "G\($0)" }, mbidCount: 0, known: [], decision: nil)
for i in 0..<500 {
    let field = pick([MultiValueParser.Field.artist, .albumArtist, .genre])
    let joined = (0..<Int.random(in: 1...3, using: &rng)).map { _ in pick(pool) }
        .joined(separator: pick([" & ", ";", " ; ", ", ", " feat. ", " / ", "/", " x ", "\u{0}", "\\", ""]))
    let native = chance(0.8) ? [joined] : (0..<Int.random(in: 0...4, using: &rng)).map { _ in pick(pool) }
    let plural = chance(0.2) ? (0..<Int.random(in: 1...3, using: &rng)).map { _ in pick(pool) } : []
    let known = (0..<Int.random(in: 0...4, using: &rng)).map { _ in pick(pool) }
    let decision: MultiValueParser.Decision? = chance(0.1) ? pick([.keep, .split([pick(pool), pick(pool)])]) : nil
    addMultiValue(name: "random \(i)", field: field, plural: plural, native: native,
                  mbidCount: pick([0, 0, 1, 2, 3]), known: known, decision: decision)
}

// MARK: Artist keys

var artistKeys: [[String: Any]] = pool.map { ["raw": $0, "key": ArtistResolver.key(for: $0)] }
for _ in 0..<200 {
    let raw = pick(pool) + pick(["", " ", "  ", "\t"]) + pick(pool)
    artistKeys.append(["raw": raw, "key": ArtistResolver.key(for: raw)])
}

// MARK: Paths

let pathPieces = ["..", ".", "...", "....", "a", "~x", "C:", "c:", "1:", "\u{00E9}:", "\u{01C5}:", "\u{216B}:", "_:", "",
                  "x.flac", "x.FLAC", "x.m4a", "x.wave", "x.aif", "x.dylib", "x.sh", "noext", ".hidden", "a:b",
                  "a*b?", " spaced ", "e\u{0301}", ".\u{0301}.", "\u{FF0E}\u{FF0E}", ":", "::", "\"", "<>|",
                  String(repeating: "a", count: 255), String(repeating: "a", count: 256),
                  String(repeating: "\u{00E9}", count: 128), "\u{0}", "Artist", "Album (2009)", " ", "\u{3000}"]
var paths: [[String: Any]] = []
@MainActor func addPath(_ raw: String) {
    @MainActor func run(_ f: (String) throws -> String) -> [String: Any] {
        do { return ["ok": try f(raw)] } catch { return ["error": "\(error)"] }
    }
    paths.append(["raw": raw, "relative": run(PathSanitizer.sanitizeRelativePath),
                  "audio": run(PathSanitizer.sanitizeAudioRelativePath)])
}
for raw in ["Artist/Album/01 Song.flac", "/etc/passwd", "\\\\server\\share", "~/x.flac", "C:\\x.flac", "a//b.flac",
            "a/b/", "a/./b.flac", "a/../b.flac", "....//x.flac", "Artist\\Album\\x.flac", "",
            String(repeating: "a/", count: 32) + "x.flac", String(repeating: "a/", count: 31) + "x.flac",
            String(repeating: "abcdefgh/", count: 340) + "x.flac", "\u{0}/x.flac", "a/b\u{0}.flac"] { addPath(raw) }
for _ in 0..<600 {
    let depth = Int.random(in: 1...5, using: &rng)
    addPath((0..<depth).map { _ in pick(pathPieces) }.joined(separator: pick(["/", "/", "/", "\\"])))
}

// MARK: Playlists and the wire encoding

nonisolated(unsafe) let isoFractional: ISO8601DateFormatter = {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return f
}()
let wireEncoder: JSONEncoder = {
    let e = JSONEncoder()
    e.outputFormatting = [.sortedKeys]
    e.dateEncodingStrategy = .custom { date, encoder in
        var c = encoder.singleValueContainer()
        try c.encode(isoFractional.string(from: date))
    }
    e.dataEncodingStrategy = .base64
    return e
}()
@MainActor func randomUUID() -> UUID {
    var b = [UInt8](repeating: 0, count: 16)
    for i in b.indices { b[i] = UInt8.random(in: 0...255, using: &rng) }
    b[6] = (b[6] & 0x0F) | 0x40
    b[8] = (b[8] & 0x3F) | 0x80
    return UUID(uuid: (b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15]))
}
@MainActor func randomDate() -> Date {
    Date(timeIntervalSince1970: Double(Int.random(in: 1_000_000_000...1_900_000_000, using: &rng))
        + pick([0, 0.5, 0.687, 0.0004, 0.9996, 0.123456789]))
}
@MainActor func randomHex() -> String { (0..<32).map { _ in String(format: "%02x", UInt8.random(in: 0...255, using: &rng)) }.joined() }

var playlists: [[String: Any]] = []
for _ in 0..<60 {
    let p = Playlist(id: randomUUID(), name: pick(pool),
                     entries: (0..<Int.random(in: 0...6, using: &rng)).map { _ in
                         PlaylistEntry(id: randomUUID(), trackID: maybe(randomUUID(), 0.8), relativePath: pick(pathPieces))
                     },
                     dateCreated: randomDate(), customArtwork: maybe(Data([0, 1, 2, 250, 251]), 0.2),
                     description: maybe(pick(pool), 0.3))
    playlists.append([
        "wire": String(decoding: try wireEncoder.encode(p), as: UTF8.self),
        "contentHash": ManifestBuilder.contentHash(for: p),
        "manifestEntry": String(decoding: try wireEncoder.encode(ManifestBuilder.manifestEntry(for: p)), as: UTF8.self),
    ])
}

// MARK: Diff plans

let formats = AudioFileFormat.allCases
@MainActor func randomEntry(id: UUID? = nil, hash: String? = nil) -> TrackManifestEntry {
    TrackManifestEntry(trackID: id ?? randomUUID(), relativePath: pick(pathPieces) + "/" + pick(["a.flac", "b.mp3", "c.m4a"]),
                       fileSize: Int64(pick([1, 1000, 50_000_000, 3_000_000_000])), contentHash: hash ?? randomHex(),
                       format: pick(formats), tagFingerprint: pick(["f1", "f2", randomHex()]),
                       title: pick(pool), artist: maybe(pick(pool)), album: maybe(pick(pool)), albumArtist: maybe(pick(pool), 0.3))
}
var plans: [[String: Any]] = []
for _ in 0..<80 {
    let local = (0..<Int.random(in: 0...8, using: &rng)).map { _ in randomEntry() }
    var incoming: [TrackManifestEntry] = []
    for e in local where chance(0.6) {
        // Same ID or same bytes, with a chance of differing tags or audio.
        let sameID = chance(0.7)
        incoming.append(TrackManifestEntry(
            trackID: sameID ? e.trackID : randomUUID(), relativePath: e.relativePath, fileSize: e.fileSize,
            contentHash: chance(0.7) ? e.contentHash : randomHex(), format: e.format,
            tagFingerprint: chance(0.7) ? e.tagFingerprint : "changed", title: chance(0.8) ? e.title : pick(pool),
            artist: e.artist, album: e.album, albumArtist: e.albumArtist))
    }
    incoming += (0..<Int.random(in: 0...6, using: &rng)).map { _ in randomEntry() }
    let localPlaylists = (0..<Int.random(in: 0...3, using: &rng)).map { _ in
        PlaylistManifestEntry(id: randomUUID(), name: pick(pool), dateCreated: randomDate(), entryCount: 2, contentHash: randomHex())
    }
    var incomingPlaylists = localPlaylists.filter { _ in chance(0.5) }.map {
        PlaylistManifestEntry(id: $0.id, name: chance(0.7) ? $0.name : pick(pool), dateCreated: $0.dateCreated,
                              entryCount: $0.entryCount, contentHash: chance(0.7) ? $0.contentHash : randomHex())
    }
    incomingPlaylists += (0..<Int.random(in: 0...2, using: &rng)).map { _ in
        PlaylistManifestEntry(id: randomUUID(), name: pick(pool), dateCreated: randomDate(), entryCount: 1, contentHash: randomHex())
    }
    let filter = SyncFilter(excludedFormats: chance(0.3) ? [pick(formats)] : [],
                            maxFileSizeBytes: maybe(Int64(60_000_000), 0.3), includePlaylists: chance(0.85),
                            playlistIDAllowlist: chance(0.2) ? Set(incomingPlaylists.prefix(1).map(\.id)) : nil)
    let direction = pick([SyncDirection.push, .pull])
    let inM = LibraryManifest(deviceID: randomUUID(), generatedAt: randomDate(), tracks: incoming.shuffled(using: &rng),
                              playlists: incomingPlaylists)
    let localM = LibraryManifest(deviceID: randomUUID(), generatedAt: randomDate(), tracks: local, playlists: localPlaylists)
    let plan = SyncDiff.plan(incoming: inM, local: localM, direction: direction, filter: filter)
    plans.append([
        "incoming": String(decoding: try wireEncoder.encode(inM), as: UTF8.self),
        "local": String(decoding: try wireEncoder.encode(localM), as: UTF8.self),
        "filter": String(decoding: try wireEncoder.encode(filter), as: UTF8.self),
        "direction": direction.rawValue,
        "plan": String(decoding: try wireEncoder.encode(plan), as: UTF8.self),
        "planHash": plan.planHash,
    ])
}

// MARK: Organizer

let tokens = OrganizerTemplate.allTokens.map(\.placeholder)
@MainActor func randomTemplate() -> String {
    (0..<Int.random(in: 1...3, using: &rng)).map { _ in chance(0.7) ? pick(tokens) : pick([" - ", "/", "s", " (", ")", "", ":", "."]) }.joined()
}
let root = URL(fileURLWithPath: "/lib", isDirectory: true)
var organizer: [[String: Any]] = []
for _ in 0..<60 {
    var tracks: [Track] = []
    for _ in 0..<Int.random(in: 1...8, using: &rng) {
        let t = randomTrack()
        let ext = pick(["flac", "FLAC", "mp3", "m4a", "wav"])
        var track = Track(url: root.appendingPathComponent("in/\(tracks.count).\(ext)"), title: t.title, artist: t.artist,
                          albumArtist: t.albumArtist, album: t.album, trackNumber: t.trackNumber.map { abs($0) },
                          fileFormat: AudioFileFormat.classify(pathExtension: ext)!, genre: t.genre, year: t.year.map { abs($0) })
        track.artistIsList = chance(0.2)
        tracks.append(track)
        if chance(0.3) { tracks.append(track.rebuilt(id: UUID(), url: root.appendingPathComponent("in/dup\(tracks.count).\(ext)"))) }
    }
    let profile = chance(0.4) ? pick(OrganizerProfile.presets) : OrganizerProfile(
        name: "random",
        levels: (0..<Int.random(in: 0...3, using: &rng)).map { _ in
            HierarchyLevel(groupBy: pick(GroupingField.allCases), name: pick(["", "Level", " "]), nameTemplate: randomTemplate())
        },
        fileTemplate: randomTemplate(), usePrimaryArtistOnly: chance(0.5))
    let resolver = ArtistResolver(tracks: tracks)
    let ops = OrganizerPlanner.plan(tracks: tracks, profile: profile, rootURL: root, resolver: resolver)
    organizer.append([
        "profile": String(decoding: try wireEncoder.encode(profile), as: UTF8.self),
        "tracks": tracks.map { t -> [String: Any] in
            ["id": t.id.uuidString, "path": String(t.url.path.dropFirst(root.path.count + 1)), "title": t.title,
             "artist": json(t.artist), "albumArtist": json(t.albumArtist), "album": json(t.album),
             "trackNumber": json(t.trackNumber), "year": json(t.year), "genre": json(t.genre),
             "format": t.fileFormat.rawValue, "artistIsList": t.artistIsList, "albumArtistIsList": t.albumArtistIsList]
        },
        "operations": ops.map { op -> [String: Any] in
            let status: String
            switch op.status {
            case .move: status = "move"
            case .unchanged: status = "unchanged"
            case .conflict: status = "conflict"
            }
            return ["id": op.track.id.uuidString, "destination": String(op.destinationURL.path.dropFirst(root.path.count + 1)),
                    "status": status]
        },
    ])
}

// MARK: Real files, through the Mac's own scanner

var files: [[String: Any]] = []
let scanner = LibraryScanner()
for dir in [extraAudio] {
    let names = try FileManager.default.contentsOfDirectory(atPath: dir.path).sorted()
    for name in names {
        let url = dir.appendingPathComponent(name)
        guard let base = Track.makeFromURL(url) else { continue }
        let t = await scanner.loadMetadata(for: base)
        files.append([
            "file": name, "title": t.title, "artist": json(t.artist), "albumArtist": json(t.albumArtist),
            "album": json(t.album), "trackNumber": json(t.trackNumber), "genre": json(t.genre),
            "secondaryGenres": t.secondaryGenres, "year": json(t.year), "isCompilation": t.isCompilation,
            "isMixCompilation": t.isMixCompilation, "label": json(t.label), "discNumber": json(t.discNumber),
            "discTotal": json(t.discTotal), "tagVersion": json(t.tagVersion), "artistIsList": t.artistIsList,
            "albumArtistIsList": t.albumArtistIsList, "hasArtwork": t.artwork != nil,
            "artworkSHA256": json(t.artwork.map { ContentHasher.hexDigest(of: $0) }),
            "tagFingerprint": TagFingerprint.compute(for: t),
            "contentHash": try ContentHasher.hexDigest(ofFileAt: url),
        ])
    }
}

let commit = (try? String(contentsOf: macFixtures.appendingPathComponent("COMMIT"), encoding: .utf8))?
    .trimmingCharacters(in: .whitespacesAndNewlines) ?? "unknown"
#if os(Linux)
let platform = "linux"
#else
let platform = "macos"
#endif
let output: [String: Any] = [
    "source": ["commit": commit, "platform": platform],
    "fingerprints": fingerprints, "multiValue": multiValue, "artistKeys": artistKeys, "paths": paths,
    "playlists": playlists, "plans": plans, "organizer": organizer, "files": files,
]
let data = try JSONSerialization.data(withJSONObject: output, options: [.sortedKeys, .prettyPrinted, .withoutEscapingSlashes])
try data.write(to: URL(fileURLWithPath: args[3]))
print("wrote \(fingerprints.count) fingerprints, \(multiValue.count) parser cases, \(paths.count) paths, \(plans.count) plans, \(organizer.count) organizer plans, \(files.count) files")
