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
