// A stimulation unit within reach as a choice: the whole card is the
// radio. The link it is on shows as an icon (Bluetooth for the Mastago
// family, a cable for a serial helper's unit), its standing as badges.

import { Bluetooth, Cable } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import { RadioGroupItem } from '@/components/ui/radio-group';
import { cn } from '@/lib/utils';
import type { Descriptor, Unit } from '../bridge/types';
import { CHOICE_ROW, CHOICE_ROW_DISABLED } from './Card';

/** Whether a unit is reached over a serial cable, by its id or family. */
export function isSerial(unit: Unit): boolean {
    return unit.id.startsWith('serial:') || /^(\/dev\/|COM\d)/.test(unit.id) || unit.kind === 'mk312bt';
}

/** The family's name for people, from its kind. */
export function familyName(kind: string): string {
    switch (kind) {
        case 'mastago':
            return 'Mastago TENS · Bluetooth';
        case 'mk312bt':
            return 'ErosTek MK-312BT · USB serial';
        default:
            return `${kind} · unit driver helper`;
    }
}

interface Props {
    unit: Unit;
    serving: Descriptor | null;
    disabled?: boolean;
}

export function UnitCard({ unit, serving, disabled = false }: Props) {
    const Icon = isSerial(unit) ? Cable : Bluetooth;
    const isServing = Boolean(serving?.connected && serving.id === unit.id);
    const wentAway = Boolean(serving && !serving.connected && serving.id === unit.id && serving.reason && serving.reason !== 'let-go');
    return (
        <label className={cn(CHOICE_ROW, disabled && CHOICE_ROW_DISABLED)}>
            <RadioGroupItem value={unit.id} disabled={disabled} aria-label={unit.label} />
            <Icon className="lucide h-5 w-5 shrink-0 text-bone/80" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="type-body block truncate font-semibold tracking-tight text-bone">{unit.label}</span>
                <span className="type-caption block text-bone/50">{familyName(unit.kind)}</span>
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
