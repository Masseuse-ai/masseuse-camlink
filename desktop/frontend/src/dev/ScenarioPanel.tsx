// The scenario panel: a floating picker of every state the window can be
// in (src/bridge/mock/scenarios.ts), and the platform to draw it for. Only
// in development builds; `?scenario=<id>&os=<darwin|windows|linux>` opens
// one directly, so each state can be screenshotted.

import { FlaskConical, Info, Menu, RefreshCw, X } from 'lucide-react';
import { useState } from 'react';

import { scenarios, type Scenario } from '../bridge/mock/scenarios';
import { useBridge, useDispatch, type Platform } from '../bridge/store';
import { cn } from '@/lib/utils';

interface Props {
    current: Scenario;
    platform: Platform;
    onPick: (id: string) => void;
    onPlatform: (platform: Platform) => void;
    onMenuReference: () => void;
}

const PLATFORMS: { id: Platform; label: string }[] = [
    { id: 'darwin', label: 'macOS' },
    { id: 'windows', label: 'Windows' },
    { id: 'linux', label: 'Linux' },
];

export function ScenarioPanel({ current, platform, onPick, onPlatform, onMenuReference }: Props) {
    const [open, setOpen] = useState(false);
    const dispatch = useDispatch();
    const bridge = useBridge();
    const groups = Array.from(new Set(scenarios.map((s) => s.group)));
    return (
        <div className="no-drag fixed right-4 bottom-16 z-40 flex flex-col items-end gap-2 font-sans">
            {open ? (
                <div className="w-[320px] overflow-hidden rounded-2xl bg-ink-soft/95 text-[13px] text-bone shadow-2xl ring-1 ring-white/12 backdrop-blur-xl">
                    <div className="flex items-center justify-between px-4 py-3">
                        <span className="text-[12px] font-semibold tracking-wide text-bone-dim uppercase">Mock-up scenarios</span>
                        <button type="button" onClick={() => setOpen(false)} aria-label="Close" className="rounded-full p-1 text-bone/60 hover:bg-white/10 hover:text-bone">
                            <X className="lucide h-4 w-4" strokeWidth={2.4} />
                        </button>
                    </div>
                    <div className="flex items-center gap-1 px-4 pb-3">
                        {PLATFORMS.map((p) => (
                            <button
                                key={p.id}
                                type="button"
                                onClick={() => onPlatform(p.id)}
                                className={cn('h-7 rounded-full px-3 text-[12px] font-medium ring-1 ring-white/12 transition-colors', platform === p.id ? 'bg-rose/15 text-bone ring-rose/50' : 'text-bone/60 hover:text-bone')}
                            >
                                {p.label}
                            </button>
                        ))}
                        <button type="button" onClick={onMenuReference} className="ml-auto inline-flex h-7 items-center gap-1.5 rounded-full px-3 text-[12px] font-medium text-bone/60 ring-1 ring-white/12 hover:text-bone">
                            <Menu className="lucide h-3.5 w-3.5" strokeWidth={2.4} /> Menus
                        </button>
                    </div>
                    {/* The native menu's requests, for a browser that has no menu. */}
                    <div className="flex items-center gap-1 px-4 pb-3">
                        <span className="mr-1 text-[11px] text-bone/45">Menu:</span>
                        <button type="button" onClick={() => dispatch({ type: 'ui/about', open: true })} className="inline-flex h-7 items-center gap-1.5 rounded-full px-3 text-[12px] font-medium text-bone/60 ring-1 ring-white/12 hover:text-bone">
                            <Info className="lucide h-3.5 w-3.5" strokeWidth={2.4} /> About
                        </button>
                        <button type="button" onClick={() => void bridge.send({ type: 'update_now' })} className="inline-flex h-7 items-center gap-1.5 rounded-full px-3 text-[12px] font-medium text-bone/60 ring-1 ring-white/12 hover:text-bone">
                            <RefreshCw className="lucide h-3.5 w-3.5" strokeWidth={2.4} /> Check for updates
                        </button>
                    </div>
                    <div className="max-h-[52vh] overflow-y-auto border-t border-white/8 py-2">
                        {groups.map((group) => (
                            <div key={group} className="px-2 pb-2">
                                <div className="px-2 pt-2 pb-1 text-[11px] font-semibold tracking-wide text-bone/45 uppercase">{group}</div>
                                {scenarios
                                    .filter((s) => s.group === group)
                                    .map((s) => (
                                        <button
                                            key={s.id}
                                            type="button"
                                            onClick={() => onPick(s.id)}
                                            title={s.note}
                                            className={cn('block w-full rounded-xl px-2 py-1.5 text-left transition-colors hover:bg-white/8', s.id === current.id ? 'bg-rose/15 text-bone ring-1 ring-rose/40' : 'text-bone/75')}
                                        >
                                            <span className="block truncate">{s.title}</span>
                                            <span className="block truncate font-mono text-[10px] text-bone/40">{s.id}</span>
                                        </button>
                                    ))}
                            </div>
                        ))}
                    </div>
                </div>
            ) : null}
            <button
                type="button"
                onClick={() => setOpen((o) => !o)}
                className="inline-flex h-9 items-center gap-2 rounded-full bg-ink-soft/90 px-3.5 text-[12px] font-medium text-bone/80 shadow-lg ring-1 ring-white/12 backdrop-blur-md hover:text-bone"
                title={current.note}
            >
                <FlaskConical className="lucide h-3.5 w-3.5 text-rose" strokeWidth={2.4} />
                {current.title}
            </button>
        </div>
    );
}
