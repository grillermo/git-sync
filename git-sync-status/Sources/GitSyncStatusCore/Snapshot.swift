import Foundation

/// One line of `git-sync status --json --follow`. Mirrors the Go
/// `status.Snapshot`: snake_case keys, whole-second ISO 8601 times, and lists
/// that are always present.
public struct Snapshot: Decodable, Sendable, Equatable {
    public var at: Date
    public var syncing: Bool
    public var problems: Int
    public var repos: [Repo]
    public var pending: [Pending]
    public var error: String?

    public enum State: String, Decodable, Sendable {
        case syncing, error, warn, ok, never
    }

    public struct Repo: Decodable, Sendable, Equatable {
        public var repo: String
        public var path: String
        public var state: State
        public var lastSync: Date?
        public var running: [Running]
        public var problems: [Problem]
    }

    public struct Running: Decodable, Sendable, Equatable {
        public var op: String
        public var peer: String?
        public var started: Date
    }

    public struct Problem: Decodable, Sendable, Equatable {
        public var ts: Date
        public var op: String
        public var peer: String?
        public var status: String
        public var msg: String
    }

    public struct Pending: Decodable, Sendable, Equatable {
        public var kind: String
        public var repo: String
        public var path: String
        public var peer: String?
        public var since: Date
    }

    public static func decode(_ line: Data) throws -> Snapshot {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        d.dateDecodingStrategy = .iso8601
        return try d.decode(Snapshot.self, from: line)
    }
}
