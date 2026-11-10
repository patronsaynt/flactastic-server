// Linux stand-in for os.Logger: accepts the same call shapes, logs nothing.
public struct Logger: Sendable {
    public init(subsystem: String, category: String) {}
    public func notice(_ message: OSLogMessage) {}
    public func error(_ message: OSLogMessage) {}
    public func info(_ message: OSLogMessage) {}
    public func debug(_ message: OSLogMessage) {}
    public func warning(_ message: OSLogMessage) {}
}

public struct OSLogPrivacy: Sendable {
    public static let `public` = OSLogPrivacy()
    public static let `private` = OSLogPrivacy()
}

public struct OSLogMessage: ExpressibleByStringInterpolation {
    public struct StringInterpolation: StringInterpolationProtocol {
        public init(literalCapacity: Int, interpolationCount: Int) {}
        public mutating func appendLiteral(_ literal: String) {}
        public mutating func appendInterpolation<T>(_ value: T, privacy: OSLogPrivacy = .private) {}
    }
    public init(stringLiteral value: String) {}
    public init(stringInterpolation: StringInterpolation) {}
}
