// What the connector said in passing, as a short stack at the foot of the
// window: the lines it prints today between the state changes (a device
// standing in, a congested connection, an update). Information goes by
// itself after a while; a warning or an error waits to be dismissed.

import { CircleAlert, Info, TriangleAlert, X } from 'lucide-react';
import { useEffect } from 'react';

import { useAppState, useDispatch, type Notice } from '../bridge/store';
import { cn } from '@/lib/utils';

const TONE: Record<Notice['level'], { icon: typeof Info; className: string }> = {
    info: { icon: Info, className: 'bg-white/8 ring-white/12 text-bone' },
    warn: { icon: TriangleAlert, className: 'bg-amber/10 ring-amber/30 text-bone [&_svg]:text-amber' },
    error: { icon: CircleAlert, className: 'bg-ember/10 ring-ember/30 text-bone [&_svg]:text-ember' },
};

const INFO_TTL_MS = 9000;

export function Notices() {
    const { notices } = useAppState();
    const dispatch = useDispatch();

    useEffect(() => {
        const timers = notices
            .filter((n) => n.level === 'info')
            .map((n) => setTimeout(() => dispatch({ type: 'ui/dismiss', id: n.id }), Math.max(0, n.at + INFO_TTL_MS - Date.now())));
        return () => timers.forEach(clearTimeout);
    }, [notices, dispatch]);

    if (notices.length === 0) return null;
    return (
        <div className="pointer-events-none absolute inset-x-0 bottom-4 z-20 flex flex-col items-center gap-2 px-6" aria-live="polite">
            {notices.map((n) => {
                const tone = TONE[n.level];
                const Icon = tone.icon;
                return (
                    <div key={n.id} className={cn('pointer-events-auto flex w-full max-w-[640px] items-start gap-3 rounded-2xl px-4 py-3 text-[14px] leading-snug shadow-lg ring-1 backdrop-blur-md animate-rise', tone.className)}>
                        <Icon className="lucide mt-0.5 h-4 w-4 shrink-0" strokeWidth={2.4} />
                        <span className="flex-1 select-text">{n.text}</span>
                        <button
                            type="button"
                            aria-label="Dismiss"
                            onClick={() => dispatch({ type: 'ui/dismiss', id: n.id })}
                            className="-m-1 rounded-full p-1 text-bone/60 transition-colors hover:bg-white/10 hover:text-bone"
                        >
                            <X className="lucide h-4 w-4" strokeWidth={2.4} />
                        </button>
                    </div>
                );
            })}
        </div>
    );
}
