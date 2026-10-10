import Foundation

public enum Tone: Sendable, Equatable {
    case ok, error, warn, syncing, muted
}

/// One line of the window's table: a repo, or a queued delivery under
/// "Pending". Clicking any row copies `path`.
public struct Row: Identifiable, Equatable, Sendable {
    public enum Kind: Sendable, Equatable { case repo, pending }

    public var id: String
    public var kind: Kind
    public var name: String
    public var symbol: String
    public var state: String
    /// The machine(s) this row's work goes to or failed against; "–" if none.
    public var dest: String
    public var tone: Tone
    public var when: Date?
    public var detail: String
    public var fullDetail: String
    public var path: String
}

/// The table's rows, in the order git-sync sent them (it already sorts
/// syncing, then problems, then most recent).
public func rows(for s: Snapshot) -> (repos: [Row], pending: [Row]) {
    (s.repos.map(repoRow), s.pending.map(pendingRow))
}

func repoRow(_ r: Snapshot.Repo) -> Row {
    let problems = r.problems.map { "\(label($0.op, $0.peer)): \($0.msg)" }
    let (symbol, state, tone): (String, String, Tone) = switch r.state {
    case .syncing: ("↻", "syncing (\(r.running.first?.op ?? "…"))", .syncing)
    case .error: ("✗", "error", .error)
    case .warn: ("⚠", "warn", .warn)
    case .ok: ("✓", "ok", .ok)
    case .never: ("–", "never synced", .muted)
    }
    let peers = (r.running.map(\.peer) + r.problems.map(\.peer)).compactMap { $0 }.filter { !$0.isEmpty }
    var seen = Set<String>()
    let dest = peers.filter { seen.insert($0).inserted }.joined(separator: ", ")
    return Row(
        id: "repo:" + r.repo, kind: .repo, name: r.repo, symbol: symbol, state: state, dest: dest.isEmpty ? "–" : dest, tone: tone,
        when: r.lastSync, detail: problems.first ?? "", fullDetail: problems.joined(separator: "\n"),
        path: r.path
    )
}

func pendingRow(_ p: Snapshot.Pending) -> Row {
    let isPush = p.kind == "push"
    let to = isPush ? "remote" : (p.peer ?? "?")
    let why = isPush ? "remote unreachable, will retry" : "\(to) offline, will retry"
    return Row(
        id: "pending:\(p.kind):\(p.peer ?? ""):\(p.repo)", kind: .pending, name: p.repo,
        symbol: "→", state: "queued", dest: to, tone: .muted, when: p.since,
        detail: why, fullDetail: why, path: p.path
    )
}

func label(_ op: String, _ peer: String?) -> String {
    guard let peer, !peer.isEmpty else { return op }
    return "\(op) → \(peer)"
}

/// "now", "2 min. ago", "1 hr. ago"; "–" for never.
public func ago(_ date: Date?, now: Date) -> String {
    guard let date else { return "–" }
    if now.timeIntervalSince(date) < 60 { return "now" }
    let f = RelativeDateTimeFormatter()
    f.locale = Locale(identifier: "en_US")
    f.unitsStyle = .short
    return f.localizedString(for: date, relativeTo: now)
}

/// What the menu bar icon shows: the glyph turning while anything syncs, and a
/// red dot while any repo has a problem. Pending work never sets the dot.
public struct IconState: Equatable, Sendable {
    public var syncing: Bool
    public var problem: Bool

    public init(syncing: Bool, problem: Bool) {
        self.syncing = syncing
        self.problem = problem
    }

    public init(_ s: Snapshot?) {
        self.init(syncing: s?.syncing ?? false, problem: (s?.problems ?? 0) > 0)
    }
}
