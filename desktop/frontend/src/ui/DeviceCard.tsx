// A camera or microphone as a choice: the whole card is the radio. What
// kind of device it is shows as a badge worked out from its name, since
// that is all the listing gives: built in, USB, a virtual camera (OBS and
// the like), or an iPhone joined through Continuity Camera.

import { Camera, Layers, Mic, MicOff, Smartphone, type LucideIcon } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import { RadioGroupItem } from '@/components/ui/radio-group';
import { cn } from '@/lib/utils';
import type { Device } from '../bridge/types';

export type DeviceNature = 'built-in' | 'usb' | 'virtual' | 'continuity';

/**
 * What a device is, by its name; what the listing calls it is all there is.
 * The built-in names are the Mac's (FaceTime, MacBook) and the ones Windows
 * gives the sound chip and the camera on the board (Realtek, Microphone
 * Array, High Definition Audio, Conexant, Intel Smart Sound, Integrated).
 */
export function natureOf(device: Device): DeviceNature {
    const n = device.name.toLowerCase();
    if (/(obs|virtual|camo|snap camera|manycam|ndi)/.test(n)) return 'virtual';
    if (/(iphone|ipad)/.test(n)) return 'continuity';
    if (/(facetime|built-in|built in|macbook|internal|integrated|realtek|microphone array|high definition audio|conexant|intel smart sound)/.test(n)) return 'built-in';
    return 'usb';
}

export function isVirtualCamera(device: Device | undefined): boolean {
    return Boolean(device && device.kind === 'video' && natureOf(device) === 'virtual');
}

const NATURE: Record<DeviceNature, { label: string; variant: 'secondary' | 'rose' | 'amber' }> = {
    'built-in': { label: 'Built in', variant: 'secondary' },
    usb: { label: 'USB', variant: 'secondary' },
    virtual: { label: 'Virtual camera', variant: 'rose' },
    continuity: { label: 'Continuity Camera', variant: 'amber' },
};

function iconFor(device: Device, nature: DeviceNature): LucideIcon {
    if (device.kind === 'audio') return Mic;
    if (nature === 'virtual') return Layers;
    if (nature === 'continuity') return Smartphone;
    return Camera;
}

interface Props {
    device: Device;
    /** Marks the device as the one being sent right now. */
    inUse?: boolean;
    /** A line under the name: why it cannot be chosen here, what it is doing elsewhere. */
    note?: string;
    disabled?: boolean;
}

export function DeviceCard({ device, inUse = false, note, disabled = false }: Props) {
    const nature = natureOf(device);
    const Icon = iconFor(device, nature);
    const badge = NATURE[nature];
    return (
        <label
            className={cn(
                'group/card flex min-w-0 cursor-pointer items-center gap-3.5 rounded-2xl bg-white/5 px-4 py-2.5 ring-1 ring-white/10 transition-colors hover:bg-white/8 has-data-checked:bg-rose/10 has-data-checked:ring-rose/40',
                disabled && 'cursor-default opacity-60 hover:bg-white/5',
            )}
        >
            <RadioGroupItem value={device.name} disabled={disabled} aria-label={device.name} />
            <Icon className="lucide h-5 w-5 shrink-0 text-bone/80" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="block truncate text-[15px] font-semibold tracking-tight text-bone">{device.name}</span>
                {note ? <span className="block text-[12px] leading-snug text-bone/50">{note}</span> : null}
            </span>
            <span className="flex shrink-0 items-center gap-1.5">
                {inUse ? <Badge variant="mint">Sending now</Badge> : null}
                {device.kind === 'audio' && nature === 'usb' ? null : <Badge variant={badge.variant}>{device.kind === 'audio' && nature === 'built-in' ? 'Built in' : badge.label}</Badge>}
            </span>
        </label>
    );
}

/** The card for a remembered device that is not connected: greyed, not a choice, standing for the day it is back. */
export function MissingDeviceCard({ kind, name, using }: { kind: Device['kind']; name: string; using: string }) {
    const Icon = kind === 'audio' ? MicOff : Camera;
    return (
        <div className="flex min-w-0 items-center gap-3.5 rounded-2xl border border-dashed border-white/15 px-4 py-2.5 opacity-80">
            <span className="size-[18px] shrink-0 rounded-full border border-dashed border-white/25" aria-hidden />
            <Icon className="lucide h-5 w-5 shrink-0 text-bone/50" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="block truncate text-[15px] font-semibold tracking-tight text-bone/60">{name}</span>
                <span className="block text-[12px] leading-snug text-bone/50">Remembered · not connected · {using} stands in until it is back</span>
            </span>
            <Badge variant="amber">Not connected</Badge>
        </div>
    );
}

/** The last option of the microphones: none. */
export function NoMicCard({ disabled = false }: { disabled?: boolean }) {
    return (
        <label
            className={cn(
                'flex min-w-0 cursor-pointer items-center gap-3.5 rounded-2xl bg-white/5 px-4 py-2.5 ring-1 ring-white/10 transition-colors hover:bg-white/8 has-data-checked:bg-rose/10 has-data-checked:ring-rose/40',
                disabled && 'cursor-default opacity-60 hover:bg-white/5',
            )}
        >
            <RadioGroupItem value="none" disabled={disabled} aria-label="No microphone" />
            <MicOff className="lucide h-5 w-5 shrink-0 text-bone/60" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="block text-[15px] font-semibold tracking-tight text-bone/80">No microphone</span>
                <span className="block text-[12px] leading-snug text-bone/50">Video only. The session hears your phone's microphone.</span>
            </span>
        </label>
    );
}
