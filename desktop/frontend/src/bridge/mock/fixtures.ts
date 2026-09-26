// The mock connector's world: the devices, units and enclave the scenarios
// (./scenarios.ts) are built from. Names are the kind ffmpeg and the
// drivers report; nothing here is a real identity or a real image digest.

import type { Descriptor, Device, EnclaveProof, FaceCamera, Hello, Share, Unit } from '../types';

export const cameras: Record<string, Device> = {
    insta: { kind: 'video', id: '0', name: 'Insta360 Link' },
    facetime: { kind: 'video', id: '1', name: 'FaceTime HD Camera' },
    obs: { kind: 'video', id: '2', name: 'OBS Virtual Camera' },
    iphone: { kind: 'video', id: '3', name: 'iPhone Camera' },
    logitech: { kind: 'video', id: '4', name: 'Logitech BRIO' },
};

export const mics: Record<string, Device> = {
    yeti: { kind: 'audio', id: '0', name: 'Yeti Stereo Microphone' },
    macbook: { kind: 'audio', id: '1', name: 'MacBook Pro Microphone' },
    iphone: { kind: 'audio', id: '2', name: 'iPhone Microphone' },
    brio: { kind: 'audio', id: '3', name: 'Logitech BRIO Microphone' },
};

export const units: Record<string, Unit> = {
    g12ab: { id: 'ble:6F1D2C3B-8A4E-4F0B-9C2D-1E2F3A4B5C6D', kind: 'mastago', label: 'Mastago TENS G-12AB', held: false },
    g34cd: { id: 'ble:0A1B2C3D-4E5F-4A6B-8C7D-9E0F1A2B3C4D', kind: 'mastago', label: 'Mastago TENS G-34CD', held: false },
    g34cdHeld: { id: 'ble:0A1B2C3D-4E5F-4A6B-8C7D-9E0F1A2B3C4D', kind: 'mastago', label: 'Mastago TENS G-34CD', held: true },
    mk312: { id: 'serial:/dev/cu.usbserial-1420', kind: 'mk312bt', label: 'MK-312BT on /dev/cu.usbserial-1420', held: false },
};

export function descriptorFor(unit: Unit, extra: Partial<Descriptor> = {}): Descriptor {
    const mastago = unit.kind === 'mastago';
    return {
        kind: unit.kind,
        label: unit.label,
        id: unit.id,
        connected: true,
        held: unit.held,
        capabilities: mastago
            ? { levelMax: 25, levelMaxDefault: 15, channels: ['A'], modes: Array.from({ length: 32 }, (_, i) => i + 1), tempo: false, timer: true, loadDetect: true }
            : { levelMax: 99, levelMaxDefault: 40, channels: ['A', 'B'], modes: Array.from({ length: 17 }, (_, i) => i + 1), tempo: true, powerModes: ['low', 'normal', 'high'] },
        status: mastago ? { batteryPercent: 82, mode: 3, levelA: 0, outputting: false } : { power: 'normal', mode: 1, levelA: 0, levelB: 0, outputting: false },
        ...extra,
    };
}

export function disconnected(unit: Unit, reason: string): Descriptor {
    return { ...descriptorFor(unit), connected: false, status: undefined, reason };
}

export const enclave: EnclaveProof = {
    image: 'sha256:9f2c1e7a4b3d5c6e8f0a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5a6',
    release: 'v0.9.3',
    commit: '4c1f9e2a7b3d',
    source: 'github.com/FemLed/masseuse-video-tee@v0.9.3',
    registry: 'ghcr.io/femled/masseuse-video-tee',
    signedBy: 'https://github.com/FemLed/masseuse-video-tee/.github/workflows/release.yml@refs/tags/v0.9.3',
    cached: false,
};

export const hello = (over: Partial<Hello> = {}): Hello => ({
    version: 'v0.13.0',
    identity: '3fK9pQ2m',
    stateDir: '/Users/you/Library/Application Support/masseuse-camlink',
    updates: 'on',
    awake: true,
    drivers: 'Mastago (built in) + 1 helper: mk312',
    phones: 0,
    ...over,
});

/** A pairing code in the alphabet the service uses (no 0, O, 1, I, L). */
export const CODE = '7QK4N2PX';

export function expiresIn(minutes: number): string {
    return new Date(Date.now() + minutes * 60_000).toISOString();
}

/** The phone's picture offered to OBS: the connector listens on loopback with a secret in the path. */
export const SHARE_ADDRESS = 'rtsp://127.0.0.1:7446/phone-q7Vn2kLp8sTzR4xM3bCw';

export const shareOffered: Share = { ready: true, receiving: false };
export const shareListening: Share = { ready: true, address: SHARE_ADDRESS, receiving: false };
export const shareArriving: Share = { ready: true, address: SHARE_ADDRESS, receiving: true };
export const shareOff: Share = { ready: false, receiving: false };

export function faceThrough(camera: Device, over: Partial<FaceCamera> = {}): FaceCamera {
    return { label: camera.name, camera: camera.name, ready: true, ...over };
}

export const stateDirs: Record<'darwin' | 'windows' | 'linux', string> = {
    darwin: '/Users/you/Library/Application Support/masseuse-camlink',
    windows: 'C:\\Users\\you\\AppData\\Local\\masseuse-camlink',
    linux: '/home/you/.local/state/masseuse-camlink',
};
