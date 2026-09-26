// Cameras and microphone: the two views the room shows. Behind you, the
// camera this computer sends (one of its own, or a camera on the network)
// and the microphone with it; and your face, the phone's own camera or,
// for people who process their picture in OBS, the loop through this
// computer (FaceView.tsx). The lists are what ffmpeg can open, as the
// connector's `devices` command prints them; a virtual camera (OBS and the
// like) is a choice like any other, with a word about where its picture
// comes from. The picture itself (size, rate, bit rate, encoder) is the
// connector's and the enclave's to manage between them; the window offers
// no say in it. On a Mac, opening this screen is when the system is asked
// whether Masseuse.ai may use the camera and the microphone (ui/MediaAccess),
// so the question comes while the person is here choosing them and not at
// the first session. On the grid (ui/Page.tsx): the two views as the
// screen's control beside the title, the two lists on six columns each, the
// way on in the footer with, in the first run, a word on what it waits for.

import { Camera as CameraIcon, ChevronRight, HouseWifi, Layers, Lock, Mic, Smartphone, TriangleAlert, Video } from 'lucide-react';
import { useEffect, useMemo, useRef, useState } from 'react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Skeleton } from '@/components/ui/skeleton';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { cn } from '@/lib/utils';
import { faceViewOf, useAppState, useBridge, useDeviceListing, useDispatch, type FaceView as FaceViewChoice } from '../bridge/store';
import type { SourceChoice } from '../bridge/types';
import { CHOICE_ROW, CHOICE_ROW_DISABLED, Card, CardLabel, ROW, Well } from '../ui/Card';
import { DeviceCard, MissingDeviceCard, NoMicCard, isVirtualCamera, natureOf } from '../ui/DeviceCard';
import { MediaAccessAlert, useMediaAsk } from '../ui/MediaAccess';
import { Page } from '../ui/Page';
import { FaceView } from './FaceView';

/** The camera list's value for a camera on the network. */
const NETWORK = 'network';

/** The shape of a device card while the connector is still listing, with a word on what is being looked for. */
function DeviceSkeleton({ children }: { children: string }) {
    return (
        <div role="status" aria-busy="true" aria-label={children}>
            <Well className={ROW}>
                <Skeleton className="h-4 w-4 shrink-0 rounded-full" />
                <Skeleton className="h-5 w-5 shrink-0 rounded-md" />
                <span className="flex min-w-0 flex-1 flex-col gap-1.5">
                    <Skeleton className="h-3.5 w-2/3" />
                    <span className="type-caption text-bone/50">{children}</span>
                </span>
            </Well>
        </div>
    );
}

export function Camera() {
    const state = useAppState();
    const dispatch = useDispatch();
    const bridge = useBridge();
    const { devices, devicesError, source, setupDone, link, camera, faceCamera, cameraTab } = state;

    const locked = camera.on || link.state === 'active';
    const tab = cameraTab;
    const setTab = (t: 'behind' | 'face') => dispatch({ type: 'ui/go', step: 'camera', tab: t });

    // The lists are the connector's answer, asked for while this screen is
    // open so a camera plugged in appears on its own; until the first answer
    // the screen says it is looking rather than that there is nothing.
    useDeviceListing(true);
    const looking = devices === null && !devicesError;
    // On a Mac, the system's question about the camera and the microphone,
    // asked as this screen opens if it has not been answered yet.
    useMediaAsk();

    // Behind you.
    const [cam, setCam] = useState<string | null>(source?.kind === 'camera' ? NETWORK : (source?.camera ?? null));
    const [mic, setMic] = useState<string | null>(source?.mic ?? null);
    const [url, setUrl] = useState(source?.url ?? '');
    const [fingerprint, setFingerprint] = useState('');
    // Your face: null until the person chooses, so the connector's report leads.
    const [faceChoice, setFaceChoice] = useState<FaceViewChoice | null>(null);
    const face: FaceViewChoice = faceChoice ?? faceViewOf(source);
    const [faceCam, setFaceCam] = useState<string | null>(source?.face?.camera ?? null);
    const [applying, setApplying] = useState(false);
    // In the first run, "Use these and continue" moves on once the
    // connector has answered with a camera it can serve: the report the
    // choice was made against is remembered, and the next one decides.
    const [continueOnAnswer, setContinueOnAnswer] = useState(false);
    const appliedFrom = useRef(source);

    // The connector's current choices are the page's starting point, and
    // follow it when the connector reports a change.
    useEffect(() => {
        if (!source) return;
        if (source.kind === 'capture') {
            setCam((c) => c ?? source.camera ?? null);
            setMic((m) => m ?? source.mic ?? null);
        } else {
            setCam((c) => c ?? NETWORK);
            if (source.url) setUrl((u) => u || source.url!);
        }
        if (source.face) setFaceCam((f) => f ?? source.face!.camera);
        setApplying(false);
    }, [source]);

    // The answer to a first-run apply: on to the unit with a camera the
    // connector can serve; without one the screen stays, saying why.
    useEffect(() => {
        if (!continueOnAnswer || source === appliedFrom.current) return;
        setContinueOnAnswer(false);
        if (source?.ready) dispatch({ type: 'ui/go', step: 'unit' });
    }, [continueOnAnswer, source, dispatch]);

    const cameras = devices?.cameras ?? [];
    const mics = devices?.mics ?? [];
    const cameraChosen = cam ?? (source?.kind === 'camera' ? NETWORK : (source?.camera ?? cameras[0]?.name ?? null));
    const micChosen = mic ?? source?.mic ?? mics[0]?.name ?? 'none';
    const chosenDevice = useMemo(() => cameras.find((d) => d.name === cameraChosen), [cameras, cameraChosen]);
    const missing = devices?.substitutions ?? [];
    const networkChosen = cameraChosen === NETWORK;
    // The virtual camera is the likely carrier for OBS's picture back; the first one is the starting point.
    const faceCamChosen = faceCam ?? source?.face?.camera ?? cameras.find((d) => natureOf(d) === 'virtual')?.name ?? null;

    // Nothing counts as changed before the connector has said what it sends.
    const behindChanged = source
        ? networkChosen
            ? url.trim() !== '' && (source.kind !== 'camera' || url !== source.url)
            : source.kind !== 'capture' || cameraChosen !== source.camera || micChosen !== (source.mic ?? 'none')
        : false;
    const faceChanged = source ? face !== faceViewOf(source) || (face === 'processed' && faceCamChosen !== (source.face?.camera ?? null)) : false;
    const faceIncomplete = face === 'processed' && !faceCamChosen;
    const faceLocked = faceCamera.on;
    const changed = (behindChanged && !locked) || (faceChanged && !faceLocked && !faceIncomplete);

    const apply = async () => {
        setApplying(true);
        const choice: SourceChoice = {};
        if (behindChanged && !locked) {
            if (networkChosen) {
                choice.url = url.trim();
                choice.fingerprint = fingerprint.trim() || undefined;
            } else {
                choice.camera = cameraChosen ?? undefined;
                choice.mic = micChosen;
            }
        }
        if (faceChanged && !faceLocked && !faceIncomplete) {
            choice.faceCamera = face === 'phone' ? 'phone' : (faceCamChosen ?? undefined);
            choice.share = face === 'processed';
        }
        appliedFrom.current = source;
        await bridge.send({ type: 'set_source', choice });
        setTimeout(() => setApplying(false), 1500);
        if (!setupDone) setContinueOnAnswer(true);
    };

    const next = () => dispatch(setupDone ? { type: 'ui/go', step: 'home' } : { type: 'ui/go', step: 'unit' });

    // The screen's one control: which of the two views is being set up.
    const control = (
        <Tabs value={tab} onValueChange={(v) => setTab(v as 'behind' | 'face')}>
            <TabsList>
                <TabsTrigger value="behind">
                    <Video /> Behind you
                </TabsTrigger>
                <TabsTrigger value="face">
                    <Smartphone /> Your face
                </TabsTrigger>
            </TabsList>
            <TabsContent value="behind" className="hidden" />
            <TabsContent value="face" className="hidden" />
        </Tabs>
    );

    // The first run moves on only with a camera the connector can serve
    // (the step is done on the same terms); there is no skipping it.
    const footer = (
        <>
            {setupDone ? (
                <Button variant="quiet" onClick={() => dispatch({ type: 'ui/go', step: 'home' })}>
                    Back to Ready
                </Button>
            ) : null}
            {changed ? (
                <Button onClick={() => void apply()} disabled={applying}>
                    {applying ? 'Applying…' : setupDone ? 'Use these' : 'Use these and continue'}
                </Button>
            ) : setupDone ? null : (
                <Button onClick={next} disabled={!source?.ready}>
                    Continue
                </Button>
            )}
        </>
    );

    return (
        <Page
            title="Cameras and microphone"
            lead={
                tab === 'behind'
                    ? 'A camera behind you lets your masseuse monitor your shoulders, hands, back, buttocks, legs, and feet. Your face alone represents 240+ of the data points monitored throughout your electrostimulation session.'
                    : 'Which video shows your face: your phone’s own camera, as captured, or use OBS Studio with additional filters and/or a dedicated front-facing camera.'
            }
            control={control}
            footer={footer}
            note={!setupDone && !source?.ready ? 'Connect a camera to continue.' : undefined}
        >
            {tab === 'behind' ? (
                <div className="col-span-12 flex min-h-0 flex-col">
                    {/* Notices over the lists, on the full width; nothing without one. */}
                    <div className="mb-4 flex flex-col gap-3 empty:hidden">
                        <MediaAccessAlert />
                        {locked ? (
                            <Alert variant="neutral">
                                <Lock />
                                <AlertTitle>A session is using the camera right now</AlertTitle>
                                <AlertDescription>A new choice takes effect when the session lets the camera go; nothing changes under it.</AlertDescription>
                            </Alert>
                        ) : null}
                        {devicesError ? (
                            <Alert variant="warn">
                                <TriangleAlert />
                                <AlertTitle>This computer's camera cannot be sent yet</AlertTitle>
                                <AlertDescription>{devicesError} Pairing and a camera on the network work without it.</AlertDescription>
                            </Alert>
                        ) : null}
                    </div>

                    <div className="grid min-h-0 flex-1 grid-cols-2 items-start gap-gutter">
                        <Card className="flex max-h-full min-h-0 flex-col">
                            <CardLabel icon={CameraIcon}>Camera</CardLabel>
                            <ScrollArea className="min-h-0 flex-1" viewportClassName="pb-0.5">
                                <RadioGroup value={cameraChosen} onValueChange={(v) => setCam(String(v))} disabled={locked} className="gap-2">
                                    {looking ? <DeviceSkeleton>Looking for cameras…</DeviceSkeleton> : null}
                                    {!looking && cameras.length === 0 && !devicesError ? (
                                        <Well className="flex flex-col items-center justify-center gap-2 py-5 text-center">
                                            <CameraIcon className="lucide h-6 w-6 text-bone/40" strokeWidth={2} />
                                            <span className="type-body font-semibold text-bone/80">No camera found</span>
                                            <span className="type-caption max-w-[30ch] text-bone/55">Plug one in, or start a virtual camera; it appears here on its own. Pairing works without one.</span>
                                        </Well>
                                    ) : null}
                                    {cameras.map((d) => (
                                        <DeviceCard key={d.id} device={d} inUse={camera.on && source?.camera === d.name} disabled={locked} />
                                    ))}
                                    {missing
                                        .filter((s) => s.kind === 'video')
                                        .map((s) => <MissingDeviceCard key={s.wanted} kind="video" name={s.wanted} using={s.using} />)}
                                    {/* A camera on the network, as the last choice; chosen, it opens for its address. */}
                                    <label className={cn(CHOICE_ROW, 'flex-col items-stretch gap-3', locked && CHOICE_ROW_DISABLED)}>
                                        <span className="flex min-h-9 items-center gap-3">
                                            <RadioGroupItem value={NETWORK} disabled={locked} aria-label="A camera on your network" />
                                            <HouseWifi className="lucide h-5 w-5 shrink-0 text-bone/80" strokeWidth={2.2} />
                                            <span className="min-w-0 flex-1">
                                                <span className="type-body block font-semibold tracking-tight text-bone">A camera on your network</span>
                                                <span className="type-caption block text-bone/50">Wyze, UniFi Protect, or another RTSPS-capable camera.</span>
                                            </span>
                                        </span>
                                        {networkChosen ? (
                                            <span className="flex flex-col gap-2.5 pl-8" onClick={(e) => e.stopPropagation()}>
                                                <span>
                                                    <Label htmlFor="rtsps" className="mb-1.5 text-[11px] font-semibold tracking-wide text-bone-dim uppercase">
                                                        Address
                                                    </Label>
                                                    <Input id="rtsps" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="rtsps://user:password@192.168.1.20:322/live" className="h-9 font-mono text-[13px]" disabled={locked} spellCheck={false} />
                                                </span>
                                                <Collapsible>
                                                    <CollapsibleTrigger className="group inline-flex items-center gap-1 text-[12px] font-medium text-bone/65 hover:text-bone data-[panel-open]:text-bone">
                                                        <ChevronRight className="lucide h-3.5 w-3.5 transition-transform group-data-[panel-open]:rotate-90" strokeWidth={2.4} />
                                                        Pin the certificate yourself
                                                    </CollapsibleTrigger>
                                                    <CollapsibleContent>
                                                        <span className="mt-2 block">
                                                            <Input id="fingerprint" value={fingerprint} onChange={(e) => setFingerprint(e.target.value)} placeholder="certificate SHA-256; otherwise trusted on first use and remembered" className="h-9 font-mono text-[13px]" disabled={locked} spellCheck={false} />
                                                        </span>
                                                    </CollapsibleContent>
                                                </Collapsible>
                                                <span className="text-[11px] leading-snug text-bone/45">The password stays on this computer and is used on your network only; the certificate is remembered so a replaced camera is refused.</span>
                                            </span>
                                        ) : null}
                                    </label>
                                </RadioGroup>
                            </ScrollArea>
                            {isVirtualCamera(chosenDevice) ? (
                                <Alert variant="neutral" className="mt-3 shrink-0 py-2">
                                    <Layers />
                                    <AlertTitle>This is what OBS (or its program) outputs</AlertTitle>
                                    <AlertDescription>Start the virtual camera there before a session, or the picture is black. It is sent as it is, effects and all.</AlertDescription>
                                </Alert>
                            ) : null}
                        </Card>

                        <Card className="flex max-h-full min-h-0 flex-col">
                            <CardLabel icon={Mic}>Microphone</CardLabel>
                            {networkChosen ? (
                                <Well className="type-secondary text-bone/60">A camera on the network carries its own sound, if it has any; this computer's microphones are not used with it.</Well>
                            ) : (
                                <ScrollArea className="min-h-0 flex-1" viewportClassName="pb-0.5">
                                    <RadioGroup value={micChosen} onValueChange={(v) => setMic(String(v))} disabled={locked} className="gap-2">
                                        {looking ? <DeviceSkeleton>Looking for microphones…</DeviceSkeleton> : null}
                                        {mics.map((d) => (
                                            <DeviceCard key={d.id} device={d} inUse={camera.on && source?.mic === d.name} disabled={locked} />
                                        ))}
                                        {missing
                                            .filter((s) => s.kind === 'audio')
                                            .map((s) => <MissingDeviceCard key={s.wanted} kind="audio" name={s.wanted} using={s.using} />)}
                                        <NoMicCard disabled={locked} />
                                    </RadioGroup>
                                </ScrollArea>
                            )}
                        </Card>
                    </div>
                </div>
            ) : (
                <FaceView choice={face} onChoice={setFaceChoice} faceCam={faceCamChosen} onFaceCam={setFaceCam} bodyCamera={networkChosen ? null : cameraChosen} />
            )}
        </Page>
    );
}
