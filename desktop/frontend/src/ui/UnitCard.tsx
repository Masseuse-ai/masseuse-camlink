// A stimulation unit within reach as a choice: the whole card is the
// radio. The row names the unit itself, its maker and model, and not the
// identifier the helper's label carries after it (a serial port's base
// name, a Bluetooth unit's short id): that is what tells two units of one
// family apart and is shown only then. The link it is on shows as an icon
// and a word (Bluetooth for the Mastago family, a cable for a serial
// helper's unit), its standing as badges.

import { Bluetooth, Cable } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import { RadioGroupItem } from '@/components/ui/radio-group';
import { cn } from '@/lib/utils';
import type { Descriptor, Unit } from '../bridge/types';
import { CHOICE_ROW, CHOICE_ROW_DISABLED } from './Card';

/** Whether a unit is reached over a serial cable, by its id or family. */
export function isSerial(unit: Pick<Unit, 'kind'> & { id?: string }): boolean {
    const id = unit.id ?? '';
    return id.startsWith('serial:') || /^(\/dev\/|COM\d)/.test(id) || unit.kind === 'mk312bt' || unit.kind === 'estim-2b';
}

/** The link a unit is on, for people. */
export function linkName(unit: Pick<Unit, 'kind'> & { id?: string }): string {
    return isSerial(unit) ? 'USB serial' : 'Bluetooth';
}

/**
 * The unit's own name for people: the maker and the model, by kind for the
 * units masseuse.ai works with (SupportedUnits), without the identifier
 * the helper's label carries after it (the serial port's base name in
 * parentheses, the Coyote's short id, the Mastago's advertised name). For a
 * family the connector does not know by name, the label less a trailing
 * parenthesis.
 */
export function unitName(kind: string, label: string): string {
    switch (kind) {
        case 'mastago':
            return 'Mastago TENS';
        case 'dglabs-coyote':
            return 'DG-Lab Coyote 3.0';
        case 'estim-2b':
            return 'E-Stim Systems 2B';
        case 'mk312bt':
            return 'ErosTek MK-312BT';
        default: {
            const stripped = label.replace(/\s*\([^()]*\)\s*$/, '').trim();
            return stripped || label;
        }
    }
}

/**
 * What tells this unit from another of its family: the parenthesized
 * suffix of the helper's label (a serial port's base name, the Coyote's
 * short id), or the Mastago's advertised name after its family's words;
 * empty when the label carries none.
 */
export function unitIdentifier(kind: string, label: string): string {
    const paren = label.match(/\(([^()]*)\)\s*$/);
    if (paren) return (paren[1] ?? '').trim();
    if (kind === 'mastago') {
        const rest = label.replace(/^Mastago TENS\s*/i, '').trim();
        return rest === label.trim() ? '' : rest;
    }
    return '';
}

interface Props {
    unit: Unit;
    serving: Descriptor | null;
    disabled?: boolean;
    /** Another unit of the same family is listed: the identifier is shown so the two read apart. */
    disambiguate?: boolean;
}

export function UnitCard({ unit, serving, disabled = false, disambiguate = false }: Props) {
    const Icon = isSerial(unit) ? Cable : Bluetooth;
    const isServing = Boolean(serving?.connected && serving.id === unit.id);
    const wentAway = Boolean(serving && !serving.connected && serving.id === unit.id && serving.reason && serving.reason !== 'let-go');
    const identifier = disambiguate ? unitIdentifier(unit.kind, unit.label) : '';
    return (
        <label className={cn(CHOICE_ROW, disabled && CHOICE_ROW_DISABLED)}>
            <RadioGroupItem value={unit.id} disabled={disabled} aria-label={unit.label} />
            <Icon className="lucide h-5 w-5 shrink-0 text-bone/80" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="type-body block truncate font-semibold tracking-tight text-bone">{unitName(unit.kind, unit.label)}</span>
                <span className="type-caption block text-bone/50">{identifier ? `${linkName(unit)} · ${identifier}` : linkName(unit)}</span>
                {/* A long word about the unit goes under its name, not beside it, so the name keeps its room. */}
                {unit.held ? (
                    <span className="mt-1.5 block">
                        <Badge variant="amber">Open in another program</Badge>
                    </span>
                ) : null}
            </span>
            <span className="flex shrink-0 items-center gap-1.5">
                {isServing ? <Badge variant="mint">Serving</Badge> : null}
                {wentAway ? <Badge variant="ember">Switched off</Badge> : null}
            </span>
        </label>
    );
}
