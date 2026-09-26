// The native menus, drawn for review (development only): the real ones are
// the operating system's, built in main.go / menu.go. Two shapes, as Wails'
// platform conventions have them: on macOS the application menu carries
// About and Quit; on Windows and Linux there is no application menu, so
// File carries the update check and Quit, and Help carries About.

import { Fragment } from 'react';

import type { Platform } from '../bridge/store';

type Item = { label: string; shortcut?: string; role?: boolean; emits?: string; opens?: string; status?: boolean } | 'separator';

interface MenuSpec {
    title: string;
    items: Item[];
}

const HELP_LINKS: Item[] = [
    { label: 'Learn more about Masseuse.ai', opens: 'masseuse.ai/app' },
    { label: 'How it stays private', opens: 'README, How it stays private' },
    { label: 'Verify this download', opens: 'VERIFY.md' },
    { label: 'Report a security issue', opens: 'SECURITY.md' },
    'separator',
    { label: 'Show the log' },
    { label: 'Open the state folder' },
];

const MAC: MenuSpec[] = [
    {
        title: 'Masseuse.ai',
        items: [
            { label: 'About Masseuse.ai', emits: 'menu: about' },
            'separator',
            { label: 'Check for updates…', emits: 'menu: check-updates' },
            { label: 'Masseuse.ai v0.13.0 · masseuse-camlink', status: true },
            { label: 'Up to date', status: true },
            'separator',
            { label: 'Hide Masseuse.ai', shortcut: '⌘H', role: true },
            { label: 'Hide Others', shortcut: '⌥⌘H', role: true },
            { label: 'Show All', role: true },
            'separator',
            { label: 'Quit Masseuse.ai', shortcut: '⌘Q', role: true },
        ],
    },
    { title: 'Edit', items: [{ label: 'Undo', shortcut: '⌘Z', role: true }, { label: 'Redo', shortcut: '⇧⌘Z', role: true }, 'separator', { label: 'Cut', shortcut: '⌘X', role: true }, { label: 'Copy', shortcut: '⌘C', role: true }, { label: 'Paste', shortcut: '⌘V', role: true }, { label: 'Select All', shortcut: '⌘A', role: true }] },
    { title: 'Window', items: [{ label: 'Minimise', shortcut: '⌘M', role: true }, { label: 'Zoom', role: true }, 'separator', { label: 'Bring All to Front', role: true }] },
    { title: 'Help', items: HELP_LINKS },
];

const OTHER: MenuSpec[] = [
    { title: 'File', items: [{ label: 'Check for updates…', emits: 'menu: check-updates' }, { label: 'Masseuse.ai v0.13.0 · masseuse-camlink', status: true }, { label: 'Up to date', status: true }, 'separator', { label: 'Quit Masseuse.ai', shortcut: 'Ctrl+Q', role: true }] },
    { title: 'Edit', items: [{ label: 'Undo', shortcut: 'Ctrl+Z', role: true }, { label: 'Redo', shortcut: 'Ctrl+Y', role: true }, 'separator', { label: 'Cut', shortcut: 'Ctrl+X', role: true }, { label: 'Copy', shortcut: 'Ctrl+C', role: true }, { label: 'Paste', shortcut: 'Ctrl+V', role: true }, { label: 'Select All', shortcut: 'Ctrl+A', role: true }] },
    { title: 'Help', items: [...HELP_LINKS, 'separator', { label: 'About Masseuse.ai', emits: 'menu: about' }] },
];

function MenuColumn({ spec, mac }: { spec: MenuSpec; mac: boolean }) {
    return (
        <div className="min-w-[220px]">
            <div className={`mb-2 inline-flex h-7 items-center rounded-md px-2.5 text-[13px] font-medium ${mac ? 'bg-white/10 text-bone' : 'bg-white/10 text-bone'}`}>{spec.title}</div>
            <div className="rounded-xl bg-ink-soft py-1.5 shadow-2xl ring-1 ring-white/12">
                {spec.items.map((item, i) =>
                    item === 'separator' ? (
                        <div key={i} className="my-1 h-px bg-white/10" />
                    ) : (
                        <div key={i} className={`flex items-center justify-between gap-6 px-3.5 py-1 text-[13px] ${item.status ? 'text-bone/45' : 'text-bone'}`}>
                            <span>{item.label}</span>
                            <span className="flex items-center gap-2 text-[11px] text-bone/45">
                                {item.status ? <span className="italic">status, relabelled as the connector reports</span> : null}
                                {item.emits ? <span className="font-mono">{item.emits}</span> : null}
                                {item.opens ? <span className="font-mono">→ {item.opens}</span> : null}
                                {item.role ? <span className="italic">role</span> : null}
                                {item.shortcut ? <span className="font-mono">{item.shortcut}</span> : null}
                            </span>
                        </div>
                    ),
                )}
            </div>
        </div>
    );
}

export function MenuReference({ platform, onClose }: { platform: Platform; onClose: () => void }) {
    const mac = platform === 'darwin';
    const specs = mac ? MAC : OTHER;
    return (
        <div className="screen-body flex flex-1 flex-col overflow-auto px-8 pt-2 pb-6">
            <div className="flex items-end justify-between gap-6">
                <div>
                    <h1 className="text-[28px] leading-tight font-semibold tracking-tight text-bone">Native menus · {mac ? 'macOS' : platform === 'windows' ? 'Windows' : 'Linux'}</h1>
                    <p className="mt-1.5 max-w-[70ch] text-[15px] leading-snug text-bone/75">
                        For review only; the menus themselves are the operating system's (desktop/menu.go). Items marked <span className="italic text-bone/60">role</span> are Wails' standard items; the dimmed ones are status lines (the connector's version, the state of updates) that cannot be chosen; the others either open a page in the browser or send a request to this window.
                    </p>
                </div>
                <button type="button" onClick={onClose} className="shrink-0 rounded-full bg-white/10 px-4 py-2 text-[13px] font-semibold text-bone ring-1 ring-white/15 hover:bg-white/14">
                    Back to the window
                </button>
            </div>
            <div className="mt-6 flex flex-wrap items-start gap-6">
                {specs.map((spec) => (
                    <Fragment key={spec.title}>
                        <MenuColumn spec={spec} mac={mac} />
                    </Fragment>
                ))}
            </div>
            <div className="mt-8 max-w-[70ch] text-[13px] leading-snug text-bone/60">
                <p>
                    Closing the window quits on every platform (as closing the terminal window does today): the camera goes off and the unit is released. Quit asks for confirmation only while a session has the camera or the unit is armed.
                    {mac ? ' The title bar is hidden and the page’s top bar stands in for it; the traffic lights sit over it.' : ' The native title bar and this menu bar sit above the page.'}
                </p>
            </div>
        </div>
    );
}
