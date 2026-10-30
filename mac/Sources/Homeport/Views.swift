import SwiftUI

@main
struct HomeportApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate
    @StateObject private var monitor = Monitor()

    var body: some Scene {
        Window("Homeport", id: "main") {
            ContentView()
                .environmentObject(monitor)
                .frame(minWidth: 520, minHeight: 560)
        }
        .defaultSize(width: 600, height: 700)
        .commands {
            CommandGroup(replacing: .newItem) {}
            CommandGroup(after: .toolbar) {
                Button("Check Now") { Task { await monitor.refresh() } }.keyboardShortcut("r")
                ConsoleCommand()
            }
        }

        Window("Homeport Console", id: "console") {
            ConsoleView().environmentObject(monitor)
        }
        .defaultSize(width: 860, height: 520)
    }
}

struct ConsoleCommand: View {
    @Environment(\.openWindow) private var openWindow
    var body: some View {
        Button("Show Console") { openWindow(id: "console") }.keyboardShortcut("l")
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }

    // HOMEPORT_SNAPSHOT=file.png: the window draws itself into that file after a few seconds (for checking the app
    // on another Mac over SSH, where screenshots aren't allowed).
    func applicationDidFinishLaunching(_ notification: Notification) {
        guard let out = ProcessInfo.processInfo.environment["HOMEPORT_SNAPSHOT"] else { return }
        DispatchQueue.main.asyncAfter(deadline: .now() + 8) {
            // One file per window: file.png, then file-2.png… (HOMEPORT_CONSOLE=Caddy, say, opens the console first).
            for (i, window) in NSApp.windows.filter({ $0.isVisible && $0.contentView != nil }).enumerated() {
                guard let view = window.contentView, let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { continue }
                view.cacheDisplay(in: view.bounds, to: rep)
                let path = i == 0 ? out : out.replacingOccurrences(of: ".png", with: "-\(i + 1).png")
                try? rep.representation(using: .png, properties: [:])?.write(to: URL(fileURLWithPath: path))
            }
        }
    }
}

struct ContentView: View {
    @EnvironmentObject var m: Monitor
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Group {
            if m.installed { StatusView() } else { InstallView() }
        }
        .safeAreaInset(edge: .top, spacing: 0) { JobStrip() }
        .task {
            if let tab = ProcessInfo.processInfo.environment["HOMEPORT_CONSOLE"].flatMap(ConsoleTab.init(rawValue:)) { m.showConsole(tab) }
            await m.watch()
        }
        .onChange(of: m.consoleRequests) { _ in openWindow(id: "console") }
    }
}

// JobStrip shows an install, update or uninstall going on (and how it ended), with the way to the console.
struct JobStrip: View {
    @EnvironmentObject var m: Monitor

    var body: some View {
        if m.job != nil || m.jobResult != nil {
            HStack(spacing: 8) {
                if let job = m.job {
                    ProgressView().controlSize(.small)
                    Text(job + "…")
                    Text(m.activity.last(where: { !$0.isEmpty }) ?? "").foregroundStyle(.secondary).lineLimit(1).truncationMode(.tail)
                } else if let ok = m.jobResult {
                    Image(systemName: ok ? "checkmark.circle.fill" : "xmark.octagon.fill").foregroundStyle(ok ? .green : .red)
                    Text(ok ? "Finished" : "It didn't finish: the console says why")
                }
                Spacer()
                Button("Console") { m.showConsole(.activity) }.controlSize(.small)
                if m.job == nil {
                    Button { m.jobResult = nil } label: { Image(systemName: "xmark") }
                        .buttonStyle(.borderless).help("Hide")
                }
            }
            .font(.callout)
            .padding(.horizontal, 14)
            .padding(.vertical, 8)
            .background(.bar)
            .overlay(alignment: .bottom) { Divider() }
        }
    }
}

// MARK: Status

struct Dot: View {
    enum Kind { case good, bad, off, busy }
    let kind: Kind

    var body: some View {
        if kind == .busy {
            ProgressView().controlSize(.small).frame(width: 12, height: 12)
        } else {
            Circle()
                .fill(kind == .good ? Color.green : kind == .bad ? Color.red : Color.secondary.opacity(0.4))
                .frame(width: 10, height: 10)
                .frame(width: 12, height: 12)
        }
    }
}

struct Row<Trailing: View>: View {
    let dot: Dot.Kind
    let title: String
    let detail: String
    @ViewBuilder var trailing: Trailing

    var body: some View {
        HStack(spacing: 10) {
            Dot(kind: dot)
            VStack(alignment: .leading, spacing: 2) {
                Text(title)
                Text(detail).font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
            }
            Spacer()
            trailing
        }
        .padding(.vertical, 2)
    }
}

struct StatusView: View {
    @EnvironmentObject var m: Monitor
    @AppStorage("stopAppsToo") private var stopAppsToo = false
    @State private var renaming = false
    @State private var uninstalling = false

    var body: some View {
        Form {
            Section { header }
            if let p = m.problem {
                Section {
                    Label(p, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                }
            }
            Section("Services") { services }
            Section(m.apps.isEmpty ? "Apps" : "Apps (\(m.apps.count))") { appRows }
            Section("This Mac") { macRows }
            Section { footer }
        }
        .formStyle(.grouped)
        .sheet(isPresented: $uninstalling) { UninstallView() }
        .sheet(isPresented: $renaming) { LocalNameSheet() }
    }

    private var state: (String, Dot.Kind) {
        if m.busy != nil { return (m.busy!, .busy) }
        if m.running { return ("Running", .good) }
        if m.stoppedHere { return ("Stopped", .off) }
        if !m.homeport.loaded && !m.caddy.loaded { return ("Stopped", .off) }
        return ("Not working: see below", .bad)
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(alignment: .center, spacing: 12) {
                Image(systemName: "house.fill")
                    .font(.system(size: 26))
                    .foregroundStyle(.white)
                    .frame(width: 48, height: 48)
                    .background(RoundedRectangle(cornerRadius: 11).fill(Color.accentColor.gradient))
                VStack(alignment: .leading, spacing: 3) {
                    Text("Homeport").font(.title2.bold())
                    HStack(spacing: 6) {
                        Dot(kind: state.1)
                        Text(state.0).foregroundStyle(.secondary)
                        if let v = m.version { Text("· \(v)").foregroundStyle(.secondary) }
                    }
                    .font(.callout)
                }
                Spacer()
            }
            if let a = m.address {
                HStack {
                    Text(a).font(.callout).textSelection(.enabled).foregroundStyle(.secondary)
                    Spacer()
                    if m.running {
                        Button(m.isLocal ? "Change name…" : "Make it local…") { renaming = true }
                            .controlSize(.small)
                            .disabled(m.busy != nil || m.job != nil)
                    }
                }
            }
            HStack {
                if m.running {
                    Button("Stop", systemImage: "stop.fill") { m.stop(withApps: stopAppsToo) }
                    Button("Restart", systemImage: "arrow.clockwise") { m.restart() }
                } else {
                    Button("Start", systemImage: "play.fill") { m.start() }.keyboardShortcut(.defaultAction)
                }
                Spacer()
                Button("Portal") { m.open("/") }.disabled(!m.running || m.address == nil)
                Button("Admin") { m.open("/admin") }.disabled(!m.running || m.address == nil)
            }
            .disabled(m.busy != nil)
        }
        .padding(.vertical, 4)
    }

    private func agentDetail(_ a: Homeport.Agent, _ what: String) -> String {
        if a.running { return what + (a.pid.map { " · process \($0)" } ?? "") }
        if m.stoppedHere { return "Stopped here (won't start at login until you press Start)" }
        if !a.loaded { return "Not running" }
        if let e = a.lastExit, e != "0", !e.hasPrefix("(never") { return "Keeps stopping (last exit code \(e)): see the logs" }
        return "Starting…"
    }

    private func agentDot(_ a: Homeport.Agent) -> Dot.Kind {
        a.running ? .good : (m.stoppedHere || !a.loaded) ? .off : .bad
    }

    @ViewBuilder private var services: some View {
        Row(dot: agentDot(m.homeport), title: "Homeport", detail: agentDetail(m.homeport, "Admin panel, portal and sign-in")) {}
        Row(dot: agentDot(m.caddy), title: "Caddy", detail: agentDetail(m.caddy, "Web server on ports 80 and 443, with the certificates")) {}
        Row(dot: m.docker == nil ? .bad : .good, title: "Docker", detail: m.docker ?? "Not running: apps can't run without it") {
            if m.docker == nil { Button("Start Docker") { m.startDocker() }.disabled(m.busy != nil) }
        }
        if let ai = m.ai {
            let chat = m.overview?.settings?.chat
            let model = chat?.localModel ?? "the local model"
            let host = URL(string: chat?.localURL ?? "")?.host ?? ""
            Row(dot: ai ? .good : .bad, title: "Local AI", detail: ai ? "\(model) · \(host)" : "\(model) isn't answering at \(host): open LM Studio or Ollama") {}
        }
    }

    @ViewBuilder private var appRows: some View {
        if !m.homeport.running {
            Text("Start the server to see the apps.").foregroundStyle(.secondary)
        } else if m.apps.isEmpty {
            Text("No apps yet: add them from the admin panel's Apps page.").foregroundStyle(.secondary)
        }
        ForEach(m.apps) { app in
            let running = app.status == "running"
            let busy = m.appBusy.contains(app.id)
            Row(dot: busy ? .busy : running ? .good : app.status == "stopped" ? .off : .bad,
                title: app.name, detail: [app.status.capitalized, app.address].compactMap { $0 }.joined(separator: " · ")) {
                if let a = app.address, running, let url = URL(string: "https://\(a)") {
                    Link(destination: url) { Image(systemName: "arrow.up.right.square") }.help("Open \(app.name)")
                }
                Button(running ? "Stop" : "Start") { m.app(app, start: !running) }.disabled(busy)
            }
        }
    }

    @ViewBuilder private var macRows: some View {
        let mac = m.mac
        if let mins = mac.sleepMinutes {
            Row(dot: mins == 0 ? .good : .bad, title: "Sleep",
                detail: mins == 0 ? "Never sleeps, so the sites stay up" : "Sleeps after \(mins) minute\(mins == 1 ? "" : "s") idle: the sites go down while it sleeps") {
                if mins != 0 { Button("Keep awake…") { m.keepAwake() }.disabled(m.busy != nil) }
            }
        }
        if let auto = mac.autorestart {
            Row(dot: auto ? .good : .bad, title: "After a power cut",
                detail: auto ? "Starts again by itself" : "Stays off until someone presses the power button") {
                if !auto && mac.sleepMinutes == 0 { Button("Fix…") { m.keepAwake() }.disabled(m.busy != nil) }
            }
        }
        if let login = mac.dockerAtLogin {
            Row(dot: login ? .good : .bad, title: "OrbStack at login",
                detail: login ? "Docker starts when you log in, so the apps come back after a restart" : "Docker doesn't start by itself: after a restart the apps stay down") {
                if !login { Button("Fix") { m.dockerAtLogin() }.disabled(m.busy != nil) }
            }
        }
        Text("Homeport runs while you're logged in: turn on automatic login (System Settings → Users & Groups) so it comes back after a restart.")
            .font(.caption).foregroundStyle(.secondary)
    }

    private var footer: some View {
        VStack(alignment: .leading, spacing: 10) {
            Toggle("Stop the apps too when stopping the server", isOn: $stopAppsToo)
            HStack {
                Button("Console") { m.showConsole() }
                Button("Update Homeport…") { m.update() }.disabled(m.busy != nil || m.job != nil)
                Button("Uninstall…", role: .destructive) { uninstalling = true }.disabled(m.busy != nil || m.job != nil)
                Spacer()
                if let c = m.checked {
                    Text("Checked \(c.formatted(date: .omitted, time: .standard))").font(.caption).foregroundStyle(.secondary)
                }
            }
        }
    }
}

// MARK: Install

struct InstallView: View {
    @EnvironmentObject var m: Monitor

    enum Reach: String, CaseIterable, Identifiable {
        case local, duckdns, desec, own
        var id: String { rawValue }
        var label: String {
            switch self {
            case .local: return "A name on this network (homeport.local, or your own choice)"
            case .duckdns: return "A free DuckDNS name (yourname.duckdns.org)"
            case .desec: return "A free deSEC name (yourname.dedyn.io)"
            case .own: return "A domain of my own"
            }
        }
    }

    @State private var reach = Reach.local
    @State private var name = ""
    @State private var token = ""
    @State private var domain = ""
    @State private var email = ""
    @State private var admin = ""
    @State private var password = ""
    @State private var again = ""
    @State private var localAI = true
    @State private var localLabel = ""
    @State private var localOwner: String?

    private var problem: String? {
        if Homeport.brew == nil { return "Install Homebrew first (it installs Caddy)." }
        if reach == .local && !Homeport.validLocal(localLabel) { return "Choose a name on this network." }
        if reach == .local && localOwner != nil { return "\(localLabel).local is taken on this network." }
        if (reach == .duckdns || reach == .desec) && (name.isEmpty || token.isEmpty) { return "Fill in the name and its token." }
        if reach == .own && !domain.contains(".") { return "Fill in your domain." }
        if admin.range(of: "^[a-z][a-z0-9._-]*$", options: .regularExpression) == nil { return "The admin's name: lowercase letters and digits, starting with a letter." }
        if password.count < 10 { return "The password needs at least 10 characters." }
        if password != again { return "The two passwords don't match." }
        return nil
    }

    var body: some View {
        Form {
            Section {
                VStack(alignment: .leading, spacing: 6) {
                    Text("Set up Homeport on this Mac").font(.title2.bold())
                    Text("It installs Caddy, uses a Docker runtime (OrbStack, Docker Desktop, or Colima if neither is there) and runs Homeport as you, starting when you log in. You can add apps from the admin panel afterwards.")
                        .foregroundStyle(.secondary)
                }
                .padding(.vertical, 4)
            }
            if Homeport.brew == nil {
                Section("Homebrew") {
                    Text("Homeport uses Homebrew to install Caddy. Open brew.sh, copy the install command into Terminal, then come back here.")
                    Link("Open brew.sh", destination: URL(string: "https://brew.sh")!)
                }
            }
            Section("How people reach this Mac") {
                Picker("Address", selection: $reach) {
                    ForEach(Reach.allCases) { Text($0.label).tag($0) }
                }
                .pickerStyle(.radioGroup)
                .labelsHidden()
                switch reach {
                case .local:
                    LocalNameField(label: $localLabel, owner: $localOwner)
                    Text("Browsers warn about its certificate once. Each Homeport on this network needs its own name.")
                        .font(.caption).foregroundStyle(.secondary)
                case .duckdns, .desec:
                    TextField("Name", text: $name, prompt: Text(reach == .duckdns ? "yourname (for yourname.duckdns.org)" : "yourname (for yourname.dedyn.io)"))
                    SecureField("Token", text: $token)
                    Link(reach == .duckdns ? "Get a name and token at duckdns.org" : "Get a token at desec.io",
                         destination: URL(string: reach == .duckdns ? "https://www.duckdns.org" : "https://desec.io")!)
                        .font(.caption)
                case .own:
                    TextField("Domain", text: $domain, prompt: Text("home.example.com"))
                    Text("Its DNS must point at this home's public address, with ports 80 and 443 forwarded to this Mac.")
                        .font(.caption).foregroundStyle(.secondary)
                }
                if reach != .local {
                    TextField("Email (optional)", text: $email, prompt: Text("For Let's Encrypt notices about certificates"))
                }
            }
            Section("First admin") {
                TextField("User name", text: $admin)
                SecureField("Password", text: $password)
                SecureField("Again", text: $again)
            }
            Section {
                Toggle("Set up a local AI model for the assistant (LM Studio's, or Ollama)", isOn: $localAI)
            }
            Section {
                HStack {
                    if let p = problem { Text(p).font(.callout).foregroundStyle(.secondary) }
                    Spacer()
                    Button("Console") { m.showConsole(.activity) }
                    Button(m.job == nil ? "Install" : "Installing…") { install() }
                        .keyboardShortcut(.defaultAction)
                        .disabled(problem != nil || m.job != nil)
                }
            }
        }
        .formStyle(.grouped)
    }

    private func install() {
        var env = ["HOMEPORT_ADMIN": admin, "HOMEPORT_PASSWORD": password]
        switch reach {
        case .local: env["HOMEPORT_DOMAIN"] = localLabel + ".local"
        case .duckdns, .desec:
            env["HOMEPORT_DDNS"] = reach.rawValue
            env["HOMEPORT_DDNS_NAME"] = name
            env["HOMEPORT_DDNS_TOKEN"] = token
        case .own: env["HOMEPORT_DOMAIN"] = domain
        }
        if !email.isEmpty { env["HOMEPORT_EMAIL"] = email }
        if !localAI { env["HOMEPORT_LOCAL_AI"] = "no" }
        m.runInstaller(title: "Installing Homeport", env: env)
        password = ""
        again = ""
        token = ""
    }
}

// MARK: The name on the home network

// LocalNameField is NAME.local, checked against the network as it's typed (another machine answering to it means
// it's taken). It starts with homeport, or this Mac's own name when homeport.local is taken.
struct LocalNameField: View {
    @Binding var label: String
    @Binding var owner: String?
    var current: String? = nil // the name Homeport has now (it's this Mac's, so not "taken")
    @State private var checking = false
    @State private var suggested = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 4) {
                TextField("Name", text: $label, prompt: Text("homeport"))
                    .onChange(of: label) { v in
                        let clean = v.lowercased().replacingOccurrences(of: ".local", with: "")
                        if clean != v { label = clean }
                    }
                Text(".local").foregroundStyle(.secondary)
            }
            HStack(spacing: 6) {
                if label.isEmpty {
                    EmptyView()
                } else if !Homeport.validLocal(label) {
                    Text("Letters, digits and dashes, starting with a letter.").foregroundStyle(.red)
                } else if checking {
                    ProgressView().controlSize(.mini)
                    Text("Checking the network…").foregroundStyle(.secondary)
                } else if let o = owner {
                    Image(systemName: "xmark.circle.fill").foregroundStyle(.red)
                    Text("\(label).local is taken on this network (by \(o)).").foregroundStyle(.red)
                } else {
                    Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                    Text("https://\(label).local is free.").foregroundStyle(.secondary)
                }
            }
            .font(.caption)
        }
        .task(id: label) {
            if label.isEmpty && current == nil && !suggested {
                suggested = true
                label = await Homeport.suggestedLocal()
                return
            }
            guard Homeport.validLocal(label) else { owner = nil; return }
            checking = true
            try? await Task.sleep(nanoseconds: 500_000_000) // after typing stops
            if Task.isCancelled { return }
            owner = label + ".local" == current ? nil : await Homeport.localOwner(label + ".local")
            checking = false
        }
    }
}

// LocalNameSheet makes Homeport local (from an internet name) or changes its .local name.
struct LocalNameSheet: View {
    @EnvironmentObject var m: Monitor
    @Environment(\.dismiss) private var dismiss
    @State private var label = ""
    @State private var owner: String?

    private var main: String { m.overview?.settings?.mainDomain ?? "" }
    private var unchanged: Bool { label + ".local" == main }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(m.isLocal ? "Change the name on this network" : "Make Homeport local").font(.title2.bold())
            Text(m.isLocal
                 ? "Homeport and its apps move to the new name (cloud.\(main) becomes cloud.NEW.local), and this Mac stops answering to \(main), so another Homeport on the network can use it. Bookmarks to the old names stop working."
                 : "Homeport moves to a name on this network only, with its own certificates (browsers warn once). Apps move under it too. \(main) and its app addresses come out of Homeport, which stops updating the name at DuckDNS or deSEC (the name stays yours there).")
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            LocalNameField(label: $label, owner: $owner, current: m.isLocal ? main : nil)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                Button(m.isLocal ? "Change name" : "Make it local") {
                    dismiss()
                    let l = label
                    DispatchQueue.main.asyncAfter(deadline: .now() + 0.4) { m.setLocalName(l) }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(!Homeport.validLocal(label) || owner != nil || unchanged)
            }
        }
        .padding(20)
        .frame(width: 500)
        .onAppear { if m.isLocal { label = String(main.dropLast(".local".count)) } }
    }
}

// MARK: Uninstall

struct UninstallView: View {
    @EnvironmentObject var m: Monitor
    @Environment(\.dismiss) private var dismiss
    @State private var keepCopy = true
    @State private var removeTools = true
    @State private var sure = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Uninstall Homeport").font(.title2.bold())
            Text("This removes every app's containers, Homeport and Caddy's background services (and the Ollama one Homeport set up), and the homeport program. Your Docker runtime, LM Studio, Ollama's models and this app stay.")
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            Toggle("Keep a copy of the data (users, settings, certificates and every app's files)", isOn: $keepCopy)
            Text(keepCopy ? "Kept beside it as “Homeport-removed-<date>” in ~/Library/Application Support." : "Moved to the Trash: gone for good once the Trash is emptied.")
                .font(.caption).foregroundStyle(keepCopy ? Color.secondary : Color.red)
            Toggle("Also remove Caddy and restic (installed with Homebrew for Homeport) and Caddy's certificates", isOn: $removeTools)
            HStack {
                Text("Type “uninstall” to confirm:")
                TextField("", text: $sure).frame(width: 120)
            }
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Uninstall", role: .destructive) {
                    dismiss()
                    // After this sheet has closed: the progress opens as a sheet of its own.
                    let (copy, tools) = (keepCopy, removeTools)
                    DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { m.uninstall(keepCopy: copy, removeTools: tools) }
                }
                .disabled(sure.lowercased() != "uninstall")
            }
        }
        .padding(20)
        .frame(width: 520)
    }
}
