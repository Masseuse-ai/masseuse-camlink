// Ready: what this computer offers a session, and what the session is
// doing with it. Idle, it waits; in a session, the camera link and its
// rates, the unit's standing, and the proof of the enclave the picture
// goes to, the three lines the connector logs made readable. On the grid
// (ui/Page.tsx): the phones as the screen's control beside the title, the
// four choices as cards on three columns each, the session on all twelve.

import { Camera, Mic, MicOff, ShieldCheck, SlidersHorizontal, Smartphone, TriangleAlert, Zap } from 'lucide-react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { faceViewOf, useAppState, useDispatch, type Step } from '../bridge/store';
import type { CameraStats } from '../bridge/types';
import { Card, CardLabel } from '../ui/Card';
import { Page } from '../ui/Page';
import { Scene } from '../ui/Scene';
import { StatusChip, type Tone } from '../ui/StatusChip';
import { familyName } from '../ui/UnitCard';

function rate(bps: number): string {
    if (bps >= 1e6) return `${(bps / 1e6).toFixed(1)} Mb/s`;
    if (bps >= 1e3) return `${Math.round(bps / 1e3)} kb/s`;
    return `${Math.round(bps)} b/s`;
}

function Meter({ label, value, ceiling, tone = 'rose' }: { label: string; value: number; ceiling: number; tone?: 'rose' | 'mint' | 'amber' }) {
    const pct = Math.max(2, Math.min(100, (value / ceiling) * 100));
    const bar = tone === 'mint' ? 'from-mint/70 to-mint' : tone === 'amber' ? 'from-amber/70 to-amber' : 'from-rose-deep to-rose';
    return (
        <div>
            <div className="type-caption flex items-baseline justify-between">
                <span className="text-bone/55">{label}</span>
                <span className="font-mono font-semibold tabular-nums text-bone">{rate(value)}</span>
            </div>
            <div className="mt-1.5 h-1 overflow-hidden rounded-full bg-white/12">
                <div className={`h-full rounded-full bg-gradient-to-r ${bar} transition-[width] duration-500`} style={{ width: `${pct}%` }} />
            </div>
        </div>
    );
}

function Verified() {
    // The enclave's proof is checked by the connector before any picture
    // leaves this computer; the window says so in one line and keeps the
    // digests and release names to the log.
    return (
        <p className="type-secondary inline-flex items-center gap-2 font-medium text-bone/75">
            <ShieldCheck className="lucide h-4 w-4 shrink-0 text-mint" strokeWidth={2.2} />
            Video's privacy protected
        </p>
    );
}

function SummaryCard({ icon: Icon, label, value, note, badge, step, tab, disabled }: { icon: typeof Camera; label: string; value: string; note?: string; badge?: { text: string; variant: 'mint' | 'rose' | 'amber' | 'secondary' }; step: Step; tab?: 'behind' | 'face'; disabled?: boolean }) {
    const dispatch = useDispatch();
    return (
        <Card className="col-span-3 flex min-w-0 flex-col p-4 pb-2.5">
            {/* The same rows in every card: the eyebrow, the standing, the choice, its note, the way to change it; a row keeps its height empty, so the four line up. */}
            <div className="type-label flex items-center gap-2 text-bone-dim">
                <Icon className="lucide h-4 w-4 shrink-0" strokeWidth={2.2} />
                <span className="truncate">{label}</span>
            </div>
            <div className="mt-2 flex h-5 items-center">
                {badge ? (
                    <Badge variant={badge.variant} className="max-w-full">
                        <span className="truncate">{badge.text}</span>
                    </Badge>
                ) : null}
            </div>
            <span className="type-body mt-2 truncate font-semibold tracking-tight text-bone" title={value}>
                {value}
            </span>
            <span className="type-caption mt-0.5 line-clamp-2 h-8 text-bone/55">{note ?? ''}</span>
            <div className="mt-2">
                <Button variant="quiet" size="sm" className="-ml-2.5" disabled={disabled} onClick={() => dispatch({ type: 'ui/go', step, tab })}>
                    Change
                </Button>
            </div>
        </Card>
    );
}

export function Home() {
    const { source, unit, link, camera, faceCamera, phones, online } = useAppState();
    const dispatch = useDispatch();
    const processed = faceViewOf(source) === 'processed';
    const share = source?.share;

    const inSession = link.state === 'active';
    const stats: CameraStats | undefined = camera.stats;
    // The connector's word on a camera that is on but sending nothing (the
    // camera delivered no picture, ffmpeg could not open it, ...): shown for
    // as long as it holds, so "camera on" is not taken for a picture going.
    const notSending = camera.on ? stats?.reason : undefined;
    const headline = inSession ? 'Session in progress' : 'Your session is about to start.';
    const lead = inSession
        ? camera.on
            ? notSending
                ? 'The camera link is up, but no picture is being sent yet.'
                : 'The camera link is up and the picture is going to the verified enclave, and nowhere else.'
            : 'The camera link is up; the camera comes on when the session reads it.'
        : phones > 0
          ? 'Follow along on your phone. If at any point you experience discomfort, switch your unit to OFF.'
          : 'Pair your phone first: type the code Masseuse.ai shows under Pair.';

    const linkChip: { tone: Tone; text: string } = (() => {
        switch (link.state) {
            case 'active':
                if (notSending) return { tone: 'warn', text: 'Camera link active · no picture yet' };
                return { tone: 'live', text: camera.on ? 'Camera link active · camera on' : 'Camera link active' };
            case 'on-hold':
                return { tone: 'warn', text: 'Camera link on hold' };
            case 'closed':
                return { tone: 'neutral', text: 'Camera link closed' };
            default:
                return { tone: 'neutral', text: online ? 'Waiting for a session' : 'Waiting for masseuse.ai' };
        }
    })();

    const micName = source?.kind === 'capture' ? source.mic : undefined;

    // The screen's one control: the phones, and the way to pair another.
    const control = (
        <Button variant="ghost" size="sm" icon={<Smartphone className="lucide h-4 w-4" strokeWidth={2.4} />} onClick={() => dispatch({ type: 'ui/go', step: 'pair' })}>
            {phones > 0 ? `${phones} phone${phones === 1 ? '' : 's'} paired` : 'Pair your phone'}
        </Button>
    );

    return (
        <Page title={headline} lead={lead} control={control} bodyClassName="grid-rows-[auto_minmax(0,1fr)] gap-y-4">
            <SummaryCard
                icon={Camera}
                label="Behind you"
                value={source?.kind === 'camera' ? source.label : (source?.camera ?? source?.label ?? 'Finding…')}
                note={source?.ready ? (camera.on ? 'Sending to the verified enclave' : 'On only while a session reads it') : source?.note}
                badge={source?.ready ? (camera.on ? { text: 'On', variant: 'mint' } : { text: 'Off until watched', variant: 'secondary' }) : source ? { text: 'Not available', variant: 'amber' } : undefined}
                step="camera"
                tab="behind"
                disabled={camera.on}
            />
            <SummaryCard
                icon={micName && micName !== 'none' ? Mic : MicOff}
                label="Microphone"
                value={source?.kind === 'camera' ? "The camera's own" : micName && micName !== 'none' ? micName : 'None · video only'}
                note={micName && micName !== 'none' ? 'Sent with the picture' : source?.kind === 'camera' ? 'Whatever the camera carries' : "The session hears your phone's microphone"}
                step="camera"
                tab="behind"
                disabled={camera.on}
            />
            <SummaryCard
                icon={processed ? SlidersHorizontal : Smartphone}
                label="Your face"
                value={processed ? 'OBS Studio' : "Your phone's camera"}
                note={processed ? (source?.face?.camera ?? 'No camera carries it back yet') : 'Straight to the room, no detour'}
                badge={
                    processed
                        ? faceCamera.on
                            ? { text: 'Live', variant: 'mint' }
                            : share?.receiving
                              ? { text: 'Phone picture arriving', variant: 'rose' }
                              : { text: 'Waiting for your phone', variant: 'secondary' }
                        : undefined
                }
                step="camera"
                tab="face"
                disabled={faceCamera.on}
            />
            <SummaryCard
                icon={Zap}
                label="Unit"
                value={unit?.connected ? unit.label : unit ? unit.label : 'No unit'}
                note={unit?.connected ? (unit.armed ? `Armed · up to ${unit.armed.levelBound} of ${unit.capabilities.levelMax}` : `${familyName(unit.kind)} · held at zero`) : unit ? 'Disconnected; reconnects on its own' : 'Optional; found on its own when switched on'}
                badge={unit?.connected ? (unit.armed ? { text: 'Armed', variant: 'rose' } : { text: 'Held at zero', variant: 'mint' }) : unit ? { text: 'Disconnected', variant: 'amber' } : undefined}
                step="unit"
                disabled={Boolean(unit?.armed)}
            />

            <Card className="col-span-12 flex min-h-0 flex-col">
                <CardLabel icon={ShieldCheck} trailing={<StatusChip tone={linkChip.tone}>{linkChip.text}</StatusChip>}>
                    Session
                </CardLabel>

                {inSession ? (
                    <div className="grid-12 min-h-0 flex-1">
                        <div className="col-span-7 flex min-w-0 flex-col justify-center gap-3">
                            {stats ? (
                                <>
                                    <Meter label="Video" value={stats.videoBps} ceiling={2_500_000} tone={stats.congested ? 'amber' : 'rose'} />
                                    <Meter label="Audio" value={stats.audioBps} ceiling={96_000} tone="mint" />
                                    {processed && faceCamera.on && faceCamera.stats ? <Meter label="Face · OBS's picture going back" value={faceCamera.stats.videoBps} ceiling={2_500_000} tone="rose" /> : null}
                                </>
                            ) : (
                                <p className="type-body text-bone/65">Camera on but not sending yet (waiting for the source).</p>
                            )}
                            {processed ? (
                                <div className="type-caption flex flex-wrap items-center gap-x-3 gap-y-1 text-bone/60">
                                    {[
                                        { text: "Phone picture to this computer", on: Boolean(share?.receiving) },
                                        { text: 'OBS picture back', on: faceCamera.on },
                                        { text: 'Shown as your face', on: faceCamera.on },
                                    ].map((hop) => (
                                        <span key={hop.text} className="inline-flex items-center gap-1.5">
                                            <span className={`h-2 w-2 rounded-full ${hop.on ? 'bg-mint animate-pulse-soft' : 'bg-bone-dim/60'}`} />
                                            {hop.text}
                                        </span>
                                    ))}
                                </div>
                            ) : null}
                            {notSending ? (
                                <Alert variant="warn">
                                    <TriangleAlert />
                                    <AlertTitle>Camera on, but no picture is being sent</AlertTitle>
                                    <AlertDescription>{notSending.charAt(0).toUpperCase() + notSending.slice(1)}. The session waits for the picture; the link is not the trouble.</AlertDescription>
                                </Alert>
                            ) : null}
                            {stats?.congested ? (
                                <Alert variant="warn">
                                    <TriangleAlert />
                                    <AlertTitle>Connection congested: dropping video to keep up (backlog {stats.backlogS.toFixed(1)} s)</AlertTitle>
                                    <AlertDescription>Whole frames are left out rather than queued, so what arrives is current and the audio keeps flowing. The session sees a rougher picture, not a frozen one.</AlertDescription>
                                </Alert>
                            ) : null}
                            {link.enclave ? <Verified /> : null}
                        </div>
                        <div className="col-span-5 flex min-h-0 flex-col justify-center">
                            <Scene scene="cameraBehindYou" className="aspect-[2/1] max-h-[220px] w-full" />
                            <p className="type-caption mt-1.5 text-center text-bone/50">
                                {processed && faceCamera.on ? "OBS's picture is your face, a small inset over this picture." : "The phone's camera stays live too, as a small inset over this picture."}
                            </p>
                        </div>
                    </div>
                ) : (
                    <div className="grid-12 min-h-0 flex-1">
                        <div className="col-span-7 flex min-w-0 flex-col justify-center gap-3">
                            {link.state === 'on-hold' ? (
                                <Alert variant="neutral">
                                    <TriangleAlert />
                                    <AlertTitle>Camera link on hold</AlertTitle>
                                    <AlertDescription>The session moved on ({link.reason ?? 'the enclave is not expecting this connector'}). The next “Use this camera” on the phone brings a new ticket; nothing to do here.</AlertDescription>
                                </Alert>
                            ) : link.state === 'closed' ? (
                                <Alert variant="neutral">
                                    <AlertTitle>Camera link closed</AlertTitle>
                                    <AlertDescription>The session let the camera go. It is off, and comes on again with the next session.</AlertDescription>
                                </Alert>
                            ) : (
                                <>
                                    <p className="type-body text-bone/80">{phones > 0 ? 'Waiting for a session on your phone.' : 'Waiting for a phone to pair.'}</p>
                                    <p className="type-secondary text-bone/55">
                                        To protect your privacy, we're setting up a confidential computing environment to process your video and protect your identity. Until then, your unit stays at zero. The computer stays awake while this window is open; the screen may go dark.
                                    </p>
                                </>
                            )}
                        </div>
                        <div className="col-span-5 flex min-h-0 flex-col justify-center">
                            <Scene scene="laptopBehindYou" className="aspect-[2/1] max-h-[220px] w-full" />
                        </div>
                    </div>
                )}
            </Card>
        </Page>
    );
}
