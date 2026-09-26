// A connector made of a script: it plays a scenario's events on their
// timers, and answers the window's commands the way the real connector
// will (a new camera chosen is reported as the source; a unit selected is
// let go of and the new one connected; an update check runs and reports),
// and stands in for the shell where the shell answers (the system's
// question about the camera and the microphone). The page cannot tell it
// from the connector, which is the point.

import type { Bridge, ConnectorCommand, ConnectorEvent, Descriptor, FaceCamera, Share, Unit } from '../types';
import { SHARE_ADDRESS, descriptorFor, disconnected, shareOffered } from './fixtures';
import type { Scenario } from './scenarios';

type Handler = (event: ConnectorEvent) => void;
type DevicesEvent = Extract<ConnectorEvent, { type: 'devices' }>;
type MediaEvent = Extract<ConnectorEvent, { type: 'media' }>;

/** How long the mock's person takes to answer the system's two prompts; long enough to see the card while they are up. */
const PROMPTS_ANSWERED_MS = 5000;

export class MockBridge implements Bridge {
    private handlers = new Set<Handler>();
    private timers: ReturnType<typeof setTimeout>[] = [];
    private started = false;

    // What was last said, so a command can answer in the same terms. The
    // devices are the last `devices` event whole (stand-ins and a listing
    // error included): the page asks for them once the hello has arrived
    // and again while the Cameras screen is open, and the answer must say
    // what the scenario says, as it stands when the answer goes out.
    private devices: DevicesEvent = { type: 'devices', cameras: [], mics: [], substitutions: [] };
    private units: Unit[] = [];
    private unit: Descriptor | null = null;
    private connectorVersion = 'v0.13.0';
    // The last source report, so a change to one view keeps the other.
    private source: Extract<ConnectorEvent, { type: 'source' }> | null = null;
    // The shell's word on the camera and the microphone, when the scenario
    // has one; null is a scenario that never speaks of it (nothing shown).
    private media: MediaEvent | null = null;

    constructor(private readonly scenario: Scenario) {}

    subscribe(handler: Handler): () => void {
        this.handlers.add(handler);
        if (!this.started) {
            this.started = true;
            for (const { at, event } of this.scenario.script) {
                this.timers.push(setTimeout(() => this.emit(event), at));
            }
        }
        return () => {
            this.handlers.delete(handler);
            if (this.handlers.size === 0) {
                for (const t of this.timers) clearTimeout(t);
                this.timers = [];
                this.started = false;
            }
        };
    }

    private emit(event: ConnectorEvent) {
        switch (event.type) {
            case 'hello':
                this.connectorVersion = event.hello.version;
                break;
            case 'devices':
                this.devices = event;
                break;
            case 'units':
                this.units = event.units;
                break;
            case 'device':
                this.unit = event.descriptor;
                break;
            case 'source':
                this.source = event;
                break;
            case 'media':
                this.media = event;
                break;
        }
        for (const h of this.handlers) h(event);
    }

    private later(ms: number, event: ConnectorEvent | (() => ConnectorEvent | null)) {
        this.timers.push(
            setTimeout(() => {
                const ev = typeof event === 'function' ? event() : event;
                if (ev) this.emit(ev);
            }, ms),
        );
    }

    async send(command: ConnectorCommand): Promise<void> {
        switch (command.type) {
            case 'list_devices':
                // Read when the answer goes out, not when it is asked: the
                // first request follows the hello at once, before the
                // scenario has listed anything.
                this.later(400, () => this.devices);
                return;

            case 'set_source': {
                const { choice } = command;
                // The face view and the phone's picture: kept from the last
                // report unless the choice names them.
                const current = this.source;
                let face: FaceCamera | null | undefined = current?.face;
                let share: Share | undefined = current?.share ?? shareOffered;
                if (choice.faceCamera !== undefined) {
                    if (choice.faceCamera === 'phone' || choice.faceCamera === '') {
                        face = null;
                    } else {
                        const carrier = this.devices.cameras.find((d) => d.name === choice.faceCamera || d.id === choice.faceCamera);
                        face = carrier
                            ? { label: carrier.name, camera: carrier.name, ready: true }
                            : { label: choice.faceCamera, camera: choice.faceCamera, ready: false, note: `${choice.faceCamera} is not connected` };
                    }
                }
                if (choice.share !== undefined && choice.share !== null) {
                    share = choice.share ? { ready: true, address: SHARE_ADDRESS, receiving: false } : { ready: true, receiving: false };
                }
                const views = { share, face };

                const bodyChanged = choice.url !== undefined || choice.camera !== undefined || choice.mic !== undefined;
                if (!bodyChanged) {
                    if (!current) throw new Error('No camera is chosen yet.');
                    this.later(400, { ...current, ...views });
                } else if (choice.url) {
                    const host = choice.url.replace(/^rtsps?:\/\/(?:[^@/]*@)?/, '').replace(/\/.*$/, '');
                    this.later(600, { type: 'source', kind: 'camera', label: `rtsps://${host}`, ready: true, shape: 'h264 video, aac audio', url: choice.url, ...views });
                    this.later(700, { type: 'notice', level: 'info', text: `Trusting camera ${host} with its certificate (saved).` });
                } else {
                    const camera = this.devices.cameras.find((d) => d.name === choice.camera || d.id === choice.camera) ?? this.devices.cameras[0];
                    if (!camera) throw new Error('No camera is connected.');
                    const mic = choice.mic === 'none' ? null : (this.devices.mics.find((d) => d.name === choice.mic || d.id === choice.mic) ?? this.devices.mics[0] ?? null);
                    this.later(500, {
                        type: 'source',
                        kind: 'capture',
                        label: mic ? `${camera.name} + ${mic.name}` : camera.name,
                        ready: true,
                        shape: '1280x720 30 fps, h264_videotoolbox',
                        camera: camera.name,
                        mic: mic?.name ?? 'none',
                        ...views,
                    });
                }
                // With the phone's picture offered and a face camera set, the
                // phone switches the loop on a few seconds later in this mock.
                if (face && share.address && !share.receiving) {
                    this.later(4500, () => (this.source ? { ...this.source, share: { ...share, receiving: true } } : null));
                }
                return;
            }

            case 'select_unit': {
                if (this.unit?.armed) {
                    throw new Error('Not now: a session on your phone is using the unit. Stop the unit on the phone first.');
                }
                const next = this.units.find((u) => u.id === command.id);
                if (!next) throw new Error(`There is no unit ${command.id} in the list.`);
                if (this.unit?.connected) {
                    const current = this.units.find((u) => u.id === this.unit?.id) ?? { id: this.unit.id ?? '', kind: this.unit.kind, label: this.unit.label, held: false };
                    this.later(200, { type: 'device', descriptor: disconnected(current, 'let-go') });
                }
                this.later(1400, { type: 'device', descriptor: descriptorFor(next) });
                return;
            }

            case 'update_now':
                this.later(100, { type: 'update', state: 'checking', text: 'Looking for a newer release…' });
                this.later(1800, { type: 'update', state: 'current', text: `Up to date: ${this.connectorVersion} is the latest release.` });
                return;

            case 'request_media_access': {
                // As macOS answers: while the standing is not determined the
                // prompts go up and the mock's person allows both a few
                // seconds later; answered already, the standing answer comes
                // back at once, no prompt. A scenario without a word on it
                // has nothing to answer.
                const current = this.media;
                if (!current) return;
                const undetermined = current.camera === 'notDetermined' || current.mic === 'notDetermined';
                const answer: MediaEvent = {
                    type: 'media',
                    camera: current.camera === 'notDetermined' ? 'authorized' : current.camera,
                    mic: current.mic === 'notDetermined' ? 'authorized' : current.mic,
                };
                this.later(undetermined ? PROMPTS_ANSWERED_MS : 100, answer);
                return;
            }

            case 'quit':
                return;
        }
    }
}
