// Why the person is here, under the wordmark on every step (ui/TopBar.tsx):
// the tagline in the brand's voice (index.css, --font-brand, the serif
// italic) and the differentiator as figures, the numbers in rose with their
// words small beside them, on one line, read aloud as the sentence they stand
// for. Quieter than a screen's heading, since it is chrome; the copy lives
// here alone.

import { cn } from '@/lib/utils';

export const TAGLINE = 'See how your full body responds to electrostimulation.';

/** The differentiator as a sentence, for assistive tech and the docs. */
export const PUNCHLINE = 'Over 300 data points analyzed 10 times a second.';

export function Punchline({ className }: { className?: string }) {
    return (
        <div className={cn('flex flex-wrap items-baseline gap-x-4 gap-y-1', className)}>
            <p className="font-brand text-[15px] leading-none text-bone/85 italic">{TAGLINE}</p>
            <p>
                <span className="sr-only">{PUNCHLINE}</span>
                <span aria-hidden className="flex items-baseline gap-x-2">
                    <Figure value="300+" label="data points" />
                    <span className="text-[11px] leading-none text-bone-dim">analyzed</span>
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
            <span className="text-[11px] leading-none text-bone-dim">{label}</span>
        </span>
    );
}
