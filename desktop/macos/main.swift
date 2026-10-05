import AppKit
import WebKit

// A window for the shared /miniapp client. The agent owns its own lifetime:
// closing this window or quitting the UI never terminates it or its PTYs.
final class RemotaiApp: NSObject, NSApplicationDelegate, NSWindowDelegate,
    WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate {
    private var window: NSWindow!
    private var web: WKWebView!
    private var localURL: URL?
    private var bootstrap: Process?
    private var downloads = Set<WKDownload>()
    private var notice: String?
    private var starting = false
    private var loaded = false
    private var reopenCount = 0
    private var checkingAgent = false
    private var recoveringAgent = false
    private var bootstrapCount = 0

    func applicationDidFinishLaunching(_ notification: Notification) {
        makeMenu()
        let size = NSSize(width: 1100, height: 780)
        window = NSWindow(contentRect: NSRect(origin: .zero, size: size),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        window.title = "Remotai"
        window.minSize = NSSize(width: 640, height: 480)
        window.isReleasedWhenClosed = false
        window.delegate = self
        window.setFrameAutosaveName("RemotaiMainWindow")
        if let screen = NSScreen.main, window.frame.width > screen.visibleFrame.width || window.frame.height > screen.visibleFrame.height {
            window.setFrame(screen.visibleFrame.insetBy(dx: 12, dy: 12), display: false)
        }
        window.center()
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .default()
        web = WKWebView(frame: NSRect(origin: .zero, size: size), configuration: config)
        web.autoresizingMask = [.width, .height]
        web.navigationDelegate = self
        web.uiDelegate = self
        web.allowsBackForwardNavigationGestures = true
        showWindow()
        startAgent()
    }

    private func makeMenu() {
        let menu = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "О Remotai", action: #selector(about), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Скрыть Remotai", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(withTitle: "Закрыть Remotai", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        menu.addItem(appItem)
        let editItem = NSMenuItem()
        editItem.title = "Правка"
        let edit = NSMenu(title: "Правка")
        for (title, action, key) in [("Отменить", "undo:", "z"), ("Вырезать", "cut:", "x"),
                                     ("Копировать", "copy:", "c"), ("Вставить", "paste:", "v"),
                                     ("Выделить всё", "selectAll:", "a")] {
            edit.addItem(withTitle: title, action: Selector(action), keyEquivalent: key)
        }
        editItem.submenu = edit
        menu.addItem(editItem)
        let viewItem = NSMenuItem()
        viewItem.title = "Окно"
        let view = NSMenu(title: "Окно")
        view.addItem(withTitle: "Показать Remotai", action: #selector(showWindow), keyEquivalent: "0")
        view.addItem(withTitle: "Назад", action: #selector(goBack), keyEquivalent: "[")
        view.addItem(withTitle: "Обновить страницу", action: #selector(reloadPage), keyEquivalent: "r")
        view.addItem(withTitle: "Закрыть окно", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        viewItem.submenu = view
        menu.addItem(viewItem)
        NSApp.mainMenu = menu
    }

    @objc private func about() {
        let alert = NSAlert()
        alert.messageText = "Remotai"
        alert.informativeText = "Терминалы продолжают работать при закрытии окна. Чтобы остановить Remotai на этом Mac, откройте «Панель ПК» и выберите «Завершить Remotai на этом компьютере…»."
        alert.beginSheetModal(for: window)
    }

    @objc private func showWindow() {
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showWindow() // Preserve navigation, drafts and terminal scroll position.
        reopenCount += 1
        writeSmokeReport(extra: ["reopened": true])
        ensureAgentRunning()
        return true
    }

    private func ensureAgentRunning() {
        guard !starting, !checkingAgent else { return }
        guard let local = localURL, var parts = URLComponents(url: local, resolvingAgainstBaseURL: false) else {
            startAgent()
            return
        }
        parts.path = "/api/setup/status"
        parts.query = nil
        parts.fragment = nil
        guard let url = parts.url else { return }
        checkingAgent = true
        let request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 5)
        URLSession.shared.dataTask(with: request) { [weak self] data, response, error in
            let state = data.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
            let alive = error == nil && (response as? HTTPURLResponse)?.statusCode == 200 &&
                !(state?["device_id"] as? String ?? "").isEmpty && !(state?["version"] as? String ?? "").isEmpty
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.checkingAgent = false
                if !alive {
                    self.recoveringAgent = true
                    self.startAgent()
                }
            }
        }.resume()
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    @objc private func goBack() { if web.canGoBack { web.goBack() } }
    @objc private func reloadPage() {
        if loaded { web.reload() } else { startAgent() }
    }

    private func showStatus(_ text: String, retry: Bool) {
        let label = NSTextField(wrappingLabelWithString: text)
        label.font = .systemFont(ofSize: 16)
        label.alignment = .center
        let stack = NSStackView(views: [label])
        stack.orientation = .vertical
        stack.spacing = 20
        if retry {
            let button = NSButton(title: "Повторить", target: self, action: #selector(startAgent))
            button.bezelStyle = .rounded
            stack.addArrangedSubview(button)
        } else {
            let progress = NSProgressIndicator()
            progress.style = .spinning
            progress.startAnimation(nil)
            stack.addArrangedSubview(progress)
        }
        let content = NSView(frame: window.contentView!.bounds)
        content.addSubview(stack)
        stack.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            stack.centerXAnchor.constraint(equalTo: content.centerXAnchor),
            stack.centerYAnchor.constraint(equalTo: content.centerYAnchor),
            stack.widthAnchor.constraint(lessThanOrEqualTo: content.widthAnchor, constant: -80),
            stack.widthAnchor.constraint(lessThanOrEqualToConstant: 540),
        ])
        window.contentView = content
    }

    @objc private func startAgent() {
        guard !starting else { return }
        starting = true
        loaded = false
        bootstrapCount += 1
        showStatus("Открываем Remotai…", retry: false)
        let task = Process()
        task.executableURL = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/Remotai")
        task.arguments = ["--desktop-window"]
        let output = Pipe()
        task.standardOutput = output
        task.standardError = FileHandle.nullDevice
        bootstrap = task
        task.terminationHandler = { [weak self] process in
            let data = output.fileHandleForReading.readDataToEndOfFile()
            let result = (try? JSONSerialization.jsonObject(with: data)) as? [String: String]
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.starting = false
                self.bootstrap = nil
                guard process.terminationStatus == 0, let raw = result?["url"],
                      let url = URL(string: raw), url.scheme == "http",
                      ["localhost", "127.0.0.1"].contains(url.host ?? ""),
                      url.port != nil, url.user == nil, url.password == nil else {
                    self.showStatus(result?["error"] ?? "Не удалось запустить Remotai. Повторите открытие приложения.", retry: true)
                    return
                }
                self.localURL = url
                self.notice = result?["notice"]
                self.web.load(URLRequest(url: url))
            }
        }
        do { try task.run() } catch {
            starting = false
            bootstrap = nil
            showStatus("В приложении не найден Remotai. Заново перенесите его из установочного диска в «Программы».", retry: true)
        }
    }

    private func isLocal(_ url: URL) -> Bool {
        url.scheme == "http" && ["localhost", "127.0.0.1"].contains(url.host ?? "") &&
        url.port == localURL?.port && url.user == nil && url.password == nil
    }

    private func openExternal(_ url: URL) {
        if ["https", "http", "tg", "mailto"].contains(url.scheme?.lowercased() ?? "") {
            NSWorkspace.shared.open(url)
        }
    }

    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = action.request.url else { decisionHandler(.cancel); return }
        if action.shouldPerformDownload && (isLocal(url) || url.scheme == "blob") {
            decisionHandler(.download)
        } else if isLocal(url) || action.targetFrame?.isMainFrame == false {
            decisionHandler(.allow)
        } else {
            decisionHandler(.cancel)
            if action.navigationType == .linkActivated || action.targetFrame == nil { openExternal(url) }
        }
    }

    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = action.request.url {
            if isLocal(url) { webView.load(action.request) } else { openExternal(url) }
        }
        return nil
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        loaded = true
        web.frame = window.contentView!.bounds
        window.contentView = web
        if let message = notice {
            notice = nil
            let alert = NSAlert()
            alert.messageText = "Обновление Remotai"
            alert.informativeText = message
            alert.beginSheetModal(for: window)
        }
        runSmokeCheck()
        if recoveringAgent {
            recoveringAgent = false
            writeSmokeReport(extra: ["reopened": true, "agent_recovered": true])
        }
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        if (error as NSError).code == NSURLErrorCancelled { return }
        loaded = false
        showStatus("Не удалось открыть Remotai. Если приложение обновляется, подождите немного и нажмите «Повторить».", retry: true)
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.beginSheetModal(for: window) { _ in completionHandler() }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "Продолжить")
        alert.addButton(withTitle: "Отмена")
        alert.beginSheetModal(for: window) { response in completionHandler(response == .alertFirstButtonReturn) }
    }

    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String,
                 defaultText: String?, initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping (String?) -> Void) {
        let alert = NSAlert()
        alert.messageText = prompt
        let input = NSTextField(string: defaultText ?? "")
        input.frame = NSRect(x: 0, y: 0, width: 320, height: 24)
        alert.accessoryView = input
        alert.addButton(withTitle: "Сохранить")
        alert.addButton(withTitle: "Отмена")
        alert.beginSheetModal(for: window) { response in completionHandler(response == .alertFirstButtonReturn ? input.stringValue : nil) }
    }

    func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping ([URL]?) -> Void) {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = parameters.allowsMultipleSelection
        panel.canChooseDirectories = parameters.allowsDirectories
        panel.canChooseFiles = true
        panel.beginSheetModal(for: window) { response in completionHandler(response == .OK ? panel.urls : nil) }
    }

    func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        decisionHandler(response.canShowMIMEType ? .allow : .download)
    }
    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        downloads.insert(download); download.delegate = self
    }
    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        downloads.insert(download); download.delegate = self
    }
    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse,
                  suggestedFilename: String, completionHandler: @escaping (URL?) -> Void) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = (suggestedFilename as NSString).lastPathComponent
        panel.beginSheetModal(for: window) { result in completionHandler(result == .OK ? panel.url : nil) }
    }
    func downloadDidFinish(_ download: WKDownload) { downloads.remove(download) }
    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        downloads.remove(download)
    }

    // Hosted-runner acceptance exercises this actual window through LaunchServices.
    // No URLs, tokens or page contents are written to the proof file.
    private var smokePath: URL? {
        let args = CommandLine.arguments
        guard args.count == 3, args[1] == "--smoke-test", args[2].hasPrefix("/") else { return nil }
        return URL(fileURLWithPath: args[2])
    }
    private var smokeChecked = false
    private var smokeDOM = false
    private func writeSmokeReport(extra: [String: Any] = [:]) {
        guard let path = smokePath else { return }
        var report: [String: Any] = ["native_window": window.isVisible, "webview_loaded": loaded,
            "dom_ready": smokeDOM, "shared_client": web.url?.path.hasPrefix("/miniapp") == true,
            "reopen_count": reopenCount, "bootstrap_count": bootstrapCount,
            "dock_app": NSApp.activationPolicy() == .regular]
        extra.forEach { report[$0] = $1 }
        if let data = try? JSONSerialization.data(withJSONObject: report, options: .prettyPrinted) {
            try? data.write(to: path, options: .atomic)
        }
    }
    private func runSmokeCheck() {
        guard let path = smokePath, !smokeChecked else { return }
        smokeChecked = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) {
            self.web.evaluateJavaScript("document.readyState === 'complete' && (location.pathname.startsWith('/miniapp') ? !!document.getElementById('root')?.children.length : document.body.innerText.includes('Remotai'))") { result, error in
                self.smokeDOM = (result as? Bool) == true && error == nil
                self.web.takeSnapshot(with: nil) { picture, _ in
                    if let data = picture?.tiffRepresentation, let bitmap = NSBitmapImageRep(data: data),
                       let png = bitmap.representation(using: .png, properties: [:]) {
                        try? png.write(to: path.deletingPathExtension().appendingPathExtension("png"))
                    }
                    self.window.performClose(nil)
                    self.writeSmokeReport(extra: ["closed_after_load": !self.window.isVisible])
                }
            }
        }
    }
}

let app = NSApplication.shared
let delegate = RemotaiApp()
app.setActivationPolicy(.regular)
app.delegate = delegate
app.run()
