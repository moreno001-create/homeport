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
        var args = ["curl", "-s", "--max-time", "120", "-X", method, "-H", "Content-Type: application/json",
                    "-w", "\n%{http_code}", "--unix-socket", socket, "http://homeport/api" + path]
        if let body, let data = try? JSONSerialization.data(withJSONObject: body) {
            args += ["--data-binary", String(decoding: data, as: UTF8.self)]
        }
        let r = await Shell.run(args, timeout: 130)
        let lines = r.out.split(separator: "\n", omittingEmptySubsequences: false)
        let code = Int(lines.last ?? "") ?? 0
        if r.ok && (200..<300).contains(code) { return nil }
        let text = lines.dropLast().joined(separator: "\n")
        struct Failure: Decodable { var error: String }
        if let f = try? JSONDecoder().decode(Failure.self, from: Data(text.utf8)) { return f.error }
        return r.ok ? "Homeport answered \(code)" : "Homeport isn't answering"
    }

    struct Domain: Decodable {
        var name: String
        var main: Bool?
        var app: String?
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
        var address: String?
    }
}
