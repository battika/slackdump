(function () {
    "use strict";

    var defaultConnectionMessage = "Cannot reach the viewer server. Check that it is running and try again.";
    var lastSyncedPath = null;

    function qs(selector, root) {
        return (root || document).querySelector(selector);
    }

    function qsa(selector, root) {
        return Array.prototype.slice.call((root || document).querySelectorAll(selector));
    }

    function container() {
        return qs(".container");
    }

    function openSidePanel() {
        var root = container();
        if (root) {
            root.classList.add("thread-open");
        }
    }

    function closeSidePanel() {
        var root = container();
        if (root) {
            root.classList.remove("thread-open");
        }
    }

    function setActiveChannel(link) {
        qsa(".channel-sidebar .channel-list a").forEach(function (el) {
            el.classList.remove("active");
        });
        if (link) {
            link.classList.add("active");
        }
    }

    // Expands the group containing the active channel.  The trigger that matters
    // is htmx history restore (browser back/forward): a sidebar click cannot
    // reach a link inside a collapsed <details>, so navigation by clicking
    // always finds the group already open.
    function expandGroup(link) {
        if (!link) {
            return;
        }
        var group = link.closest("details.channel-group");
        if (group) {
            group.open = true;
            link.scrollIntoView({block: "nearest"});
        }
    }

    function syncActiveChannel() {
        var path = window.location.pathname;
        var match = null;

        qsa(".channel-sidebar .channel-list a").forEach(function (link) {
            var target = link.getAttribute("hx-get") || link.getAttribute("href");
            if (target && (path === target || path.indexOf(target + "/") === 0)) {
                match = link;
            }
        });
        setActiveChannel(match);
        if (path !== lastSyncedPath) {
            lastSyncedPath = path;
            expandGroup(match);
        }
    }

    function showConnectionError(message) {
        var banner = qs("#connection-status");
        if (!banner) {
            return;
        }
        banner.textContent = message || defaultConnectionMessage;
        banner.hidden = false;
    }

    function clearConnectionError() {
        var banner = qs("#connection-status");
        if (banner) {
            banner.hidden = true;
        }
    }

    function onDocumentClick(event) {
        var close = event.target.closest("[data-close-panel]");
        if (close) {
            event.preventDefault();
            closeSidePanel();
            return;
        }

        var sidePanelLink = event.target.closest('a[hx-target="#thread"]');
        if (sidePanelLink) {
            openSidePanel();
        }

        var channelLink = event.target.closest(".channel-sidebar .channel-list a");
        if (channelLink) {
            setActiveChannel(channelLink);
        }
    }

    function onTabKeydown(event) {
        var list = event.target.closest('[role="tablist"]');
        if (!list || event.target.getAttribute("role") !== "tab") {
            return;
        }

        var tabs = qsa('[role="tab"]:not([disabled])', list);
        var idx = tabs.indexOf(document.activeElement);
        if (idx === -1 || tabs.length === 0) {
            return;
        }

        if (event.key === "ArrowRight") {
            event.preventDefault();
            tabs[(idx + 1) % tabs.length].focus();
        } else if (event.key === "ArrowLeft") {
            event.preventDefault();
            tabs[(idx - 1 + tabs.length) % tabs.length].focus();
        } else if (event.key === "Home") {
            event.preventDefault();
            tabs[0].focus();
        } else if (event.key === "End") {
            event.preventDefault();
            tabs[tabs.length - 1].focus();
        }
    }

    // Step between search hits.  These only click the server-rendered links;
    // the URLs and all state live on the server, so the keyboard shortcut and
    // a mouse click take exactly the same path.
    function onSearchKeydown(event) {
        // Modifier chords (Ctrl/Cmd+N "new window", Alt+N menu mnemonics) belong
        // to the browser.  Shift is deliberately not in the list: it is what
        // produces "N".
        if (event.ctrlKey || event.metaKey || event.altKey) {
            return;
        }
        // Keys typed into any editable surface are text, not commands.  The
        // target can be the document itself (no matches()), hence the guard.
        var target = event.target;
        if (event.isComposing || !target || !target.matches ||
            target.isContentEditable ||
            target.matches("input, textarea, select")) {
            return;
        }
        var sel = null;
        if (event.key === "n") {
            sel = '.search-nav a[rel="next"]';
        } else if (event.key === "N") {
            sel = '.search-nav a[rel="prev"]';
        } else {
            return;
        }
        var link = qs(sel);
        if (link) {
            event.preventDefault();
            link.click();
        }
    }

    // An out-of-band swap is not a navigation, so the #anchor never fires and
    // the activated message can land thousands of pixels outside the viewport.
    // Bring it into view after the swap.
    function scrollHitIntoView() {
        var hit = qs("#conversation .message-header.search-hit");
        if (hit) {
            hit.scrollIntoView({block: "center"});
        }
    }

    function init() {
        document.addEventListener("click", onDocumentClick);
        document.addEventListener("keydown", onTabKeydown);
        document.addEventListener("keydown", onSearchKeydown);
        syncActiveChannel();
    }

    document.body.addEventListener("htmx:sendError", function () {
        showConnectionError(defaultConnectionMessage);
    });

    document.body.addEventListener("htmx:timeout", function () {
        showConnectionError("Request timed out. The viewer server may be unavailable.");
    });

    document.body.addEventListener("htmx:afterRequest", function (event) {
        if (event.detail && event.detail.successful) {
            clearConnectionError();
        }
    });

    document.body.addEventListener("htmx:beforeSwap", function (event) {
        if (!event.detail || !event.detail.xhr || !event.detail.target) {
            return;
        }
        if (event.detail.target.id === "channel-heading" && event.detail.xhr.status === 400) {
            event.detail.shouldSwap = true;
            event.detail.isError = false;
        }
    });

    document.body.addEventListener("htmx:afterSettle", function () {
        syncActiveChannel();
        scrollHitIntoView();
    });

    // Anything swapped into the side panel should make it visible.  The panel
    // is display:none until .container gets thread-open, and onDocumentClick
    // only opens it for anchors with hx-target="#thread" — which the search
    // box is not: it is an <input> firing on keyup, so results would load into
    // a hidden panel.  Keying off the swap itself covers every producer.
    document.body.addEventListener("htmx:afterSwap", function (event) {
        var target = event.detail && event.detail.target;
        if (target && target.id === "thread" && target.children.length > 0) {
            openSidePanel();
        }
    });

    window.addEventListener("offline", function () {
        showConnectionError("Your browser is offline. Check your network connection.");
    });
    window.addEventListener("online", clearConnectionError);

    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", init);
    } else {
        init();
    }
}());
