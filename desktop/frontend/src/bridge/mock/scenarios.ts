// The states the window can be in, each as the connector would bring it
// about: an opening position for the page and a timed script of events.
// The scenario panel (src/dev) lists them; `?scenario=<id>` opens one.

import type { AppState, Platform, Step } from '../store';
import { CODE_TTL_MS, type ConnectorEvent, type Descriptor, type Device, type Unit } from '../types';
import { CODE, cameras, descriptorFor, disconnected, enclave, expiresIn, faceThrough, hello, mics, shareArriving, shareListening, shareOffered, stateDirs, units } from './fixtures';

export interface Timed {
    /** Milliseconds after the page subscribes. */
    at: number;
    event: ConnectorEvent;
}

export interface Scenario {
    id: string;
    group: 'Pair' | 'Cameras and microphone' | 'Unit' | 'Ready' | 'Blocked';
    title: string;
    /** What the scenario shows, for the panel. */
    note?: string;
    platform?: Platform;
    step: Step;
    /** Which view the Cameras screen opens on. */
    tab?: 'behind' | 'face';
    setupDone?: boolean;
    aboutOpen?: boolean;
    script: Timed[];
}

const defaultCameras: Device[] = [cameras.insta!, cameras.facetime!];
const defaultMics: Device[] = [mics.yeti!, mics.macbook!];

const SHAPE = '1280x720 30 fps, h264_videotoolbox';

function source(camera: Device, mic: Device | null, over: Partial<Extract<ConnectorEvent, { type: 'source' }>> = {}): ConnectorEvent {
    return {
        type: 'source',
        kind: 'capture',
        label: mic ? `${camera.name} + ${mic.name}` : camera.name,
        ready: true,
        shape: SHAPE,
        camera: camera.name,
        mic: mic?.name ?? 'none',
        // A current connector offers the phone's picture and, until asked, no face camera: the phone's own is the face.
        share: shareOffered,
        face: null,
        ...over,
    };
}

const obsCameras: Device[] = [cameras.obs!, cameras.insta!, cameras.facetime!];
const stats = (videoBps: number) => ({ videoBps, audioBps: 64_000, congested: false, backlogS: 0.2 });

function devices(cams: Device[], ms: Device[], substitutions: Extract<ConnectorEvent, { type: 'devices' }>['substitutions'] = []): ConnectorEvent {
    return { type: 'devices', cameras: cams, mics: ms, substitutions };
}

function unitsEvent(list: Unit[], scanning = false): ConnectorEvent {
    return { type: 'units', units: list, scanning };
}

function device(descriptor: Descriptor): ConnectorEvent {
    return { type: 'device', descriptor };
}

/** A code with `minutes` of its ten-minute life left (the service's CODE_TTL_MS). */
const code = (minutes: number, value = CODE): ConnectorEvent => ({ type: 'code', code: value, expiresAt: expiresIn(minutes), ttlMs: CODE_TTL_MS });

/** The connector's first moments: hello, the service answering, the devices, the camera chosen, the units seen. */
function opening(over: { phones?: number; platform?: Platform; cams?: Device[]; mics?: Device[]; src?: ConnectorEvent; units?: Unit[]; unit?: Descriptor | null; updates?: 'on' | 'off'; updatesNote?: string } = {}): Timed[] {
    const platform = over.platform ?? 'darwin';
    const list = over.units ?? [units.g12ab!];
    const unit = over.unit === undefined ? descriptorFor(units.g12ab!) : over.unit;
    const script: Timed[] = [
        { at: 0, event: { type: 'hello', hello: hello({ phones: over.phones ?? 0, stateDir: stateDirs[platform], updates: over.updates ?? 'on', updatesNote: over.updatesNote }) } },
        { at: 250, event: devices(over.cams ?? defaultCameras, over.mics ?? defaultMics) },
        { at: 300, event: over.src ?? source(cameras.insta!, mics.yeti!) },
        { at: 700, event: { type: 'online', online: true } },
        { at: 1200, event: unitsEvent(list, false) },
    ];
    if (unit) script.push({ at: 1500, event: device(unit) });
    return script;
}

export const scenarios: Scenario[] = [
    // Pair
    {
        id: 'pair-fresh',
        group: 'Pair',
        title: 'First run: the code',
        note: 'Nothing paired yet; the service has sent a code good for nine minutes.',
        step: 'pair',
        script: [...opening(), { at: 900, event: code(9) }],
    },
    {
        id: 'pair-expiring',
        group: 'Pair',
        title: 'Code about to expire',
        note: 'Forty-five seconds left: the ring and the code turn ember; the fresh code arrives when this one lapses.',
        step: 'pair',
        script: [...opening(), { at: 900, event: code(0.75) }, { at: 47_000, event: code(10, 'M8RD3TWK') }],
    },
    {
        id: 'pair-offline',
        group: 'Pair',
        title: 'Cannot reach masseuse.ai yet',
        note: 'No code until the service answers; it does after six seconds.',
        step: 'pair',
        script: [
            { at: 0, event: { type: 'hello', hello: hello() } },
            { at: 250, event: devices(defaultCameras, defaultMics) },
            { at: 300, event: source(cameras.insta!, mics.yeti!) },
            { at: 800, event: { type: 'online', online: false } },
            { at: 6000, event: { type: 'online', online: true } },
            { at: 6200, event: code(10) },
            { at: 6500, event: unitsEvent([units.g12ab!]) },
        ],
    },
    {
        id: 'pair-just-paired',
        group: 'Pair',
        title: 'A phone pairs, on to Cameras',
        note: 'The code is typed into the phone three seconds in; the page moves on to the camera.',
        step: 'pair',
        script: [...opening(), { at: 900, event: code(9) }, { at: 3000, event: { type: 'paired', phones: 1 } }],
    },
    {
        id: 'pair-another',
        group: 'Pair',
        title: 'Pair another phone (already paired)',
        note: 'Reached from Ready: two phones paired, a code for a third.',
        step: 'pair',
        setupDone: true,
        script: [...opening({ phones: 2 }), { at: 900, event: code(9) }],
    },
    {
        id: 'launch-paired',
        group: 'Pair',
        title: 'Launch on a paired computer',
        note: 'The page starts on Pair as any launch does; the hello says a phone is paired, and the window opens on Ready with every step a place to change a choice.',
        step: 'pair',
        script: [...opening({ phones: 1 }), { at: 900, event: code(9) }],
    },

    // Camera and microphone
    {
        id: 'camera-normal',
        group: 'Cameras and microphone',
        title: 'Two cameras, two microphones',
        step: 'camera',
        script: opening({ phones: 1 }),
    },
    {
        id: 'camera-obs',
        group: 'Cameras and microphone',
        title: 'OBS Virtual Camera present and chosen',
        note: 'The virtual camera is what OBS outputs; the hint says to start it there.',
        step: 'camera',
        script: opening({
            phones: 1,
            cams: [cameras.obs!, cameras.insta!, cameras.facetime!, cameras.iphone!],
            mics: [mics.yeti!, mics.macbook!, mics.iphone!],
            src: source(cameras.obs!, mics.yeti!),
        }),
    },
    {
        id: 'camera-remembered-missing',
        group: 'Cameras and microphone',
        title: 'Remembered camera not connected',
        note: 'The Insta360 Link is remembered; the FaceTime camera stands in for the day it is back.',
        step: 'camera',
        script: opening({
            phones: 1,
            cams: [cameras.facetime!],
            mics: [mics.macbook!],
            src: source(cameras.facetime!, mics.macbook!),
        }).map((t) =>
            t.event.type === 'devices' ? { ...t, event: devices([cameras.facetime!], [mics.macbook!], [{ kind: 'video', wanted: 'Insta360 Link', using: 'FaceTime HD Camera' }, { kind: 'audio', wanted: 'Yeti Stereo Microphone', using: 'MacBook Pro Microphone' }]) } : t,
        ),
    },
    {
        id: 'camera-none',
        group: 'Cameras and microphone',
        title: 'No camera found',
        note: 'Pairing still works; the session is guided without a camera until one is plugged in.',
        step: 'camera',
        script: opening({
            phones: 1,
            cams: [],
            mics: [mics.macbook!],
            src: { type: 'source', kind: 'capture', label: "This computer's camera", ready: false, note: 'no camera found', share: shareOffered, face: null },
        }),
    },
    {
        id: 'camera-locked',
        group: 'Cameras and microphone',
        title: 'A session has the camera',
        note: 'While the camera is on for a session the choice waits.',
        step: 'camera',
        setupDone: true,
        script: [
            ...opening({ phones: 1 }),
            { at: 1600, event: { type: 'link', state: 'active', enclave } },
            { at: 1800, event: { type: 'camera', on: true, stats: { videoBps: 2_100_000, audioBps: 64_000, congested: false, backlogS: 0.2 } } },
        ],
    },
    {
        id: 'camera-network',
        group: 'Cameras and microphone',
        title: 'A camera on the network',
        note: 'RTSPS from a home camera instead of the computer’s own.',
        step: 'camera',
        setupDone: true,
        script: opening({
            phones: 1,
            src: { type: 'source', kind: 'camera', label: 'rtsps://192.168.1.20:322/live', ready: true, shape: 'h264 video, aac audio', url: 'rtsps://camera:••••••••@192.168.1.20:322/live', share: shareOffered, face: null },
        }),
    },

    // Your face
    {
        id: 'face-obs-setup',
        group: 'Cameras and microphone',
        title: 'Your face: OBS Studio, setting up',
        note: 'Chosen and applied; the address is up, the phone has not switched it on, and no virtual camera is found yet.',
        step: 'camera',
        tab: 'face',
        setupDone: true,
        script: opening({
            phones: 1,
            cams: [cameras.insta!, cameras.facetime!],
            src: source(cameras.insta!, mics.yeti!, { share: shareListening, face: { label: 'OBS Virtual Camera', camera: 'OBS Virtual Camera', ready: false, note: 'OBS Virtual Camera is not connected' } }),
        }),
    },
    {
        id: 'face-obs-ready',
        group: 'Cameras and microphone',
        title: 'Your face: OBS Studio, ready',
        note: 'The phone’s picture is arriving; OBS Virtual Camera is found and chosen; the face camera comes on when the room reads it.',
        step: 'camera',
        tab: 'face',
        setupDone: true,
        script: opening({
            phones: 1,
            cams: obsCameras,
            src: source(cameras.insta!, mics.yeti!, { share: shareArriving, face: faceThrough(cameras.obs!) }),
        }),
    },
    {
        id: 'face-obs-same-device',
        group: 'Cameras and microphone',
        title: 'Your face: the face camera is the camera behind you',
        note: 'The chosen face camera is also sending the view behind you; it is greyed in the list.',
        step: 'camera',
        tab: 'face',
        setupDone: true,
        script: opening({
            phones: 1,
            cams: obsCameras,
            src: source(cameras.obs!, mics.yeti!, { share: shareListening, face: null }),
        }),
    },
    {
        id: 'face-obs-locked',
        group: 'Cameras and microphone',
        title: 'Your face: the room is showing OBS’s picture',
        note: 'Both directions live during a session; the choice waits.',
        step: 'camera',
        tab: 'face',
        setupDone: true,
        script: [
            ...opening({ phones: 1, cams: obsCameras, src: source(cameras.insta!, mics.yeti!, { share: shareArriving, face: faceThrough(cameras.obs!) }) }),
            { at: 1600, event: { type: 'link', state: 'active', enclave } },
            { at: 1800, event: { type: 'camera', on: true, stats: stats(2_100_000) } },
            { at: 2000, event: { type: 'face', on: true, stats: stats(1_800_000) } },
        ],
    },
    {
        id: 'face-older-connector',
        group: 'Cameras and microphone',
        title: 'Your face: an older connector',
        note: 'The connector’s report has no share or face; the tab says the phone’s camera is the face.',
        step: 'camera',
        tab: 'face',
        setupDone: true,
        script: opening({ phones: 1 }).map((t) => {
            if (t.event.type !== 'source') return t;
            const { share: _share, face: _face, ...rest } = t.event;
            return { ...t, event: rest };
        }),
    },
    {
        id: 'face-obs-live',
        group: 'Ready',
        title: 'Session active, face through OBS Studio',
        note: 'The loop is live: the phone’s picture arrives, OBS’s picture goes back and is shown as the face.',
        step: 'home',
        setupDone: true,
        script: [
            ...opening({ phones: 1, cams: obsCameras, src: source(cameras.insta!, mics.yeti!, { share: shareArriving, face: faceThrough(cameras.obs!) }), unit: descriptorFor(units.g12ab!, { armed: { levelBound: 15 }, status: { batteryPercent: 82, mode: 7, levelA: 6, outputting: true } }) }),
            { at: 1800, event: { type: 'link', state: 'active', enclave } },
            { at: 2000, event: { type: 'camera', on: true, stats: stats(2_100_000) } },
            { at: 2600, event: { type: 'face', on: true, stats: stats(1_750_000) } },
        ],
    },

    // Unit
    {
        id: 'unit-scanning',
        group: 'Unit',
        title: 'Looking for units',
        note: 'Nothing within reach yet; the list refreshes every half minute.',
        step: 'unit',
        script: opening({ phones: 1, units: [], unit: null }).map((t) => (t.event.type === 'units' ? { ...t, event: unitsEvent([], true) } : t)),
    },
    {
        id: 'unit-one',
        group: 'Unit',
        title: 'One unit, connected',
        step: 'unit',
        script: opening({ phones: 1 }),
    },
    {
        id: 'unit-several',
        group: 'Unit',
        title: 'Several units within reach',
        note: 'Two Mastago units over Bluetooth and an MK-312BT over USB serial; the first is served.',
        step: 'unit',
        script: opening({ phones: 1, units: [units.g12ab!, units.g34cd!, units.mk312!] }),
    },
    {
        id: 'unit-held',
        group: 'Unit',
        title: 'Another program has the unit open',
        note: 'The connector shares the link and warns before a session.',
        step: 'unit',
        script: opening({ phones: 1, units: [units.g12ab!, units.g34cdHeld!], unit: descriptorFor(units.g34cdHeld!) }),
    },
    {
        id: 'unit-bluetooth-permission',
        group: 'Unit',
        title: 'Bluetooth permission needed (macOS)',
        step: 'unit',
        script: [...opening({ phones: 1, units: [], unit: null }), { at: 1300, event: { type: 'bluetooth', state: 'permission' } }],
    },
    {
        id: 'unit-bluetooth-off',
        group: 'Unit',
        title: 'Bluetooth is off',
        platform: 'windows',
        step: 'unit',
        script: [...opening({ phones: 1, platform: 'windows', units: [], unit: null }), { at: 1300, event: { type: 'bluetooth', state: 'off' } }],
    },
    {
        id: 'unit-idle-off',
        group: 'Unit',
        title: 'Unit switched itself off (idle)',
        step: 'unit',
        script: [...opening({ phones: 1 }), { at: 3000, event: device(disconnected(units.g12ab!, 'idle-off')) }],
    },
    {
        id: 'unit-battery-off',
        group: 'Unit',
        title: 'Unit switched itself off (battery)',
        step: 'unit',
        script: [...opening({ phones: 1 }), { at: 3000, event: device(disconnected(units.g12ab!, 'battery-off')) }],
    },
    {
        id: 'unit-armed',
        group: 'Unit',
        title: 'A session has the unit armed',
        note: 'Switching units waits until the phone stops the unit.',
        step: 'unit',
        setupDone: true,
        script: [
            ...opening({ phones: 1, units: [units.g12ab!, units.g34cd!], unit: descriptorFor(units.g12ab!, { armed: { levelBound: 15 }, status: { batteryPercent: 82, mode: 7, levelA: 6, outputting: true } }) }),
            { at: 1600, event: { type: 'link', state: 'active', enclave } },
            { at: 1800, event: { type: 'camera', on: true, stats: { videoBps: 2_100_000, audioBps: 64_000, congested: false, backlogS: 0.2 } } },
        ],
    },

    // Ready
    {
        id: 'home-idle',
        group: 'Ready',
        title: 'Ready, waiting for a session',
        step: 'home',
        setupDone: true,
        script: opening({ phones: 1 }),
    },
    {
        id: 'home-session',
        group: 'Ready',
        title: 'Session active',
        note: 'The camera link is up to the verified enclave; the unit is armed within its bound.',
        step: 'home',
        setupDone: true,
        script: [
            ...opening({ phones: 1, unit: descriptorFor(units.g12ab!, { armed: { levelBound: 15 }, status: { batteryPercent: 82, mode: 7, levelA: 6, outputting: true } }) }),
            { at: 1800, event: { type: 'link', state: 'active', enclave } },
            { at: 2000, event: { type: 'camera', on: true, stats: { videoBps: 2_100_000, audioBps: 64_000, congested: false, backlogS: 0.2 } } },
            { at: 4000, event: { type: 'camera', on: true, stats: { videoBps: 2_300_000, audioBps: 64_000, congested: false, backlogS: 0.3 } } },
            { at: 6000, event: { type: 'camera', on: true, stats: { videoBps: 2_050_000, audioBps: 63_000, congested: false, backlogS: 0.1 } } },
        ],
    },
    {
        id: 'home-congested',
        group: 'Ready',
        title: 'Session active, connection congested',
        step: 'home',
        setupDone: true,
        script: [
            ...opening({ phones: 1 }),
            { at: 1800, event: { type: 'link', state: 'active', enclave } },
            { at: 2000, event: { type: 'camera', on: true, stats: { videoBps: 1_600_000, audioBps: 64_000, congested: true, backlogS: 1.4 } } },
            { at: 2100, event: { type: 'notice', level: 'warn', text: 'Connection cannot keep up: video now 1.6 Mb/s.' } },
        ],
    },
    {
        id: 'home-no-picture',
        group: 'Ready',
        title: 'Session active, the camera delivers no picture',
        note: 'ffmpeg runs but never publishes, as when macOS has refused the camera; the meters sit at zero with the connector’s reason, said once as a notice.',
        step: 'home',
        setupDone: true,
        script: [
            ...opening({ phones: 1 }),
            { at: 1800, event: { type: 'link', state: 'active', enclave } },
            { at: 2000, event: { type: 'camera', on: true } },
            { at: 3000, event: { type: 'camera', on: true, stats: { videoBps: 0, audioBps: 0, congested: false, backlogS: 0 } } },
            {
                at: 4500,
                event: {
                    type: 'camera',
                    on: true,
                    stats: { videoBps: 0, audioBps: 0, congested: false, backlogS: 0, reason: 'the camera delivered no picture in 10 s; macOS may have refused it: System Settings › Privacy & Security › Camera, and Microphone, must list Masseuse and allow it' },
                },
            },
            { at: 4600, event: { type: 'notice', level: 'warn', text: 'Camera on but not sending yet: the camera delivered no picture in 10 s; macOS may have refused it: System Settings › Privacy & Security › Camera, and Microphone, must list Masseuse and allow it.' } },
        ],
    },
    {
        id: 'home-on-hold',
        group: 'Ready',
        title: 'Camera link on hold',
        note: 'The session moved on; the next "Use this camera" brings a new ticket.',
        step: 'home',
        setupDone: true,
        script: [...opening({ phones: 1 }), { at: 1800, event: { type: 'link', state: 'on-hold', reason: 'the enclave is not expecting this connector; waiting for the service' } }],
    },
    {
        id: 'home-update-staged',
        group: 'Ready',
        title: 'Update downloaded, installing later',
        step: 'home',
        setupDone: true,
        script: [
            ...opening({ phones: 1 }),
            { at: 1800, event: { type: 'link', state: 'active', enclave } },
            { at: 2000, event: { type: 'camera', on: true, stats: { videoBps: 2_100_000, audioBps: 64_000, congested: false, backlogS: 0.2 } } },
            { at: 2500, event: { type: 'update', state: 'staged', tag: 'v0.13.1', text: 'Update: Masseuse.ai v0.13.1 downloaded and verified; installing when the session ends.' } },
        ],
    },
    {
        id: 'home-updates-off',
        group: 'Ready',
        title: 'Updates off (a build from a working tree)',
        step: 'home',
        setupDone: true,
        script: [...opening({ phones: 1, updates: 'off', updatesNote: 'this is not a release build' }), { at: 1600, event: { type: 'update', state: 'off', text: 'Updates are off: this is not a release build.' } }],
    },
    {
        id: 'home-no-unit',
        group: 'Ready',
        title: 'Ready without a unit',
        note: 'The person skipped the unit; the session is guided without one.',
        step: 'home',
        setupDone: true,
        script: opening({ phones: 1, units: [], unit: null }),
    },
    {
        id: 'home-windows',
        group: 'Ready',
        title: 'Ready, on Windows',
        note: 'The native title bar and menu bar sit above the page; no inset for traffic lights.',
        platform: 'windows',
        step: 'home',
        setupDone: true,
        script: opening({ phones: 1, platform: 'windows', src: source(cameras.logitech!, mics.brio!, { shape: '1280x720 30 fps, h264_mf' }) }),
    },

    {
        id: 'about',
        group: 'Ready',
        title: 'About dialog',
        note: 'Opened from the application menu (About Masseuse.ai).',
        step: 'home',
        setupDone: true,
        aboutOpen: true,
        script: opening({ phones: 1 }),
    },

    // Blocked
    {
        id: 'blocked-already-running',
        group: 'Blocked',
        title: 'Already running in another window',
        step: 'home',
        script: [{ at: 0, event: { type: 'blocked', kind: 'already-running' } }],
    },
    {
        id: 'blocked-connector-stopped',
        group: 'Blocked',
        title: 'The connector stopped',
        step: 'home',
        script: [
            ...opening({ phones: 1 }),
            {
                at: 2500,
                event: {
                    type: 'blocked',
                    kind: 'connector-stopped',
                    detail: 'time=07:41:12 level=ERROR msg="rendezvous stopped" err="Get \\"https://masseuse.ai/api/camlink/hello\\": dial tcp: lookup masseuse.ai: no such host"\nStopped.',
                },
            },
        ],
    },
    {
        id: 'blocked-state-dir',
        group: 'Blocked',
        title: 'The state folder cannot be written',
        platform: 'windows',
        step: 'home',
        script: [{ at: 0, event: { type: 'blocked', kind: 'state-dir-unwritable', detail: 'mkdir C:\\Users\\you\\AppData\\Local\\masseuse-camlink: access is denied' } }],
    },
];

export const DEFAULT_SCENARIO = 'pair-fresh';

export function findScenario(id: string | null | undefined): Scenario {
    return scenarios.find((s) => s.id === id) ?? scenarios.find((s) => s.id === DEFAULT_SCENARIO)!;
}

/** The opening position a scenario asks for, over the state's defaults. */
export function scenarioState(scenario: Scenario, base: AppState, platform?: Platform): AppState {
    const p = platform ?? scenario.platform ?? 'darwin';
    return {
        ...base,
        platform: p,
        shell: { ...base.shell, stateDir: stateDirs[p] },
        step: scenario.step,
        cameraTab: scenario.tab ?? 'behind',
        setupDone: scenario.setupDone ?? false,
        aboutOpen: scenario.aboutOpen ?? false,
    };
}
