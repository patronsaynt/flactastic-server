// swift-tools-version: 6.0
// Compiles the Mac app's own sources, unmodified, to emit the test vectors the
// server must agree with. Run fetch.sh first. On Linux the shims stand in for
// the Apple-only modules; on macOS the real frameworks are used.
import Foundation
import PackageDescription

let deps = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
    .appendingPathComponent("../../.deps/taglib").standardizedFileURL.path
let linux = TargetDependencyCondition.when(platforms: [.linux])

let package = Package(
    name: "vectors",
    platforms: [.macOS(.v14)],
    dependencies: [.package(url: "https://github.com/apple/swift-crypto.git", "3.0.0"..<"5.0.0")],
    targets: [
        .systemLibrary(name: "CTagLib", path: "Sources/CTagLib"),
        .target(name: "CTagLibHelper", dependencies: ["CTagLib"], path: "Sources/CTagLibHelper",
                publicHeadersPath: ".", cxxSettings: [.unsafeFlags(["-I\(deps)/include"])]),
        .target(name: "CoreAudioTypes", path: "Sources/CoreAudioTypes"),
        .target(name: "AVFoundation", path: "Sources/AVFoundation"),
        .target(name: "os", path: "Sources/os"),
        .target(name: "CryptoKit", dependencies: [.product(name: "Crypto", package: "swift-crypto")],
                path: "Sources/CryptoKit"),
        .executableTarget(
            name: "vectors",
            dependencies: [
                "CTagLibHelper",
                .target(name: "CoreAudioTypes", condition: linux),
                .target(name: "AVFoundation", condition: linux),
                .target(name: "os", condition: linux),
                .target(name: "CryptoKit", condition: linux),
            ],
            path: "Sources",
            exclude: ["CTagLib", "CTagLibHelper", "CoreAudioTypes", "AVFoundation", "os", "CryptoKit"],
            sources: ["Mac", "vectors"],
            swiftSettings: [.unsafeFlags(["-Xcc", "-I\(deps)/include"])],
            linkerSettings: [.unsafeFlags(["-L\(deps)/lib", "-ltag_c", "-ltag", "-lz", "-lstdc++"])]
        ),
    ],
    swiftLanguageModes: [.v6]
)
