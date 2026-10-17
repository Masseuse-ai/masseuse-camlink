// A camera or microphone as a choice: the whole card is the radio. What
// kind of device it is shows as a badge worked out from its name, since
// that is all the listing gives: built in, USB, a virtual camera (OBS and
// the like) or microphone (a sound program's), or an iPhone joined through
// Continuity Camera.

import { Camera, Layers, Mic, MicOff, Smartphone, type LucideIcon } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import { RadioGroupItem } from '@/components/ui/radio-group';
import { cn } from '@/lib/utils';
import type { Device } from '../bridge/types';
import { CHOICE_ROW, CHOICE_ROW_DISABLED, ROW } from './Card';

export type DeviceNature = 'built-in' | 'usb' | 'virtual' | 'continuity';

/**
 * The names of virtual devices: cameras that show what a program outputs,
 * and microphones that carry a sound program's sound (BlackHole, Loopback,
 * Zoom's, Teams', VB-Audio's cable and VoiceMeeter, Krisp, Voicemod, Steam
 * and NVIDIA Broadcast; Windows' Stereo Mix records what the computer
 * plays). The connector's are the same (virtualCameraName and
 * virtualMicName in internal/capture/devices.go, which pass such a device
 * over when they pick one by default): the two change together.
 */
const VIRTUAL_CAMERA = /(obs|virtual|camo|snap camera|manycam|ndi)/;
const VIRTUAL_MIC = /(virtual|blackhole|soundflower|loopback|zoomaudiodevice|teams audio|vb-audio|voicemeeter|krisp|voicemod|steam streaming|nvidia broadcast|stereo mix|camo|ndi webcam)/;

/**
 * What a device is, by its name; what the listing calls it is all there is.
 * The built-in names are the Mac's (FaceTime, MacBook) and the ones Windows
 * gives the sound chip and the camera on the board (Realtek, Microphone
 * Array, High Definition Audio, Conexant, Intel Smart Sound, Integrated).
 */
export function natureOf(device: Device): DeviceNature {
    const n = device.name.toLowerCase();
    if ((device.kind === 'audio' ? VIRTUAL_MIC : VIRTUAL_CAMERA).test(n)) return 'virtual';
    if (/(iphone|ipad)/.test(n)) return 'continuity';
    if (/(facetime|built-in|built in|macbook|internal|integrated|realtek|microphone array|high definition audio|conexant|intel smart sound)/.test(n)) return 'built-in';
    return 'usb';
}

export function isVirtualCamera(device: Device | undefined): boolean {
    return Boolean(device && device.kind === 'video' && natureOf(device) === 'virtual');
}

export function isVirtualMicrophone(device: Device | undefined): boolean {
    return Boolean(device && device.kind === 'audio' && natureOf(device) === 'virtual');
}

/** The device the connector takes when none is named (defaultDevice in internal/capture/devices.go): the first that is neither virtual nor a phone's, else the first not a phone's, else the first. */
export function defaultOf(devices: Device[]): Device | undefined {
    return devices.find((d) => !['virtual', 'continuity'].includes(natureOf(d))) ?? devices.find((d) => natureOf(d) !== 'continuity') ?? devices[0];
}

const NATURE: Record<DeviceNature, { label: string; variant: 'secondary' | 'rose' | 'amber' }> = {
    'built-in': { label: 'Built in', variant: 'secondary' },
    usb: { label: 'USB', variant: 'secondary' },
    virtual: { label: 'Virtual camera', variant: 'rose' },
    continuity: { label: 'Continuity Camera', variant: 'amber' },
};

/** The badge's words: a microphone's own where they differ from a camera's. */
function badgeLabel(device: Device, nature: DeviceNature): string {
    if (device.kind === 'audio' && nature === 'virtual') return 'Virtual microphone';
    return NATURE[nature].label;
}

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
            className={cn('group/card', CHOICE_ROW, disabled && CHOICE_ROW_DISABLED)}
        >
            <RadioGroupItem value={device.name} disabled={disabled} aria-label={device.name} />
            <Icon className="lucide h-5 w-5 shrink-0 text-bone/80" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="type-body block truncate font-semibold tracking-tight text-bone">{device.name}</span>
                {note ? <span className="type-caption block text-bone/50">{note}</span> : null}
            </span>
            {/* Two badges wrap onto two lines rather than squeezing the name. */}
            <span className="flex max-w-[55%] min-w-0 shrink flex-wrap items-center justify-end gap-1.5">
                {inUse ? <Badge variant="mint">Sending now</Badge> : null}
                {device.kind === 'audio' && nature === 'usb' ? null : <Badge variant={badge.variant}>{badgeLabel(device, nature)}</Badge>}
            </span>
        </label>
    );
}

/**
 * The card for a device chosen that is not connected: greyed, not a choice
 * to make, standing for the day it is back. While it is still the choice
 * its ring holds a muted dot, the radio's chosen colour; picking another
 * device and applying replaces it. A microphone may have nothing in its
 * place: the picture goes alone.
 */
export function MissingDeviceCard({ kind, name, using, chosen = false }: { kind: Device['kind']; name: string; using: string; chosen?: boolean }) {
    const Icon = kind === 'audio' ? MicOff : Camera;
    const standIn = using ? `${using} stands in until it is back` : 'the picture goes without sound until it is back';
    return (
        <div className={cn(ROW, 'border border-dashed border-white/15 opacity-80')} aria-label={`${name}, remembered, not connected${chosen ? ', still the choice' : ''}; ${standIn}`}>
            <span className="grid size-[18px] shrink-0 place-items-center rounded-full border border-dashed border-white/25" aria-hidden>
                {chosen ? <span className="size-2 rounded-full bg-rose/60" /> : null}
            </span>
            <Icon className="lucide h-5 w-5 shrink-0 text-bone/50" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="type-body block truncate font-semibold tracking-tight text-bone/60">{name}</span>
                <span className="type-caption block text-bone/50">Remembered · not connected · {standIn}</span>
            </span>
            <Badge variant="amber">Not connected</Badge>
        </div>
    );
}

/** The last option of the microphones: none. */
export function NoMicCard({ disabled = false }: { disabled?: boolean }) {
    return (
        <label
            className={cn(CHOICE_ROW, disabled && CHOICE_ROW_DISABLED)}
        >
            <RadioGroupItem value="none" disabled={disabled} aria-label="No microphone" />
            <MicOff className="lucide h-5 w-5 shrink-0 text-bone/60" strokeWidth={2.2} />
            <span className="min-w-0 flex-1">
                <span className="type-body block font-semibold tracking-tight text-bone/80">No microphone</span>
                <span className="type-caption block text-bone/50">Video only. The session hears your phone's microphone.</span>
            </span>
        </label>
    );
}
