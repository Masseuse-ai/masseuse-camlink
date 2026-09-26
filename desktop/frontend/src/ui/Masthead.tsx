// The masthead: why the person is here, on every step, as a row of its own
// on the content grid under the title strip (ui/TopBar.tsx). The tagline in
// the brand's voice (index.css, --font-brand, the serif italic) at the left,
// the differentiator as figures at the right on the same baseline, the
// numbers in rose with their words small beside them, read aloud as the
// sentence they stand for; a hairline under the row. It is chrome, quieter
// than a screen's title, and the copy lives here alone.

import { cn } from '@/lib/utils';

export const TAGLINE = 'See your whole body respond to electrostimulation.';

/** The differentiator as a sentence, for assistive tech and the docs. */
export const PUNCHLINE = 'Over 300 data points analyzed 10 times a second.';

export function Masthead({ className }: { className?: string }) {
    return (
        <div className={cn('flex h-10 shrink-0 items-baseline justify-between gap-gutter border-b border-white/8 px-inset', className)}>
            <p className="min-w-0 truncate font-brand text-[17px] leading-10 text-bone/85 italic">{TAGLINE}</p>
            <p className="shrink-0 leading-10">
                <span className="sr-only">{PUNCHLINE}</span>
                <span aria-hidden className="inline-flex items-baseline gap-x-2">
                    <Figure value="300+" label="data points" />
                    <span className="type-caption text-bone-dim">analyzed</span>
                    <Figure value="10×" label="a second" />
                </span>
            </p>
        </div>
    );
}

/** One figure: the number in rose, as the steps' numerals are, its words small beside it. */
export function Figure({ value, label, className }: { value: string; label: string; className?: string }) {
    return (
        <span className={cn('inline-flex items-baseline gap-1.5', className)}>
            <span className="text-[20px] leading-none font-semibold tracking-tight text-rose tabular-nums">{value}</span>
            <span className="type-caption text-bone-dim">{label}</span>
        </span>
    );
}
