// What passes between the window and the connector, in the shapes the
// connector already has (cmd/masseuse-camlink: capture.Device, estim.Unit,
// estim.Descriptor, the lines main.go prints). The connector's -ipc mode
// will serialise these same things as JSON lines (docs/DESKTOP.md); the
// mock (./mock) produces them today. `SourceChoice` is the shell's, from
// the generated bindings, so the two never drift.

import type { SourceChoice } from '../../bindings/github.com/FemLed/masseuse-camlink/desktop';

export type { SourceChoice };

/** A camera or microphone ffmpeg can open (internal/capture, Device). */
export interface Device {
    kind: 'video' | 'audio';
    /** What the listing calls it: the avfoundation index, the dshow name, the v4l2 node, the PulseAudio source. */
    id: string;
    /** What the person sees. */
    name: string;
}

/** A remembered device that is not connected, and the one standing in (capture.Substitution). */
export interface Substitution {
    kind: 'video' | 'audio';
    wanted: string;
    using: string;
}

/** A stimulation unit within reach (internal/estim, Unit). */
export interface Unit {
    id: string;
    /** The device family: "mastago" (Bluetooth), a helper's own name ("mk312bt", USB serial). */
    kind: string;
    label: string;
    /** Another program on this computer has it open. */
    held: boolean;
}

/** The unit the connector serves, found or lost (internal/estim, Descriptor). */
export interface Descriptor {
    kind: string;
    label: string;
    id?: string;
    connected: boolean;
    held?: boolean;
    /** Why it went, when it went: idle-off, output-off, battery-off, button-off, link-lost, let-go. */
    reason?: string;
    capabilities: {
        levelMax: number;
        levelMaxDefault?: number;
        channels: string[];
        modes: number[];
        tempo: boolean;
        powerModes?: string[];
        timer?: boolean;
        loadDetect?: boolean;
    };
    /** What the unit last reported (estim.Status), the parts a person reads. */
    status?: {
        batteryPercent?: number;
        power?: string;
        mode?: number;
        levelA?: number;
        levelB?: number;
        outputting?: boolean;
    };
    /** A session on the phone has the unit armed, within this bound (of levelMax). */
    armed?: { levelBound: number };
}

/** The enclave the camera streams to, as the connector verified it (main.go, policyAttester). */
export interface EnclaveProof {
    /** sha256:… of the attested image. */
    image: string;
    release: string;
    commit: string;
    /** github.com/FemLed/masseuse-video-tee@vX.Y.Z */
    source: string;
    registry: string;
    signedBy: string;
    /** The provenance came from the cache: the same digest was checked on an earlier dial. */
    cached: boolean;
}

/** The connector's first word after starting (the window's first lines today). */
export interface Hello {
    /** masseuse-camlink vX.Y.Z */
    version: string;
    /** The identity key's public part, its first characters. */
    identity: string;
    stateDir: string;
    /** "on", or why updates are off. */
    updates: 'on' | 'off';
    updatesNote?: string;
    /** The computer is held awake while this runs. */
    awake: boolean;
    awakeNote?: string;
    /** The unit drivers line: "Mastago (built in) + 1 helper: mk312". */
    drivers: string;
    /** How many phones are paired already. */
    phones: number;
}

/** How long the service lets a pairing code live: ten minutes, then a new one (masseuse.ai's rendezvous, CODE_TTL_MS). */
export const CODE_TTL_MS = 10 * 60_000;

/** The phone's picture, offered to this computer for OBS. */
export interface Share {
    /** The connector offers it (off under -no-share). */
    ready: boolean;
    /** Where OBS reads it while the connector listens: rtsp://127.0.0.1:7446/phone-… */
    address?: string;
    /** The phone's picture is arriving (the phone switched it on). */
    receiving: boolean;
}

/** The camera that carries OBS's picture back to the room. */
export interface FaceCamera {
    label: string;
    /** The device, by name: "OBS Virtual Camera". */
    camera: string;
    ready: boolean;
    note?: string;
}

export type LinkState = 'idle' | 'active' | 'on-hold' | 'closed';

export interface CameraStats {
    videoBps: number;
    audioBps: number;
    congested: boolean;
    backlogS: number;
    /**
     * Why the camera is on but nothing is being sent, when the connector can
     * say (the camera delivered no picture, ffmpeg could not open it, ...);
     * absent while sending or while it is too soon to say.
     */
    reason?: string;
}

export type UpdateState = 'current' | 'checking' | 'staged' | 'installing' | 'failed' | 'off';

/**
 * The system's standing on this computer's camera or microphone for
 * Masseuse.ai: macOS's AVAuthorizationStatus in words (desktop/permissions.go).
 * `notDetermined` has not been asked yet and asking brings the prompt;
 * `denied` was refused at the prompt or switched off in System Settings
 * since, and asking brings nothing (Privacy & Security is the way back);
 * `restricted` is a profile's or parental controls' and cannot be changed
 * here. Windows and Linux always say `authorized`.
 */
export type MediaPermission = 'notDetermined' | 'restricted' | 'denied' | 'authorized';

/** Everything the connector tells the window, and the shell's own words in the same stream (`media`, `blocked connector-stopped`). */
export type ConnectorEvent =
    | { type: 'hello'; hello: Hello }
    | { type: 'online'; online: boolean }
    | {
          type: 'code';
          code: string;
          expiresAt: string;
          /** How long a code lives from issue (the service's CODE_TTL_MS, ten minutes); the pie timer's whole. Defaults to CODE_TTL_MS. */
          ttlMs?: number;
      }
    | { type: 'paired'; phones: number }
    | { type: 'devices'; cameras: Device[]; mics: Device[]; substitutions: Substitution[]; error?: string }
    | {
          type: 'source';
          kind: 'capture' | 'camera';
          label: string;
          ready: boolean;
          /** Why it is not ready, for the person. */
          note?: string;
          /** "1280x720 30 fps, h264_videotoolbox" */
          shape?: string;
          camera?: string;
          mic?: string;
          url?: string;
          /**
           * The phone's picture offered to this computer for OBS (the source
           * report v2's `share`). Absent from an older connector's report.
           */
          share?: Share;
          /**
           * The camera that carries OBS's picture back to the room as the
           * person's face (the report's `face`); null is the phone's own
           * camera, straight to the room. Absent from an older connector.
           */
          face?: FaceCamera | null;
      }
    | {
          /** The face camera (the second capture), on only while the room reads it. */
          type: 'face';
          on: boolean;
          stats?: CameraStats;
      }
    | { type: 'link'; state: LinkState; reason?: string; enclave?: EnclaveProof }
    | { type: 'camera'; on: boolean; stats?: CameraStats }
    | { type: 'units'; units: Unit[]; scanning: boolean }
    | { type: 'device'; descriptor: Descriptor }
    | { type: 'bluetooth'; state: 'ok' | 'permission' | 'off' }
    | {
          /** The shell's word on the camera and the microphone (desktop/connector.go, reportMedia): first before the connector's hello, then whenever the standing changes, a prompt answered or a switch in System Settings. */
          type: 'media';
          camera: MediaPermission;
          mic: MediaPermission;
      }
    | { type: 'update'; state: UpdateState; tag?: string; text: string }
    | { type: 'notice'; level: 'info' | 'warn' | 'error'; text: string }
    | { type: 'blocked'; kind: 'already-running' | 'connector-stopped' | 'state-dir-unwritable'; detail?: string };

/** Everything the window asks of the connector, and of the shell in the same breath (`request_media_access`, `quit`). */
export type ConnectorCommand =
    | { type: 'list_devices' }
    | { type: 'set_source'; choice: SourceChoice }
    | { type: 'select_unit'; id: string }
    | { type: 'update_now' }
    /** The shell's: ask the system for the camera and the microphone (macOS's prompts); the answer is a `media` event. */
    | { type: 'request_media_access' }
    | { type: 'quit' };

/** The link to the connector, whichever end is behind it. */
export interface Bridge {
    /** Hands every event to `handler`; returns the unsubscribe. */
    subscribe(handler: (event: ConnectorEvent) => void): () => void;
    send(command: ConnectorCommand): Promise<void>;
}
