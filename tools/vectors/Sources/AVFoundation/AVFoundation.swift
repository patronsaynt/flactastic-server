// Linux stand-in for the AVFoundation calls in LibraryScanner. Every open
// fails, so only the TagLib path runs: the one the fingerprint depends on.
@_exported import CoreAudioTypes
import Foundation

public struct CMTime: Sendable {}
public func CMTimeGetSeconds(_ time: CMTime) -> Double { .nan }

public struct AVAsyncProperty<Root, Value>: Sendable {}
public extension AVAsyncProperty where Root == AVURLAsset, Value == CMTime {
    static var duration: AVAsyncProperty<AVURLAsset, CMTime> { .init() }
}

public final class AVURLAsset: Sendable {
    public init(url: URL) {}
    public func load<T>(_ property: AVAsyncProperty<AVURLAsset, T>) async throws -> T {
        throw CocoaError(.fileReadUnsupportedScheme)
    }
}

public final class AVAudioFormat {
    public var sampleRate: Double { 0 }
    public var streamDescription: UnsafePointer<AudioStreamBasicDescription> { fatalError() }
}

public final class AVAudioFile {
    public init(forReading url: URL) throws { throw CocoaError(.fileReadUnsupportedScheme) }
    public var processingFormat: AVAudioFormat { fatalError() }
    public var fileFormat: AVAudioFormat { fatalError() }
}
