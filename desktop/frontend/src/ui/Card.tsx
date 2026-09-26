// The brand's surfaces: a card on translucent white with its ring, and the
// small uppercase label the phone puts over a card's contents.

import type { LucideIcon } from 'lucide-react';
import type { ReactNode } from 'react';

import { cn } from '@/lib/utils';

export function Card({ className, children }: { className?: string; children: ReactNode }) {
    return <section className={cn('rounded-3xl bg-white/6 p-5 ring-1 ring-white/10', className)}>{children}</section>;
}

/** A card's heading: the eyebrow, and an optional line of controls at the right. */
export function CardLabel({ icon: Icon, children, tone = 'dim', trailing }: { icon?: LucideIcon; children: ReactNode; tone?: 'dim' | 'rose'; trailing?: ReactNode }) {
    return (
        <div className="mb-3 flex items-center justify-between gap-3">
            <div className={cn('flex items-center gap-2 text-[12px] font-semibold tracking-wide uppercase', tone === 'rose' ? 'text-rose' : 'text-bone-dim')}>
                {Icon ? <Icon className="lucide h-4 w-4" strokeWidth={2.2} /> : null}
                <span>{children}</span>
            </div>
            {trailing}
        </div>
    );
}

/** A quieter well inside a card. */
export function Well({ className, children }: { className?: string; children: ReactNode }) {
    return <div className={cn('rounded-2xl bg-white/6 p-3.5 ring-1 ring-white/10', className)}>{children}</div>;
}
