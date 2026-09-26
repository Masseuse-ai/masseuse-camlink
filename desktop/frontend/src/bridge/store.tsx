// The window's state: what the connector has said so far (folded in from
// its events, one reducer case per event type) and where the person is
// (the step, the About dialog). Screens read it with useAppState and act
// through useBridge; nothing here knows whether the events came from the
// connector or from the mock.

import { createContext, useContext, useEffect, useMemo, useReducer, useRef, type Dispatch, type ReactNode, type RefObject } from 'react';

import { CODE_TTL_MS, type Bridge, type CameraStats, type ConnectorEvent, type Descriptor, type Device, type EnclaveProof, type FaceCamera, type Hello, type LinkState, type MediaPermission, type Share, type Substitution, type Unit, type UpdateState } from './types';

export type Platform = 'darwin' | 'windows' | 'linux';

/** The steps of the first run, and the home they lead to. */
export type Step = 'pair' | 'camera' | 'unit' | 'home';

export interface Notice {
    id: number;
    level: 'info' | 'warn' | 'error';
    text: string;
    at: number;
}

export interface SourceState {
    kind: 'capture' | 'camera';
    label: string;
    ready: boolean;
    note?: string;
    shape?: string;
    camera?: string;
    mic?: string;
    url?: string;
    /** Absent when the connector's report has no `share` (an older release). */
    share?: Share;
    /** null is the phone's own camera; absent when the connector's report has no `face`. */
    face?: FaceCamera | null;
}

/** Which picture the room shows as the person's face. */
export type FaceView = 'phone' | 'processed';

export function faceViewOf(source: SourceState | null): FaceView {
    return source?.face ? 'processed' : 'phone';
}

/** Whether the connector's report knows the loop at all (an older release does not). */
export function offersFaceLoop(source: SourceState | null): boolean {
    return Boolean(source && source.share !== undefined && source.face !== undefined);
}

export interface AppState {
    platform: Platform;
    shell: { version: string; wired: boolean; stateDir: string };
    hello: Hello | null;
    /** null until the connector has said either way. */
    online: boolean | null;
    /** The pairing code, when it expires (ms since the epoch), how long a code lives, and when this one arrived. */
    code: { code: string; expiresAt: number; ttlMs: number; receivedAt: number } | null;
    phones: number;
    devices: { cameras: Device[]; mics: Device[]; substitutions: Substitution[] } | null;
    /** Why the devices could not be listed (no ffmpeg), when they could not. */
    devicesError: string | null;
    source: SourceState | null;
    link: { state: LinkState; reason?: string; enclave?: EnclaveProof };
    camera: { on: boolean; stats?: CameraStats };
    /** The face camera (OBS's picture going back), on only while the room reads it. */
    faceCamera: { on: boolean; stats?: CameraStats };
    units: Unit[];
    unitsScanning: boolean;
    unit: Descriptor | null;
    bluetooth: 'ok' | 'permission' | 'off';
    /** The system's standing on the camera and the microphone, the shell's word (macOS's); null until it has spoken. */
    media: { camera: MediaPermission; mic: MediaPermission } | null;
    update: { state: UpdateState; tag?: string; text: string };
    notices: Notice[];
    blocked: { kind: 'already-running' | 'connector-stopped' | 'state-dir-unwritable'; detail?: string } | null;

    /** Where the person is. */
    step: Step;
    /** Which view the Cameras screen opens on: the camera behind them, or their face. */
    cameraTab: 'behind' | 'face';
    /** The first run is done: home is the base, and the steps are reached from it to change a choice. */
    setupDone: boolean;
    aboutOpen: boolean;
    /** The system has been asked for the camera and the microphone once in this run of the page (ui/MediaAccess.tsx); the Allow button asks again. */
    mediaAsked: boolean;
}

export const initialState: AppState = {
    platform: 'darwin',
    shell: { version: '(devel)', wired: false, stateDir: '' },
    hello: null,
    online: null,
    code: null,
    phones: 0,
    devices: null,
    devicesError: null,
    source: null,
    link: { state: 'idle' },
    camera: { on: false },
    faceCamera: { on: false },
    units: [],
    unitsScanning: false,
    unit: null,
    bluetooth: 'ok',
    media: null,
    update: { state: 'current', text: '' },
    notices: [],
    blocked: null,
    step: 'pair',
    cameraTab: 'behind',
    setupDone: false,
    aboutOpen: false,
    mediaAsked: false,
};

export type Action =
    | { type: 'event'; event: ConnectorEvent }
    | { type: 'ui/go'; step: Step; tab?: 'behind' | 'face' }
    | { type: 'ui/setup-done' }
    | { type: 'ui/about'; open: boolean }
    | { type: 'ui/notice'; level: Notice['level']; text: string }
    | { type: 'ui/dismiss'; id: number }
    | { type: 'ui/shell'; shell: AppState['shell'] }
    | { type: 'ui/media-asked' }
    | { type: 'ui/reset'; state: AppState };

let noticeSeq = 1;

/** How many notices the stack keeps; older ones scroll off. */
const NOTICE_LIMIT = 4;

function pushNotice(notices: Notice[], level: Notice['level'], text: string): Notice[] {
    return [...notices, { id: noticeSeq++, level, text, at: Date.now() }].slice(-NOTICE_LIMIT);
}

export function reducer(state: AppState, action: Action): AppState {
    switch (action.type) {
        case 'ui/reset':
            return action.state;
        case 'ui/go':
            // Reaching Ready by any path ends the first run: Ready is home
            // and the steps are where a choice is changed from then on.
            return {
                ...state,
                step: action.step,
                setupDone: state.setupDone || action.step === 'home',
                cameraTab: action.tab ?? state.cameraTab,
            };
        case 'ui/setup-done':
            return { ...state, setupDone: true, step: 'home' };
        case 'ui/about':
            return { ...state, aboutOpen: action.open };
        case 'ui/notice':
            return { ...state, notices: pushNotice(state.notices, action.level, action.text) };
        case 'ui/dismiss':
            return { ...state, notices: state.notices.filter((n) => n.id !== action.id) };
        case 'ui/shell':
            return { ...state, shell: action.shell };
        case 'ui/media-asked':
            return { ...state, mediaAsked: true };
        case 'event':
            return applyEvent(state, action.event);
    }
}

function applyEvent(state: AppState, ev: ConnectorEvent): AppState {
    switch (ev.type) {
        case 'hello': {
            // A computer that is already paired opens on Ready: pairing is
            // the setup's gate, and a phone paired on an earlier run has
            // passed it. Only from the page's first state (on Pair, the
            // first run, no phone known yet): a scenario that starts
            // elsewhere stays there, and a phone paired during this run
            // (the paired event) walks the steps.
            const pristine = state.step === 'pair' && !state.setupDone && state.phones === 0;
            const paired = pristine && ev.hello.phones > 0;
            return { ...state, hello: ev.hello, phones: ev.hello.phones, ...(paired ? { setupDone: true, step: 'home' as const } : {}) };
        }
        case 'online':
            return { ...state, online: ev.online };
        case 'code': {
            const expiresAt = new Date(ev.expiresAt).getTime();
            return { ...state, code: { code: ev.code, expiresAt: Number.isNaN(expiresAt) ? 0 : expiresAt, ttlMs: ev.ttlMs ?? CODE_TTL_MS, receivedAt: Date.now() } };
        }
        case 'paired':
            // A phone pairing is the way on, not a screen: in the first run, on
            // to the camera (Pair's check turning green in the bar is the
            // confirmation); once the setup is done, back to Ready. Paired while
            // on another step, the page stays where it is.
            return { ...state, phones: ev.phones, step: state.setupDone ? 'home' : state.step === 'pair' ? 'camera' : state.step };
        case 'devices':
            return {
                ...state,
                devices: ev.error ? state.devices : { cameras: ev.cameras, mics: ev.mics, substitutions: ev.substitutions },
                devicesError: ev.error ?? null,
            };
        case 'source': {
            const { type: _type, ...source } = ev;
            return { ...state, source };
        }
        case 'link':
            return { ...state, link: { state: ev.state, reason: ev.reason, enclave: ev.enclave ?? state.link.enclave } };
        case 'camera':
            return { ...state, camera: { on: ev.on, stats: ev.on ? ev.stats : undefined } };
        case 'face':
            return { ...state, faceCamera: { on: ev.on, stats: ev.on ? ev.stats : undefined } };
        case 'units':
            return { ...state, units: ev.units, unitsScanning: ev.scanning };
        case 'device':
            return { ...state, unit: ev.descriptor };
        case 'bluetooth':
            return { ...state, bluetooth: ev.state };
        case 'media':
            return { ...state, media: { camera: ev.camera, mic: ev.mic } };
        case 'update': {
            // The state lives in the application menu; a change worth a
            // word (downloaded, installing, refused) is said once as well.
            const said = ev.state === 'staged' || ev.state === 'installing' || ev.state === 'failed';
            return {
                ...state,
                update: { state: ev.state, tag: ev.tag, text: ev.text },
                notices: said && ev.text && ev.state !== state.update.state ? pushNotice(state.notices, ev.state === 'failed' ? 'warn' : 'info', ev.text) : state.notices,
            };
        }
        case 'notice':
            return { ...state, notices: pushNotice(state.notices, ev.level, ev.text) };
        case 'blocked':
            return { ...state, blocked: { kind: ev.kind, detail: ev.detail } };
    }
}

/** The screen the state calls for. */
export type Screen = Step | 'blocked';

export function screenOf(state: AppState): Screen {
    return state.blocked ? 'blocked' : state.step;
}

interface StoreValue {
    state: AppState;
    dispatch: Dispatch<Action>;
    bridge: Bridge;
    /** When a device listing was asked for and not yet answered (ms since the epoch), 0 when none is outstanding. */
    listingAsked: RefObject<number>;
}

/** How long an unanswered listing blocks the next ask: past this the connector is asked again. */
const LISTING_TIMEOUT_MS = 15_000;

const StoreContext = createContext<StoreValue | null>(null);

interface ProviderProps {
    bridge: Bridge;
    initial: AppState;
    children: ReactNode;
}

export function StoreProvider({ bridge, initial, children }: ProviderProps) {
    const [state, dispatch] = useReducer(reducer, initial);
    const listingAsked = useRef(0);

    // A new bridge (the scenario panel swapping scenarios) starts the state
    // over; the reducer's initial state is only read once, so it is reset
    // by hand.
    useEffect(() => {
        dispatch({ type: 'ui/reset', state: initial });
        listingAsked.current = 0;
        return bridge.subscribe((event) => {
            // The connector's answer to list_devices: the next ask may go.
            if (event.type === 'devices') listingAsked.current = 0;
            dispatch({ type: 'event', event });
        });
    }, [bridge, initial]);

    const value = useMemo(() => ({ state, dispatch, bridge, listingAsked }), [state, bridge]);
    return <StoreContext.Provider value={value}>{children}</StoreContext.Provider>;
}

function useStore(): StoreValue {
    const value = useContext(StoreContext);
    if (!value) throw new Error('StoreProvider is missing');
    return value;
}

export function useAppState(): AppState {
    return useStore().state;
}

export function useDispatch(): Dispatch<Action> {
    return useStore().dispatch;
}

/**
 * Asks the connector for the cameras and microphones it can open; the
 * answer arrives as a `devices` event. The connector lists them only when
 * asked (cmd/masseuse-camlink/ipc.go, list_devices), so this asks once the
 * connector has said hello, and, while `live` and the window is visible,
 * every ten seconds, so a camera plugged in or a virtual camera started
 * appears on its own. Each ask has the connector run ffmpeg's device
 * enumeration, so one ask is outstanding at a time, across every caller of
 * this hook: not until the answer has arrived (or the timeout has passed)
 * is the connector asked again. Nothing is asked while the connector is
 * stopped, and a refusal is not a notice: the list simply stays as it was.
 */
export function useDeviceListing(live: boolean, intervalMs = 10_000): void {
    const { state, bridge, listingAsked } = useStore();
    const { hello, blocked } = state;
    useEffect(() => {
        if (!hello || blocked) return;
        const ask = () => {
            if (document.visibilityState !== 'visible') return;
            const now = Date.now();
            if (listingAsked.current && now - listingAsked.current < LISTING_TIMEOUT_MS) return;
            listingAsked.current = now;
            bridge.send({ type: 'list_devices' }).catch(() => {
                listingAsked.current = 0;
            });
        };
        ask();
        if (!live) return;
        const timer = setInterval(ask, intervalMs);
        document.addEventListener('visibilitychange', ask);
        return () => {
            clearInterval(timer);
            document.removeEventListener('visibilitychange', ask);
        };
    }, [hello, blocked, live, bridge, intervalMs, listingAsked]);
}

/** The connector, to ask things of; a refusal becomes a notice. */
export function useBridge() {
    const { bridge, dispatch } = useStore();
    return useMemo(
        () => ({
            send: async (command: Parameters<Bridge['send']>[0]) => {
                try {
                    await bridge.send(command);
                } catch (err) {
                    dispatch({ type: 'ui/notice', level: 'error', text: err instanceof Error ? err.message : String(err) });
                }
            },
        }),
        [bridge, dispatch],
    );
}
