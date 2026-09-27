// A stimulation unit within reach as a choice: the whole card is the
// radio. The row titles the unit by its maker and model (the parts the
// driver names, `maker` and `model`; docs/PROTOCOL.md 7.3), not by the
// tag that tells two units of one family apart (a serial port's base
// name, a Bluetooth unit's short id): the tag shows only when a twin of
// the family is listed. The link it is on shows as an icon and a word
// (Bluetooth for the Mastogo family, a cable for a serial helper's unit),
// its standing as badges.

import { Bluetooth, Cable } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import { RadioGroupItem } from '@/components/ui/radio-group';
import { cn } from '@/lib/utils';
import type { Descriptor, Unit } from '../bridge/types';
import { CHOICE_ROW, CHOICE_ROW_DISABLED } from './Card';

/** The parts of a unit's name a screen composes from (internal/estim, Identity). */
export type Named = Pick<Unit, 'label'> & Partial<Pick<Unit, 'maker' | 'model' | 'tag'>>;

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
 * The model and the tag read out of a one-string label, for a connector
 * that predates the parts: a trailing parenthesis is the tag, the rest
 * the model (estim.IdentityFromLabel).
 */
function partsOfLabel(label: string): { model: string; tag: string } {
    const match = label.trim().match(/^(.*\S)\s*\(([^()]*)\)$/);
    return match ? { model: (match[1] ?? '').trim(), tag: (match[2] ?? '').trim() } : { model: label.trim(), tag: '' };
}

/** The model alone ("Coyote 3.0", "2B", "Wireless TENS"): the Ready tile's word. */
export function unitModel(unit: Named): string {
    return unit.model?.trim() || partsOfLabel(unit.label).model || unit.label;
}

/** The maker and the model ("DG-LAB Coyote 3.0"): the title on the Unit page; the model alone when the maker is not named. */
export function unitTitle(unit: Named): string {
    const model = unitModel(unit);
    const maker = unit.maker?.trim() ?? '';
    return maker && model ? `${maker} ${model}` : model || maker;
}

/** What tells this unit from another of its family; '' when the connector named none. */
export function unitTag(unit: Named): string {
    return unit.tag?.trim() || partsOfLabel(unit.label).tag;
}

interface Props {
    unit: Unit;
    serving: Descriptor | null;
    disabled?: boolean;
    /** Another unit of the same family is listed: the tag is shown so the two read apart. */
    disambiguate?: boolean;
}

export function UnitCard({ unit, serving, disabled = false, disambiguate = false }: Props) {
    const Icon = isSerial(unit) ? Cable : Bluetooth;
    const isServing = Boolean(serving?.connected && serving.id === unit.id);
    const wentAway = Boolean(serving && !serving.connected && serving.id === unit.id && serving.reason && serving.reason !== 'let-go');
    const tag = disambiguate ? unitTag(unit) : '';
    return (
        <label className={cn(CHOICE_ROW, disabled && CHOICE_ROW_DISABLED)}>
            <RadioGroupItem value={unit.id} disabled={disabled} aria-label={unit.label} />
            <Icon className="lucide h-5 w-5 shrink-0 text-bone/80" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="type-body block truncate font-semibold tracking-tight text-bone">{unitTitle(unit)}</span>
                <span className="type-caption block text-bone/50">{tag ? `${linkName(unit)} · ${tag}` : linkName(unit)}</span>
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
