// The units Masseuse.ai works with, as the masseuse.ai app lists them behind
// "Which units work?": four makers, each with its mark, the model and how it
// reaches the computer that runs Masseuse.ai. The marks name the products,
// so they show in the makers' own colours with the trademark line under them
// (src/brand/units/README.md). The connector finds any of these on its own
// once it is switched on; the list is here so a person knows theirs is one.

import dgLab from '../brand/units/dg-lab-1.svg';
import erostek from '../brand/units/erostek-1.svg';
import estimSystems from '../brand/units/estim-systems-2.svg';
import mastogo from '../brand/units/mastogo-1.svg';
import { cn } from '@/lib/utils';

export interface SupportedUnit {
    maker: string;
    model: string;
    /** How it connects to the computer that runs Masseuse.ai. */
    link: string;
    mark: string;
}

/** In the order the masseuse.ai app lists them. */
export const SUPPORTED_UNITS: readonly SupportedUnit[] = [
    { maker: 'Mastogo', model: 'Mastogo units', link: 'Bluetooth', mark: mastogo },
    { maker: 'DG-Lab', model: 'DG-Lab Coyote', link: 'Bluetooth', mark: dgLab },
    { maker: 'E-Stim Systems', model: 'E-Stim Systems 2B', link: 'Serial link cable', mark: estimSystems },
    { maker: 'ErosTek', model: 'ErosTek MK-312BT', link: 'Serial link cable', mark: erostek },
];

export const TRADEMARK_LINE = 'Mastogo, DG-Lab, E-Stim Systems and ErosTek are trademarks of their owners, who are not affiliated with masseuse.ai.';

interface Props {
    /** `list`: full rows with the marks in a well (the dialog); `compact`: the same rows, smaller (the empty state). */
    layout?: 'list' | 'compact';
    className?: string;
}

export function SupportedUnits({ layout = 'list', className }: Props) {
    const compact = layout === 'compact';
    return (
        <div className={className}>
            {/* One well the same size in every row, the mark fitted inside it,
                so the four marks read at one scale and the names start on one
                line; the widest mark (E-Stim Systems, 6:1) sets the well. */}
            <ul className={cn(compact ? 'space-y-1' : 'space-y-2')} aria-label="Units Masseuse.ai works with">
                {SUPPORTED_UNITS.map((unit) => (
                    <li key={unit.model} className={cn('flex min-w-0 items-center rounded-2xl bg-white/6 ring-1 ring-white/10', compact ? 'gap-3 p-1.5' : 'gap-3.5 p-3')}>
                        <div className={cn('grid shrink-0 place-items-center rounded-xl bg-ink px-2 ring-1 ring-white/10', compact ? 'h-9 w-[112px]' : 'h-14 w-40')}>
                            <img src={unit.mark} alt={unit.maker} className={cn('w-full object-contain', compact ? 'max-h-5' : 'max-h-10')} />
                        </div>
                        <div className="flex min-w-0 flex-1 items-baseline justify-between gap-3">
                            <p className={cn('truncate font-semibold tracking-tight text-bone', compact ? 'text-[13px]' : 'text-[15px]')}>{unit.model}</p>
                            <p className={cn('shrink-0 leading-snug text-bone/60', compact ? 'text-[11px]' : 'text-[13px]')}>{unit.link}</p>
                        </div>
                    </li>
                ))}
            </ul>
            <p className={cn('leading-snug text-bone/45', compact ? 'mt-2 text-[10px]' : 'mt-3 text-[12px]')}>{TRADEMARK_LINE}</p>
        </div>
    );
}
