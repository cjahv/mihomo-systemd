(() => {
    "use strict";

    if (window.parent === window || !document.referrer) return;
    const parent = new URL(document.referrer);
    if (!["http:", "https:"].includes(parent.protocol) || parent.hostname !== location.hostname) return;

    const protocol = "mihomo-panel-route-v1";
    // Mirrors the route grammar in internal/manager/web/index.html. Panel paths
    // are mirrored generically so upstream MetaCubeXD pages (control, profiles,
    // profiles/:id/edit, ...) sync without a manager change.
    const pagePattern = /^[A-Za-z0-9_~.%-]+(?:\/[A-Za-z0-9_~.%-]+)*$/;
    let router;
    let activeRequest = null;

    function isPanelPage(page) {
        return typeof page === "string" && page.length <= 256 && pagePattern.test(page) &&
            !page.split("/").some(segment => segment === "." || segment === "..");
    }

    function currentPage() {
        const page = router.currentRoute.value.path.slice(1);
        return isPanelPage(page) ? page : null;
    }

    function send(type, fields) {
        window.parent.postMessage({ protocol, type, ...fields }, parent.origin);
    }

    window.addEventListener("message", async event => {
        if (event.source !== window.parent || event.origin !== parent.origin || !router) return;
        const message = event.data;
        if (message?.protocol !== protocol) return;
        if (message.type === "hello") {
            send("ready", { page: currentPage() });
            return;
        }
        if (message.type !== "navigate" || !isPanelPage(message.page) ||
            !Number.isSafeInteger(message.id) || !currentPage()) return;

        activeRequest = message.id;
        try {
            await router.replace("/" + message.page);
        } catch (_) {
            // Report the actual location even when the panel rejects a navigation.
        } finally {
            if (activeRequest === message.id) {
                activeRequest = null;
                send("result", { id: message.id, page: currentPage() });
            }
        }
    });

    // The checked MetaCubeXD build mounts Nuxt at #__nuxt. Use its router
    // history adapter so a successful child navigation adds only the parent's
    // history entry. Standalone panel navigation keeps Vue Router's own history.
    async function attach() {
        const candidate = document.getElementById("__nuxt")?.__vue_app__?.config.globalProperties.$router;
        if (!candidate) return false;
        await candidate.isReady();
        const history = candidate.options.history;
        history.push = history.replace.bind(history);
        router = candidate;
        router.afterEach((_to, _from, failure) => {
            if (!failure && activeRequest === null) send("page", { page: currentPage() });
        });
        send("ready", { page: currentPage() });
        return true;
    }

    const deadline = Date.now() + 15000;
    async function waitForRouter() {
        if (!await attach() && Date.now() < deadline) setTimeout(waitForRouter, 50);
    }
    void waitForRouter();
})();
