import Foundation

public enum Tone: Sendable, Equatable {
    case ok, error, warn, syncing, muted
}

/// One line of the window's table: a repo, or a queued delivery under
/// "Pending". Clicking its detail runs `git-sync` with `retry`, if set.
public struct Row: Identifiable, Equatable, Sendable {
    public enum Kind: Sendable, Equatable { case repo, pending }

    public var id: String
    public var kind: Kind
    public var name: String
    public var symbol: String
    public var state: String
    public var tone: Tone
    public var when: Date?
    public var detail: String
    public var fullDetail: String
    public var path: String
    /// The git-sync arguments that redo what failed, nil when nothing did.
    /// See `retry(_:_:_:)`; a queued delivery is drained with `retry`.
    public var retry: [String]? = nil
}

/// The table's rows, in the order git-sync sent them (it already sorts
/// syncing, then problems, then most recent).
public func rows(for s: Snapshot) -> (repos: [Row], pending: [Row]) {
    (s.repos.flatMap(repoRows), s.pending.map(pendingRow))
}

/// A repo with problems gets one row per machine they happened on, so an error
/// with one peer is not hidden behind another. Anything else - syncing, ok,
/// never - stays one row.
func repoRows(_ r: Snapshot.Repo) -> [Row] {
    guard r.state == .error || r.state == .warn, !r.problems.isEmpty else { return [repoRow(r)] }
    var order: [String] = []
    var byMachine: [String: [Snapshot.Problem]] = [:]
    for p in r.problems {
        let m = machine(p)
        if byMachine[m] == nil { order.append(m) }
        byMachine[m, default: []].append(p)
    }
    return order.map { m in
        let ps = byMachine[m]!
        let isError = ps.contains { $0.status == "error" }
        let lines = ps.map(line)
        return Row(
            id: "repo:\(r.repo):\(m)", kind: .repo, name: r.repo,
            symbol: isError ? "✗" : "⚠", state: isError ? "error" : "warn",
            tone: isError ? .error : .warn, when: r.lastSync,
            detail: lines.first ?? "", fullDetail: lines.joined(separator: "\n"), path: r.path,
            retry: retry(r.repo, m, ps)
        )
    }
}

/// What redoes a machine's problems with a repo. A push re-notifies every
/// peer, so they redo their receive and ./activate. This Mac's own receive
/// and ./activate are not redone by a push - each channel clears only on
/// its own success - so they run here.
func retry(_ repo: String, _ machine: String, _ problems: [Snapshot.Problem]) -> [String] {
    guard machine == "this Mac" else { return ["push", repo] }
    return problems.contains { $0.op == "receive" } ? ["receive", repo] : ["activate", repo]
}

/// Where a problem happened: the peer it was sent to, the remote for a push,
/// or this machine for its own receives and ./activate runs.
func machine(_ p: Snapshot.Problem) -> String {
    if let peer = p.peer, !peer.isEmpty { return peer }
    return p.op == "push" ? "remote" : "this Mac"
}

/// "[192.168.1.4] notify: peer 192.168.1.4 receive failed (exit 1)"
func line(_ p: Snapshot.Problem) -> String {
    "[\(machine(p))] \(p.op): \(p.msg)"
}

func repoRow(_ r: Snapshot.Repo) -> Row {
    let problems = r.problems.map(line)
    let (symbol, state, tone): (String, String, Tone) = switch r.state {
    case .syncing: ("↻", r.running.first?.op ?? "syncing", .syncing)
    case .error: ("✗", "error", .error)
    case .warn: ("⚠", "warn", .warn)
    case .ok: ("✓", "ok", .ok)
    case .never: ("–", "never synced", .muted)
    }
    return Row(
        id: "repo:" + r.repo, kind: .repo, name: r.repo, symbol: symbol, state: state, tone: tone,
        when: r.lastSync, detail: problems.first ?? "", fullDetail: problems.joined(separator: "\n"),
        path: r.path, retry: tone == .error || tone == .warn ? ["push", r.repo] : nil
    )
}

func pendingRow(_ p: Snapshot.Pending) -> Row {
    let isPush = p.kind == "push"
    let why = isPush ? "remote unreachable, will retry" : "\(p.peer ?? "?") offline, will retry"
    return Row(
        id: "pending:\(p.kind):\(p.peer ?? ""):\(p.repo)", kind: .pending, name: p.repo,
        symbol: "→", state: "queued", tone: .muted, when: p.since,
        detail: why, fullDetail: why, path: p.path, retry: ["retry"]
    )
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
