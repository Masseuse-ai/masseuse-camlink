// The pairing code, in the cells the phone's code input draws
// (masseuse.ai web app, ui/CodeInput.tsx): four, the dash, four, in the
// mono face, here large and read-only. The cells fill the width they are
// given and size their letters to it (container query units), so the code
// is as large as the window allows and never overflows at the window's
// minimum width. Without a code the cells breathe, empty, until the
// service sends one.

import { cn } from '@/lib/utils';

interface Props {
    /** Eight letters and numbers, or null while there is none. */
    code: string | null;
    /** The code's last stretch: the letters turn ember and breathe, so the eye goes to the phone. */
    ending?: boolean;
    className?: string;
}

export function CodeCells({ code, ending = false, className }: Props) {
    const chars = code ? code.replace(/[^A-Z0-9]/gi, '').toUpperCase().slice(0, 8).split('') : [];
    return (
        <div
            className={cn('@container w-full max-w-[560px]', className)}
            role="img"
            aria-label={code ? `Pairing code ${chars.slice(0, 4).join('')} ${chars.slice(4).join('')}` : 'No pairing code yet'}
        >
            <div className="grid grid-cols-[repeat(4,minmax(0,1fr))_4cqw_repeat(4,minmax(0,1fr))] items-center gap-[1.4cqw]">
                {Array.from({ length: 8 }, (_, i) => {
                    const ch = chars[i];
                    return (
                        <span key={i} className="contents">
                            {i === 4 ? (
                                <span aria-hidden className="text-center text-[5cqw] leading-none text-bone/40">
                                    –
                                </span>
                            ) : null}
                            <span
                                className={cn(
                                    'grid aspect-[3/4] min-w-0 place-items-center rounded-[1.6cqw] font-mono text-[7.4cqw] leading-none font-semibold tabular-nums select-text transition-[background-color,box-shadow,color] duration-300',
                                    !ch && 'animate-pulse bg-white/6 ring-1 ring-white/10',
                                    ch && !ending && 'bg-white/12 text-bone ring-1 ring-white/20',
                                    ch && ending && 'animate-pulse-soft bg-ember/12 text-ember ring-1 ring-ember/50',
                                )}
                            >
                                {ch ?? ''}
                            </span>
                        </span>
                    );
                })}
            </div>
        </div>
    );
}
