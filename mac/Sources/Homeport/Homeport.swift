import Foundation

// Homeport is what the Mac installer sets up: two launchd agents (Homeport and Caddy) running as this user, the state
// folder, and the local socket the homeport command uses (the app asks the same socket how things are).
enum Homeport {
    static let label = "dev.homeport"
    static let uid = getuid()
    static let state = Shell.home + "/Library/Application Support/Homeport"
    static let socket = state + "/homeport.sock"
    static let logs = state + "/logs"
    static let agents = Shell.home + "/Library/LaunchAgents"
    static let installer = "https://moreno001-create.github.io/homeport/install-mac.sh"
    // The server's agents, in the order they start (Caddy first: Homeport hands it its configuration).
    static let server = ["caddy", "homeport"]

    static func plist(_ name: String) -> String { "\(agents)/\(label).\(name).plist" }
    static func target(_ name: String) -> String { "gui/\(uid)/\(label).\(name)" }
    static var installed: Bool { FileManager.default.fileExists(atPath: plist("homeport")) }
    static var brew: String? {
        ["/opt/homebrew/bin/brew", "/usr/local/bin/brew"].first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    // MARK: Homeport's own API, over its socket (only this user can open it)

    static func get<T: Decodable>(_ path: String, as: T.Type) async -> T? {
        let r = await Shell.run(["curl", "-sf", "--max-time", "8", "--unix-socket", socket, "http://homeport/api" + path])
        guard r.ok else { return nil }
        return try? JSONDecoder().decode(T.self, from: Data(r.out.utf8))
    }

    static func post(_ path: String) async -> Bool {
        await call("POST", path) == nil
    }

    // call sends a request with an optional JSON body; nil means it worked, else it's Homeport's error.
    static func call(_ method: String, _ path: String, body: [String: Any]? = nil) async -> String? {
        await request(method, path, body: body).error
    }

    // request is call that also returns what Homeport answered.
    static func request(_ method: String, _ path: String, body: [String: Any]? = nil) async -> (error: String?, answer: String) {
        var args = ["curl", "-s", "--max-time", "120", "-X", method, "-H", "Content-Type: application/json",
                    "-w", "\n%{http_code}", "--unix-socket", socket, "http://homeport/api" + path]
        if let body, let data = try? JSONSerialization.data(withJSONObject: body) {
            args += ["--data-binary", String(decoding: data, as: UTF8.self)]
        }
        let r = await Shell.run(args, timeout: 130)
        let lines = r.out.split(separator: "\n", omittingEmptySubsequences: false)
        let code = Int(lines.last ?? "") ?? 0
        let text = lines.dropLast().joined(separator: "\n")
        if r.ok && (200..<300).contains(code) { return (nil, text) }
        struct Failure: Decodable { var error: String }
        if let f = try? JSONDecoder().decode(Failure.self, from: Data(text.utf8)) { return (f.error, text) }
        return (r.ok ? "Homeport answered \(code)" : "Homeport isn't answering", text)
    }

    struct Domain: Decodable {
        var name: String
        var main: Bool?
        var app: String?
    }

    // MARK: Releases

    // newestRelease is the tag of the newest full release on GitHub, or with testBuilds the newest of any kind.
    static func newestRelease(testBuilds: Bool) async -> String? {
        struct Release: Decodable { var tag_name: String; var prerelease: Bool; var draft: Bool }
        let r = await Shell.run(["curl", "-sf", "--max-time", "15", "-H", "Accept: application/vnd.github+json",
                                 "https://api.github.com/repos/moreno001-create/homeport/releases?per_page=20"], timeout: 20)
        guard r.ok, let list = try? JSONDecoder().decode([Release].self, from: Data(r.out.utf8)) else { return nil }
        return list.filter { !$0.draft && (testBuilds || !$0.prerelease) }.map(\.tag_name)
            .max { compareVersions($0, $1) < 0 }
    }

    // compareVersions orders tags like v0.7.16 < v0.7.17-rc5 < v0.7.17-rc6 < v0.7.17: -1, 0 or 1.
    static func compareVersions(_ a: String, _ b: String) -> Int {
        func parts(_ v: String) -> (nums: [Int], pre: String?) {
            let s = v.hasPrefix("v") ? String(v.dropFirst()) : v
            let main = s.split(separator: "-", maxSplits: 1)
            return (main[0].split(separator: ".").map { Int($0) ?? 0 }, main.count > 1 ? String(main[1]) : nil)
        }
        let (x, y) = (parts(a), parts(b))
        for i in 0..<max(x.nums.count, y.nums.count) {
            let p = i < x.nums.count ? x.nums[i] : 0, q = i < y.nums.count ? y.nums[i] : 0
            if p != q { return p < q ? -1 : 1 }
        }
        switch (x.pre, y.pre) {
        case (nil, nil): return 0
        case (nil, _): return 1   // a full release comes after its test builds
        case (_, nil): return -1
        case let (p?, q?):
            // rc5 < rc6 < rc10: compare the number at the end when the rest matches.
            let pn = Int(p.drop { !$0.isNumber }) ?? 0, qn = Int(q.drop { !$0.isNumber }) ?? 0
            if p.prefix(while: { !$0.isNumber }) == q.prefix(while: { !$0.isNumber }) && pn != qn { return pn < qn ? -1 : 1 }
            return p == q ? 0 : (p < q ? -1 : 1)
        }
    }

    // MARK: launchd

    struct Agent: Equatable {
        var loaded = false
        var running = false
        var pid: Int?
        var lastExit: String?
    }

    static func agent(_ name: String) async -> Agent {
        let r = await Shell.run(["launchctl", "print", target(name)])
        guard r.ok else { return Agent() }
        var a = Agent(loaded: true)
        // The service's own lines are indented once; deeper ones belong to its sockets and such.
        for line in r.out.split(separator: "\n") where line.hasPrefix("\t") && !line.hasPrefix("\t\t") {
            let l = line.trimmingCharacters(in: .whitespaces)
            if l.hasPrefix("state = ") { a.running = l == "state = running" }
            if l.hasPrefix("pid = ") { a.pid = Int(l.dropFirst(6)) }
            if l.hasPrefix("last exit code = ") { a.lastExit = String(l.dropFirst(17)) }
        }
        return a
    }

    // disabled says whether the server was stopped here (launchd then won't start it at login either).
    static func disabled() async -> Bool {
        let r = await Shell.run(["launchctl", "print-disabled", "gui/\(uid)"])
        return r.out.split(separator: "\n").contains { line in
            line.contains("\"\(label).homeport\"") && (line.contains("disabled") || line.contains("true"))
        }
    }

    // start, stop and restart the server's agents. Stopping disables them, so the server stays stopped after a restart
    // of the Mac until it's started again here.
    static func start() async -> String? {
        for name in server where FileManager.default.fileExists(atPath: plist(name)) {
            _ = await Shell.run(["launchctl", "enable", target(name)])
            if await agent(name).loaded { continue }
            let r = await Shell.run(["launchctl", "bootstrap", "gui/\(uid)", plist(name)], timeout: 30)
            if !r.ok { return "Couldn't start \(name): \(r.out.trimmingCharacters(in: .whitespacesAndNewlines))" }
        }
        return nil
    }

    static func stop() async -> String? {
        for name in server.reversed() {
            _ = await Shell.run(["launchctl", "disable", target(name)])
            guard await agent(name).loaded else { continue }
            let r = await Shell.run(["launchctl", "bootout", target(name)], timeout: 90)
            if !r.ok, await agent(name).loaded {
                return "Couldn't stop \(name): \(r.out.trimmingCharacters(in: .whitespacesAndNewlines))"
            }
        }
        return nil
    }

    static func restart() async -> String? {
        for name in server {
            let r = await Shell.run(["launchctl", "kickstart", "-k", target(name)], timeout: 90)
            if !r.ok { return "Couldn't restart \(name): \(r.out.trimmingCharacters(in: .whitespacesAndNewlines))" }
        }
        return nil
    }

    // MARK: What Homeport reports

    struct Overview: Decodable {
        var version: String?
        var address: String?
        var settings: Settings?
        var working: [String]?

        struct Settings: Decodable {
            var mainDomain: String?
            var tls: String?
            var chat: Chat?
        }

        struct Chat: Decodable {
            var localURL: String?
            var localModel: String?
        }
    }

    struct App: Decodable, Identifiable, Equatable {
        var id: String
        var name: String
        var status: String
        var domain: String?
        var address: String?
    }

    struct Job: Decodable {
        var id: String
        var status: String // running, done, failed
        var error: String?
    }

    // MARK: Names on the home network

    // validLocal says whether label works as NAME in NAME.local.
    static func validLocal(_ label: String) -> Bool {
        label.range(of: "^[a-z][a-z0-9-]{0,40}$", options: .regularExpression) != nil
    }

    // localOwner is the address of another device on this network that answers to name (NAME.local), nil when it's
    // free or it's this Mac. Two Homeports on one network need different names.
    static func localOwner(_ name: String) async -> String? {
        async let answers = Shell.run(["dns-sd", "-G", "v4", name], timeout: 2)
        async let ifconfig = Shell.run(["ifconfig"], timeout: 5)
        let mine = Set((await ifconfig).out.split(separator: "\n").compactMap { line -> String? in
            let f = line.split(whereSeparator: { $0 == " " || $0 == "\t" })
            return f.first == "inet" && f.count > 1 ? String(f[1]) : nil
        })
        for line in (await answers).out.split(separator: "\n") {
            let f = line.split(separator: " ", omittingEmptySubsequences: true)
            if f.count >= 6, f[1] == "Add", !mine.contains(String(f[5])) { return String(f[5]) }
        }
        return nil
    }

    // suggestedLocal is homeport, or this Mac's own name when another machine has homeport.local.
    static func suggestedLocal() async -> String {
        if await localOwner("homeport.local") == nil { return "homeport" }
        let host = await Shell.run(["scutil", "--get", "LocalHostName"]).out.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        let clean = String(host.map { $0.isLetter || $0.isNumber || $0 == "-" ? $0 : "-" })
        return validLocal(clean) ? clean : "homeport-2"
    }
}

// MacSettings are this Mac's settings that matter for a server: it mustn't sleep, should start again after a power
// cut, and Docker must start at login (else the apps don't run after a restart).
struct MacSettings: Equatable {
    var sleepMinutes: Int?    // 0 = never
    var autorestart: Bool?    // nil: this Mac doesn't have the setting
    var dockerAtLogin: Bool?  // nil: not OrbStack, or not known

    static func read() async -> MacSettings {
        var m = MacSettings()
        let pm = await Shell.run(["pmset", "-g"], timeout: 5).out
        for line in pm.split(separator: "\n") {
            let f = line.split(whereSeparator: { $0 == " " || $0 == "\t" })
            guard f.count >= 2, let v = Int(f[1]) else { continue }
            if f[0] == "sleep" { m.sleepMinutes = v }
            if f[0] == "autorestart" { m.autorestart = v == 1 }
        }
        if FileManager.default.fileExists(atPath: "/Applications/OrbStack.app") {
            let orb = await Shell.run(["orb", "config", "show"], timeout: 5)
            if orb.ok, let line = orb.out.split(separator: "\n").first(where: { $0.hasPrefix("app.start_at_login:") }) {
                m.dockerAtLogin = line.hasSuffix("true")
            }
        }
        return m
    }
}
