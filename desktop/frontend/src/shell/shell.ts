// The window around the page: the Wails runtime when the page runs inside
// the desktop shell (main.go), a browser otherwise (the mock-ups). The
// shell's own methods (bindings) open links and quit; the connector's data
// comes through the bridge (src/bridge) either way.

import { Clipboard, Events, System } from '@wailsio/runtime';

import { ConnectorService } from '../../bindings/github.com/FemLed/masseuse-camlink/desktop';
import type { Platform } from '../bridge/store';

// The webview's own bridge object, there from the moment the document is
// created: WebView2's on Windows, WebKit's script message handler (named
// "external" by Wails) on macOS and Linux. The Wails runtime's own answer,
// System.IsDesktop(), reads window._wails.environment, which the shell
// injects only once the page has loaded (on Windows and Linux after the
// page's scripts have run), so it cannot be asked at the first render.
interface BridgeWindow {
    chrome?: { webview?: { postMessage?: unknown } };
    webkit?: { messageHandlers?: { external?: unknown } };
    _wails?: { environment?: { OS?: string } };
}

function bridgeWindow(): BridgeWindow {
    return window as unknown as BridgeWindow;
}

/** Whether the page is inside a webview the shell owns, known before the runtime has spoken. */
export function inShell(): boolean {
    const w = bridgeWindow();
    return Boolean(w.chrome?.webview?.postMessage) || Boolean(w.webkit?.messageHandlers?.external);
}

/** Whether the page is inside the desktop shell rather than a browser. */
export function inWails(): boolean {
    return System.IsDesktop() || inShell();
}

/** The platform the shell runs on, when inside it: the runtime's word, or the browser's own until the runtime has spoken. */
export function hostPlatform(): Platform | null {
    if (System.IsMac()) return 'darwin';
    if (System.IsWindows()) return 'windows';
    if (System.IsLinux()) return 'linux';
    if (!inShell()) return null;
    const ua = navigator.userAgent;
    if (/Windows NT/.test(ua)) return 'windows';
    if (/Macintosh|Mac OS X/.test(ua)) return 'darwin';
    if (/Linux|X11/.test(ua)) return 'linux';
    return null;
}

/** Whether the runtime's configuration (window._wails.environment) has been injected. */
function runtimeConfigured(): boolean {
    return Boolean(bridgeWindow()._wails?.environment?.OS);
}

/** The event the injected runtime configuration dispatches once it is in place. */
const RUNTIME_CONFIG_READY = 'wails:runtime-config-ready';

/**
 * Resolves once the shell's runtime is configured, so System.IsDesktop()
 * and the platform answer right; at once in a browser, or when the
 * configuration is already there. Bounded: past `timeoutMs` the page goes
 * on regardless (inShell() and the browser's platform still hold), so a
 * missed event can never leave the window blank.
 */
export function runtimeReady(timeoutMs = 3000): Promise<void> {
    if (!inShell() || runtimeConfigured()) return Promise.resolve();
    return new Promise((resolve) => {
        let done = false;
        const finish = () => {
            if (done) return;
            done = true;
            clearInterval(poll);
            clearTimeout(cap);
            window.removeEventListener(RUNTIME_CONFIG_READY, finish);
            resolve();
        };
        window.addEventListener(RUNTIME_CONFIG_READY, finish, { once: true });
        // The event is dispatched from a microtask after the injection; the
        // poll covers an injection that landed between the check above and
        // the listener, and any runtime that stops dispatching it.
        const poll = setInterval(() => {
            if (runtimeConfigured()) finish();
        }, 50);
        const cap = setTimeout(finish, timeoutMs);
    });
}

/** The pages the Help menu and About open, by name; the shell holds the same list (connector.go). */
export const LINKS = {
    'learn-more': 'https://masseuse.ai/app',
    privacy: 'https://github.com/FemLed/masseuse-camlink#how-it-stays-private',
    verify: 'https://github.com/FemLed/masseuse-camlink/blob/main/VERIFY.md',
    security: 'https://github.com/FemLed/masseuse-camlink/blob/main/SECURITY.md',
    source: 'https://github.com/FemLed/masseuse-camlink',
    releases: 'https://github.com/FemLed/masseuse-camlink/releases',
} as const;

export type LinkName = keyof typeof LINKS;

/** Opens one of the known pages in the default browser. */
export async function openLink(name: LinkName): Promise<void> {
    if (inWails()) {
        await ConnectorService.OpenLink(name);
    } else {
        window.open(LINKS[name], '_blank', 'noopener');
    }
}

export async function revealStateDir(): Promise<void> {
    if (inWails()) await ConnectorService.RevealStateDir();
}

/** Ends the program; the camera goes off and the unit is released on the way out. */
export async function quit(): Promise<void> {
    if (inWails()) await ConnectorService.Quit();
}

/** Puts text on the clipboard: the shell's clipboard inside the window, the browser's outside. */
export async function copyText(text: string): Promise<boolean> {
    try {
        if (inWails()) {
            await Clipboard.SetText(text);
        } else {
            await navigator.clipboard.writeText(text);
        }
        return true;
    } catch {
        return false;
    }
}

/** Starts the connector again after it stopped; in a browser the page starts over. */
export async function restartConnector(): Promise<void> {
    if (inWails()) {
        await ConnectorService.Restart();
    } else {
        window.location.reload();
    }
}

/** Reveals the connector's log (the shell keeps it under the state directory). */
export async function showLog(): Promise<void> {
    if (inWails()) await ConnectorService.ShowLog();
}

export type MenuAction = 'about' | 'check-updates';

/** Listens for the native menu's requests (main.go emits them as "menu" events). */
export function onMenu(handler: (action: MenuAction) => void): () => void {
    if (!inWails()) return () => {};
    return Events.On('menu', (ev) => {
        const action = ev.data;
        if (action === 'about' || action === 'check-updates') handler(action);
    });
}
