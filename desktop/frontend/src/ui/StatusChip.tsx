import { Loader2 } from 'lucide-react';
import { useRef, type ReactNode } from 'react';

export type Tone = 'neutral' | 'live' | 'warn' | 'busy' | 'speaking';

const DOT: Record<Tone, string> = {
    neutral: 'bg-bone-dim',
    live: 'bg-mint animate-pulse-soft',
    warn: 'bg-amber',
    busy: 'bg-rose',
    // The masseuse is talking: her colour, breathing.
    speaking: 'bg-rose animate-pulse-soft shadow-[0_0_12px_2px_rgba(240,167,204,0.55)]',
};

interface Props {
    tone: Tone;
    children: ReactNode;
    detail?: ReactNode;
    /** Three quick taps toggle diagnostics. */
    onTripleTap?: () => void;
}

export function StatusChip({ tone, children, detail, onTripleTap }: Props) {
    const taps = useRef<number[]>([]);
    const handleTap = () => {
        const now = Date.now();
        taps.current = [...taps.current.filter((t) => now - t < 700), now];
        if (taps.current.length >= 3) {
            taps.current = [];
            onTripleTap?.();
        }
    };
    return (
        <button
            type="button"
            onClick={handleTap}
            className="inline-flex max-w-[70vw] items-center gap-2 rounded-full bg-black/40 px-3.5 py-2 text-[13px] font-medium text-bone ring-1 ring-white/12 backdrop-blur-md"
        >
            {tone === 'busy'
                ? <Loader2 className="lucide h-3.5 w-3.5 animate-spin text-rose" strokeWidth={2.5} />
                : <span className={`h-2 w-2 shrink-0 rounded-full ${DOT[tone]}`} />}
            <span className="truncate">{children}</span>
            {detail ? <span className="font-mono text-[11px] text-bone-dim">{detail}</span> : null}
        </button>
    );
}
