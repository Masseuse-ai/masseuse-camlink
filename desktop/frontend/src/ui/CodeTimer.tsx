// How much longer the code is good for, as the ring an authenticator app
// draws: full when the code is new, wiped clockwise from twelve o'clock
// over the code's life (the gap grows the way a clock's hand goes), ember
// and still for the last stretch (the code's cells are what breathe); and,
// for the line beside it, the time left in words
// (countdownText: "9 minutes and 5 seconds"). The service sends the next
// code when this one lapses.

import { useEffect, useState } from 'react';

import { cn } from '@/lib/utils';

/** The last stretch of a code's life, when the ring and the code turn ember. */
export const CODE_ENDING_MS = 60_000;

export interface Countdown {
    /** 0 to 1 of the code's life left. */
    fraction: number;
    remainingMs: number;
    /** In the last stretch: time to look at the phone. */
    ending: boolean;
    /** Past its end: the next code is due. */
    expired: boolean;
}

/** The code's remaining life, recomputed every second. */
export function useCountdown(expiresAt: number | undefined, ttlMs: number | undefined): Countdown | null {
    const [now, setNow] = useState(() => Date.now());
    useEffect(() => {
        if (!expiresAt) return;
        setNow(Date.now());
        const t = setInterval(() => setNow(Date.now()), 1000);
        return () => clearInterval(t);
    }, [expiresAt]);
    if (!expiresAt || !ttlMs) return null;
    const remainingMs = Math.max(0, expiresAt - now);
    return {
        fraction: Math.max(0, Math.min(1, remainingMs / ttlMs)),
        remainingMs,
        ending: remainingMs > 0 && remainingMs <= CODE_ENDING_MS,
        expired: remainingMs <= 0,
    };
}

/**
 * The time left in words, whole seconds rounded up so a new code starts at
 * "10 minutes": "9 minutes and 5 seconds", "1 minute and 1 second", "45
 * seconds", "9 minutes".
 */
export function countdownText(remainingMs: number): string {
    const total = Math.max(0, Math.ceil(remainingMs / 1000));
    const minutes = Math.floor(total / 60);
    const seconds = total % 60;
    const unit = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;
    if (minutes === 0) return unit(seconds, 'second');
    if (seconds === 0) return unit(minutes, 'minute');
    return `${unit(minutes, 'minute')} and ${unit(seconds, 'second')}`;
}

interface Props {
    countdown: Countdown | null;
    size?: number;
    className?: string;
}

export function CodeTimer({ countdown, size = 32, className }: Props) {
    const stroke = 3.5;
    const r = (size - stroke) / 2;
    const c = 2 * Math.PI * r;
    const fraction = countdown?.fraction ?? 0;
    const ending = countdown?.ending ?? false;
    // The stroke runs from three o'clock clockwise; mirrored and turned a
    // quarter it runs from twelve o'clock counter-clockwise, so the part
    // that is gone grows clockwise from twelve, the way a clock's hand goes.
    return (
        <svg
            width={size}
            height={size}
            viewBox={`0 0 ${size} ${size}`}
            role="img"
            aria-label={countdown ? (countdown.expired ? 'The code has expired; a new one is coming' : `${countdownText(countdown.remainingMs)} left on this code`) : 'No code yet'}
            className={cn('shrink-0 rotate-90 -scale-x-100', className)}
        >
            <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke="currentColor" strokeWidth={stroke} className="text-white/12" />
            <circle
                cx={size / 2}
                cy={size / 2}
                r={r}
                fill="none"
                stroke="currentColor"
                strokeWidth={stroke}
                strokeLinecap="round"
                strokeDasharray={c}
                strokeDashoffset={c * (1 - fraction)}
                // Ember for the last stretch, but still: the code's cells
                // breathe (CodeCells); a pulsing arc reads as a broken ring.
                className={cn('transition-[stroke-dashoffset,color] duration-1000 ease-linear', ending ? 'text-ember' : 'text-rose')}
            />
        </svg>
    );
}
