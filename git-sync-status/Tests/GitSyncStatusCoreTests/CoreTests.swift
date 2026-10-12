import Foundation
import Testing
@testable import GitSyncStatusCore

/// The shape `git-sync status --json` prints (see internal/status/build.go).
let fixture = """
{"at":"2026-10-09T17:20:00-06:00","syncing":true,"problems":1,"repos":[
 {"repo":"git-sync","path":"/Users/me/c/git-sync","state":"syncing","last_sync":"2026-10-09T17:19:00-06:00",
  "running":[{"op":"push","started":"2026-10-09T17:19:58-06:00"}],"problems":[]},
 {"repo":"top_cpu","path":"/Users/me/c/top_cpu","state":"error","last_sync":"2026-10-09T16:10:00-06:00",
  "running":[],"problems":[{"ts":"2026-10-09T16:20:00-06:00","op":"notify","peer":"192.168.1.1","status":"error","msg":"peer 192.168.1.1 receive failed (exit 1)"}]},
 {"repo":"zsh","path":"/Users/me/c/zsh","state":"never","running":[],"problems":[]}
],"pending":[{"kind":"notify","repo":"agents-configs","path":"/Users/me/c/agents-configs","peer":"192.168.1.3","since":"2026-10-09T16:49:36-06:00"}]}
""".replacingOccurrences(of: "\n", with: "")

func decoded() throws -> Snapshot {
    try Snapshot.decode(Data(fixture.utf8))
}

@Test func decodesASnapshot() throws {
    let s = try decoded()
    #expect(s.syncing)
    #expect(s.problems == 1)
    #expect(s.repos.map(\.repo) == ["git-sync", "top_cpu", "zsh"])
    #expect(s.repos[0].state == .syncing)
    #expect(s.repos[0].running.first?.op == "push")
    #expect(s.repos[1].problems.first?.peer == "192.168.1.1")
    #expect(s.repos[2].lastSync == nil)
    #expect(s.pending.first?.peer == "192.168.1.3")
    #expect(s.error == nil)
}

@Test func decodesTheNotInstalledSnapshot() throws {
    let line = #"{"at":"2026-10-09T17:20:00-06:00","syncing":false,"problems":0,"repos":[],"pending":[],"error":"git-sync is not installed: no config at /x"}"#
    let s = try Snapshot.decode(Data(line.utf8))
    #expect(s.error?.contains("not installed") == true)
}

@Test func mapsReposInOrderAndPendingLast() throws {
    let (repos, pending) = rows(for: try decoded())
    #expect(repos.map(\.name) == ["git-sync", "top_cpu", "zsh"])
    #expect(repos[0].state == "syncing (push)")
    #expect(repos[0].tone == .syncing)
    #expect(repos[1].tone == .error)
    #expect(repos[1].detail == "[192.168.1.1] notify: peer 192.168.1.1 receive failed (exit 1)")
    #expect(repos[2].state == "never synced")
    #expect(repos[2].when == nil)
    #expect(pending.count == 1)
    #expect(pending[0].state == "queued")
    #expect(pending[0].detail == "192.168.1.3 offline, will retry")
    #expect(pending[0].path == "/Users/me/c/agents-configs")
    #expect(pending[0].kind == .pending)
}

@Test func aRepoGetsOneRowPerMachineWithProblems() throws {
    let line = """
    {"at":"2026-10-09T17:20:00-06:00","syncing":false,"problems":1,"repos":[
     {"repo":"currentps","path":"/Users/me/c/currentps","state":"error","last_sync":"2026-10-09T16:10:00-06:00","running":[],"problems":[
      {"ts":"2026-10-09T16:30:00-06:00","op":"receive","status":"error","msg":"stash failed, not touching this repo"},
      {"ts":"2026-10-09T16:25:00-06:00","op":"notify","peer":"192.168.1.4","status":"warn","msg":"peer 192.168.1.4 diverged"},
      {"ts":"2026-10-09T16:20:00-06:00","op":"activate","status":"warn","msg":"./activate exited 1"},
      {"ts":"2026-10-09T16:15:00-06:00","op":"push","status":"error","msg":"push to github failed"}]}
    ],"pending":[]}
    """.replacingOccurrences(of: "\n", with: "")
    let (repos, _) = rows(for: try Snapshot.decode(Data(line.utf8)))
    #expect(repos.map(\.detail) == [
        "[this Mac] receive: stash failed, not touching this repo",
        "[192.168.1.4] notify: peer 192.168.1.4 diverged",
        "[remote] push: push to github failed",
    ])
    #expect(repos.map(\.name) == ["currentps", "currentps", "currentps"])
    #expect(repos.map(\.tone) == [.error, .warn, .error])
    #expect(repos[0].fullDetail == "[this Mac] receive: stash failed, not touching this repo\n[this Mac] activate: ./activate exited 1")
    #expect(Set(repos.map(\.id)).count == 3)
}

@Test func everyRowCopiesItsRepoPath() throws {
    let (repos, pending) = rows(for: try decoded())
    #expect((repos + pending).allSatisfy { $0.path.hasPrefix("/Users/me/c/") })
}

@Test func rowIDsAreUnique() throws {
    let (repos, pending) = rows(for: try decoded())
    let ids = (repos + pending).map(\.id)
    #expect(Set(ids).count == ids.count)
}

@Test func agoReadsLikeAHuman() {
    let now = Date(timeIntervalSince1970: 1_000_000)
    #expect(ago(nil, now: now) == "–")
    #expect(ago(now.addingTimeInterval(-5), now: now) == "now")
    #expect(ago(now.addingTimeInterval(-120), now: now).contains("2 min"))
}

@Test func iconStateFollowsTheSnapshot() throws {
    #expect(IconState(nil) == IconState(syncing: false, problem: false))
    #expect(IconState(try decoded()) == IconState(syncing: true, problem: true))
}
