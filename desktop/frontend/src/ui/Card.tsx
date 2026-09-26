// The brand's surfaces on the padding scale (docs/DESKTOP.md, "Shape"): a
// card on translucent white with its ring and 20 pt inside, the small
// uppercase label the phone puts over a card's contents with 12 pt under it,
// a quieter well inside a card with 16 pt inside, and the one list row.

import type { LucideIcon } from 'lucide-react';
import type { ReactNode } from 'react';

import { cn } from '@/lib/utils';

export function Card({ className, children }: { className?: string; children: ReactNode }) {
    return <section className={cn('rounded-3xl bg-white/6 p-5 ring-1 ring-white/10', className)}>{children}</section>;
}

/** A card's heading: the eyebrow, and an optional line of controls at the right, on one 16-pt line. */
export function CardLabel({ icon: Icon, children, tone = 'dim', trailing, className }: { icon?: LucideIcon; children: ReactNode; tone?: 'dim' | 'rose'; trailing?: ReactNode; className?: string }) {
    return (
        <div className={cn('mb-3 flex min-h-5 items-center justify-between gap-3', className)}>
            <div className={cn('type-label flex items-center gap-2', tone === 'rose' ? 'text-rose' : 'text-bone-dim')}>
                {Icon ? <Icon className="lucide h-4 w-4" strokeWidth={2.2} /> : null}
                <span>{children}</span>
            </div>
            {trailing}
        </div>
    );
}

/** A quieter well inside a card. */
export function Well({ className, children }: { className?: string; children: ReactNode }) {
    return <div className={cn('rounded-2xl bg-white/6 p-4 ring-1 ring-white/10', className)}>{children}</div>;
}

/**
 * The one list row (HIG lists): 52 pt tall, the leading glyph in its own
 * column, the title with an optional second line, a badge trailing. The
 * radio rows (ui/DeviceCard.tsx, ui/UnitCard.tsx) and the plain ones share
 * it.
 */
export const ROW = 'flex min-h-13 min-w-0 items-center gap-3 rounded-2xl px-4 py-2';

/** A row that is a choice: the whole row is the radio, lit in rose when checked. */
export const CHOICE_ROW = cn(ROW, 'cursor-pointer bg-white/5 ring-1 ring-white/10 transition-colors hover:bg-white/8 has-data-checked:bg-rose/10 has-data-checked:ring-rose/40');

/** A choice that cannot be made right now. */
export const CHOICE_ROW_DISABLED = 'cursor-default opacity-60 hover:bg-white/5';
