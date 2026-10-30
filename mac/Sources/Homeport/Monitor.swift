import AppKit
import Foundation

// Monitor checks Homeport, Caddy, Docker, the local AI and the apps every few seconds, and runs what the buttons ask.
@MainActor
final class Monitor: ObservableObject {
    @Published var installed = Homeport.installed
    @Published var homeport = Homeport.Agent()
    @Published var caddy = Homeport.Agent()
    @Published var stoppedHere = false
    @Published var overview: Homeport.Overview?
    @Published var apps: [Homeport.App] = []
    @Published var docker: String?       // "OrbStack · Docker 29.8.2", nil when it doesn't answer
    @Published var ai: Bool?             // the local model server answers (nil: none set up)
    @Published var checked: Date?
    @Published var busy: String?         // what a button is doing
    @Published var problem: String?
    @Published var appBusy: Set<String> = []

    // The installer's output, shown in a sheet while it runs.
    @Published var log: [String] = []
    @Published var logTitle = ""
    @Published var logOpen = false
    @Published var logDone = false
    @Published var logOK = false

    // Apps the server's Stop button stopped, so Start brings them back.
    private let stoppedAppsKey = "stoppedApps"

    var running: Bool { homeport.running && caddy.running }
    var address: String? { overview?.address ?? overview?.settings?.mainDomain }
    var version: String? { overview?.version }

    func watch() async {
        while !Task.isCancelled {
            await refresh()
            try? await Task.sleep(nanoseconds: 5_000_000_000)
        }
    }

    func refresh() async {
        installed = Homeport.installed
        guard installed else { return }
        async let h = Homeport.agent("homeport")
        async let c = Homeport.agent("caddy")
        async let off = Homeport.disabled()
        async let d = dockerStatus()
        homeport = await h
        caddy = await c
        stoppedHere = await off
        docker = await d
        if homeport.running {
            async let o = Homeport.get("/overview", as: Homeport.Overview.self)
            async let a = Homeport.get("/apps", as: [Homeport.App].self)
            if let o = await o { overview = o }
            if let a = await a { apps = a.sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending } }
        } else {
            apps = []
        }
        ai = await aiStatus()
        checked = Date()
    }

    private func dockerStatus() async -> String? {
        let r = await Shell.run(["docker", "info", "--format", "{{.ServerVersion}}"], timeout: 8)
        guard r.ok else { return nil }
        let ctx = await Shell.run(["docker", "context", "show"], timeout: 5).out.trimmingCharacters(in: .whitespacesAndNewlines)
        let names = ["orbstack": "OrbStack", "colima": "Colima", "desktop-linux": "Docker Desktop"]
        let runtime = names[ctx] ?? (ctx.isEmpty ? "Docker" : ctx)
        return "\(runtime) · Docker \(r.out.trimmingCharacters(in: .whitespacesAndNewlines))"
    }

    private func aiStatus() async -> Bool? {
        guard let url = overview?.settings?.chat?.localURL, !url.isEmpty else { return nil }
        let base = url.hasSuffix("/") ? String(url.dropLast()) : url
        return await Shell.run(["curl", "-sf", "--max-time", "3", "-o", "/dev/null", base + "/models"], timeout: 5).ok
    }

    // MARK: The server

    func start() {
        act("Starting the server…") {
            if let err = await Homeport.start() { return err }
            // Bring back the apps Stop stopped, once Homeport answers.
            let ids = UserDefaults.standard.stringArray(forKey: self.stoppedAppsKey) ?? []
            if !ids.isEmpty, await self.waitForHomeport() {
                for id in ids { _ = await Homeport.post("/apps/\(id)/start") }
                UserDefaults.standard.removeObject(forKey: self.stoppedAppsKey)
            }
            return nil
        }
    }

    func stop(withApps: Bool) {
        act(withApps ? "Stopping the apps and the server…" : "Stopping the server…") {
            if withApps, self.homeport.running {
                let going = self.apps.filter { $0.status == "running" }.map(\.id)
                for id in going { _ = await Homeport.post("/apps/\(id)/stop") }
                UserDefaults.standard.set(going, forKey: self.stoppedAppsKey)
            }
            return await Homeport.stop()
        }
    }

    func restart() {
        act("Restarting the server…") { await Homeport.restart() }
    }

    func startDocker() {
        act("Starting Docker…") {
            if FileManager.default.fileExists(atPath: "/Applications/OrbStack.app") {
                _ = await Shell.run(["open", "-ga", "OrbStack"])
            } else if FileManager.default.fileExists(atPath: "/Applications/Docker.app") {
                _ = await Shell.run(["open", "-ga", "Docker"])
            } else {
                let r = await Shell.run(["colima", "start"], timeout: 300)
                if !r.ok { return "Couldn't start Colima: " + r.out.trimmingCharacters(in: .whitespacesAndNewlines) }
            }
            for _ in 0..<30 {
                if await self.dockerStatus() != nil { return nil }
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
            return "Docker didn't answer after a minute"
        }
    }

    func app(_ app: Homeport.App, start: Bool) {
        appBusy.insert(app.id)
        Task {
            let ok = await Homeport.post("/apps/\(app.id)/\(start ? "start" : "stop")")
            if !ok { problem = "Couldn't \(start ? "start" : "stop") \(app.name)" }
            await refresh()
            appBusy.remove(app.id)
        }
    }

    private func waitForHomeport() async -> Bool {
        for _ in 0..<30 {
            if await Homeport.get("/overview", as: Homeport.Overview.self) != nil { return true }
            try? await Task.sleep(nanoseconds: 1_000_000_000)
        }
        return false
    }

    private func act(_ what: String, _ work: @escaping () async -> String?) {
        guard busy == nil else { return }
        busy = what
        problem = nil
        Task {
            problem = await work()
            await refresh()
            busy = nil
        }
    }

    // MARK: Install and update (the Mac installer, run unattended with its answers in the environment)

    func runInstaller(title: String, env: [String: String]) {
        log = []
        logTitle = title
        logDone = false
        logOK = false
        logOpen = true
        Task {
            let script = NSTemporaryDirectory() + "homeport-install-mac.sh"
            let get = await Shell.run(["curl", "-fsSL", Homeport.installer, "-o", script], timeout: 60)
            guard get.ok else {
                log.append("Couldn't download the installer: " + get.out)
                logDone = true
                return
            }
            let status = await Shell.stream(["bash", script], env: env) { line in
                self.log.append(line)
            }
            try? FileManager.default.removeItem(atPath: script)
            logOK = status == 0
            log.append(logOK ? "" : "The installer stopped (exit code \(status)). The lines above say why.")
            logDone = true
            await refresh()
        }
    }

    func update() {
        runInstaller(title: "Updating Homeport", env: [:])
    }

    // MARK: Make it local

    var isLocal: Bool { (overview?.settings?.mainDomain ?? "").hasSuffix(".local") }

    // makeLocal moves the portal and admin panel to homeport.local with Homeport's own certificates, then takes the old
    // internet name and its app addresses out of Homeport (which also stops updating it at DuckDNS or deSEC).
    func makeLocal() {
        guard let old = overview?.settings?.mainDomain else { return }
        act("Moving Homeport to homeport.local…") {
            if let err = await Homeport.call("PUT", "/settings", body: ["mainDomain": "homeport.local", "tls": "internal"]) {
                return err
            }
            var inUse: [String] = []
            for d in await Homeport.get("/domains", as: [Homeport.Domain].self) ?? []
            where d.name == old || d.name.hasSuffix("." + old) {
                if let app = d.app {
                    inUse.append("\(app) (\(d.name))")
                } else if let err = await Homeport.call("DELETE", "/domains/\(d.name)") {
                    return "Couldn't remove \(d.name): \(err)"
                }
            }
            if !inUse.isEmpty {
                return "Homeport is local now, but these apps still use \(old): \(inUse.joined(separator: ", ")). Change their address in the admin panel, then press Make it local again."
            }
            return nil
        }
    }

    // MARK: Uninstall

    // uninstall removes the apps' containers, the launchd agents and the homeport program, optionally Caddy and restic,
    // and keeps the data as a dated copy beside it (keepCopy) or moves it to the Trash.
    func uninstall(keepCopy: Bool, removeTools: Bool) {
        log = []
        logTitle = "Uninstalling Homeport"
        logDone = false
        logOK = false
        logOpen = true
        busy = "Uninstalling…"
        Task {
            let fm = FileManager.default
            let say = self.say

            say("==> Removing the apps' containers")
            struct Project: Decodable { var Name: String; var ConfigFiles: String }
            let ls = await Shell.run(["docker", "compose", "ls", "--all", "--format", "json"], timeout: 20)
            if !ls.ok {
                say("Docker isn't running, so the apps' containers stay. Start Docker and delete the hp-… containers in it.")
            } else {
                let projects = (try? JSONDecoder().decode([Project].self, from: Data(ls.out.utf8))) ?? []
                let ours = projects.filter { $0.Name.hasPrefix("hp-") && $0.ConfigFiles.contains(Homeport.state) }
                if ours.isEmpty { say("No app containers.") }
                for p in ours {
                    let r = await Shell.run(["docker", "compose", "-p", p.Name, "down", "--remove-orphans"], timeout: 180)
                    say(r.ok ? "Removed \(p.Name)." : "Couldn't remove \(p.Name): \(r.out.trimmingCharacters(in: .whitespacesAndNewlines))")
                }
            }

            say("==> Stopping and removing the background services")
            for name in ["homeport", "caddy", "ollama"] where fm.fileExists(atPath: Homeport.plist(name)) {
                _ = await Shell.run(["launchctl", "bootout", Homeport.target(name)], timeout: 90)
                _ = await Shell.run(["launchctl", "enable", Homeport.target(name)]) // clears a Stop's "disabled"
                try? fm.removeItem(atPath: Homeport.plist(name))
                say("Removed \(Homeport.label).\(name).")
            }

            say("==> Removing the homeport program")
            for dir in ["/opt/homebrew/bin", "/usr/local/bin"] where fm.fileExists(atPath: dir + "/homeport") {
                do {
                    try fm.removeItem(atPath: dir + "/homeport")
                    say("Removed \(dir)/homeport.")
                } catch {
                    say("Couldn't remove \(dir)/homeport: \(error.localizedDescription)")
                }
            }

            if removeTools, let brew = Homeport.brew {
                say("==> Removing Caddy and restic (Homebrew)")
                for tool in ["caddy", "restic"] {
                    let r = await Shell.run([brew, "uninstall", tool], timeout: 180)
                    say(r.ok ? "Removed \(tool)." : "\(tool): \(r.out.trimmingCharacters(in: .whitespacesAndNewlines))")
                }
                let caddyData = Shell.home + "/Library/Application Support/Caddy"
                if fm.fileExists(atPath: caddyData), (try? fm.trashItem(at: URL(fileURLWithPath: caddyData), resultingItemURL: nil)) != nil {
                    say("Moved Caddy's certificates to the Trash.")
                }
            }

            say("==> Homeport's data (users, settings, certificates and every app's folder)")
            if fm.fileExists(atPath: Homeport.state) {
                do {
                    if keepCopy {
                        let f = DateFormatter()
                        f.dateFormat = "yyyyMMdd-HHmmss"
                        let aside = Homeport.state + "-removed-" + f.string(from: Date())
                        try fm.moveItem(atPath: Homeport.state, toPath: aside)
                        say("Kept as \(aside).")
                    } else {
                        try fm.trashItem(at: URL(fileURLWithPath: Homeport.state), resultingItemURL: nil)
                        say("Moved to the Trash (it's gone for good once the Trash is emptied).")
                    }
                } catch {
                    say("Couldn't move it: \(error.localizedDescription)")
                }
            }

            say("")
            say("Homeport is removed. Left as they were: your Docker runtime, LM Studio, Ollama and its models, and the name at DuckDNS or deSEC. Drag this app to the Trash to remove it too.")
            logOK = true
            logDone = true
            overview = nil
            apps = []
            busy = nil
            await refresh()
        }
    }

    private func say(_ line: String) { log.append(line) }

    // MARK: Opening things

    func open(_ path: String) {
        guard let a = address, let url = URL(string: "https://\(a)\(path)") else { return }
        NSWorkspace.shared.open(url)
    }

    func openLogs() {
        let log = Homeport.logs + "/homeport.log"
        if FileManager.default.fileExists(atPath: log) {
            NSWorkspace.shared.open([URL(fileURLWithPath: log)], withApplicationAt: URL(fileURLWithPath: "/System/Applications/Utilities/Console.app"),
                                    configuration: NSWorkspace.OpenConfiguration())
        } else {
            NSWorkspace.shared.open(URL(fileURLWithPath: Homeport.state))
        }
    }
}
