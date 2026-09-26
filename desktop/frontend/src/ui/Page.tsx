// The screen template every step is laid out on, so the regions never move
// between steps: a header (the title on the display size, the lead under it
// at a reading measure, and at the right, on the title's line, the one
// control that belongs to the whole screen), the body on the twelve-column
// grid (index.css, grid-12), and a footer of one height with the actions at
// the right, the primary rightmost, and room at the left for a word about
// them. Blocked stands outside it: an error has the window to itself.

import type { ReactNode } from 'react';

import { cn } from '@/lib/utils';

interface Props {
    /** Without a title there is no header: the screen sets its own inside the body (Pair, whose title belongs with the code). */
    title?: ReactNode;
    lead?: ReactNode;
    /** The screen's one control, on the title's line at the right. */
    control?: ReactNode;
    /** The actions, right-aligned; without any the body reaches the bottom. */
    footer?: ReactNode;
    /** A word beside the actions, at their left. */
    note?: ReactNode;
    /** The body's classes beyond the grid: its rows, its alignment. */
    bodyClassName?: string;
    children: ReactNode;
}

/** A screen's title, wherever it stands. */
export function PageTitle({ children }: { children: ReactNode }) {
    return <h1 className="type-display text-bone">{children}</h1>;
}

/** The line under a title, at a reading measure. */
export function PageLead({ children }: { children: ReactNode }) {
    return <p className="type-body mt-1 max-w-[72ch] text-bone/70">{children}</p>;
}

export function Page({ title, lead, control, footer, note, bodyClassName, children }: Props) {
    return (
        <div className="screen-body flex min-h-0 flex-1 flex-col px-inset pt-4 pb-4">
            {title ? (
                <header className="flex shrink-0 items-start justify-between gap-gutter">
                    <div className="min-w-0">
                        <PageTitle>{title}</PageTitle>
                        {lead ? <PageLead>{lead}</PageLead> : null}
                    </div>
                    {control ? <div className="flex h-8 shrink-0 items-center">{control}</div> : null}
                </header>
            ) : null}
            <div className={cn('grid-12 min-h-0 flex-1', title && 'mt-4', bodyClassName)}>{children}</div>
            {footer || note ? (
                <footer className="flex h-14 shrink-0 items-center justify-end gap-3">
                    {note ? <span className="type-caption mr-auto line-clamp-2 min-w-0 text-bone/55">{note}</span> : null}
                    {footer}
                </footer>
            ) : null}
        </div>
    );
}
