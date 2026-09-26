// In a browser the page has no window around it, so the mock-ups draw one
// (development only): the default 1040x720 window on a darker backdrop,
// with the chrome the platform would add, scaled down as a whole when the
// browser is smaller than that so the layout stays the window's. On macOS
// the chrome is the three traffic lights over the page's own top bar (the
// native title bar is hidden; main.go); on Windows and Linux a title bar
// and the menu bar the application menu is shown in. Inside the desktop
// shell none of this renders; the operating system draws the real thing.

import { useEffect, useState, type ReactNode } from 'react';

import type { Platform } from '../bridge/store';

export const WINDOW = { width: 1040, height: 720 };

/** The title bar and menu bar Windows and Linux draw above the page; macOS draws none (the page's top bar is the title bar). */
const CHROME_HEIGHT = { darwin: 0, windows: 60, linux: 60 } as const;

function useFitScale(height: number, margin = 32): number {
    const compute = () => Math.min(1, (window.innerWidth - margin) / WINDOW.width, (window.innerHeight - margin) / height);
    const [scale, setScale] = useState(compute);
    useEffect(() => {
        const onResize = () => setScale(compute());
        window.addEventListener('resize', onResize);
        return () => window.removeEventListener('resize', onResize);
    }, [height]);
    return scale;
}

export function WindowFrame({ platform, children }: { platform: Platform; children: ReactNode }) {
    const mac = platform === 'darwin';
    const height = WINDOW.height + CHROME_HEIGHT[platform];
    const scale = useFitScale(height);
    return (
        <div className="flex h-full w-full items-center justify-center overflow-hidden bg-[#050408]">
            <div
                className="relative flex shrink-0 flex-col overflow-hidden rounded-[12px] bg-ink shadow-[0_30px_80px_rgb(0_0_0/0.6)] ring-1 ring-white/12"
                style={{ width: WINDOW.width, height, transform: `scale(${scale})`, transformOrigin: 'center' }}
            >
                {mac ? (
                    <div aria-hidden className="absolute top-[18px] left-[20px] z-30 flex items-center gap-2">
                        <span className="h-3 w-3 rounded-full bg-[#ff5f57] ring-1 ring-black/20" />
                        <span className="h-3 w-3 rounded-full bg-[#febc2e] ring-1 ring-black/20" />
                        <span className="h-3 w-3 rounded-full bg-[#28c840] ring-1 ring-black/20" />
                    </div>
                ) : (
                    <div aria-hidden className="h-[60px] shrink-0 select-none border-b border-white/8 bg-[#100d16] text-bone/80">
                        <div className="flex h-8 items-center justify-between pl-3 text-[12px]">
                            <span className="flex items-center gap-2">
                                <img src="/mark.svg" alt="" className="h-4 w-4 rounded-[3px]" />
                                Masseuse.ai
                            </span>
                            <span className="flex h-full items-stretch text-[13px]">
                                <span className="flex w-11 items-center justify-center">–</span>
                                <span className="flex w-11 items-center justify-center">▢</span>
                                <span className="flex w-11 items-center justify-center">✕</span>
                            </span>
                        </div>
                        <div className="flex h-7 items-center gap-1 px-2 text-[12px]">
                            {['File', 'Edit', 'Help'].map((m) => (
                                <span key={m} className="rounded px-2 py-0.5">
                                    {m}
                                </span>
                            ))}
                        </div>
                    </div>
                )}
                <div className="relative flex min-h-0 flex-1 flex-col">{children}</div>
            </div>
        </div>
    );
}
