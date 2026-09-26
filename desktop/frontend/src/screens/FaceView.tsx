// Your face: which picture the room shows as the person's face and streams
// on. The phone's own camera, straight to the room, is the default and
// needs nothing. The other choice is for people who process their picture
// in OBS: the room sends the phone's picture to this computer, OBS reads it
// as a Media Source, and a virtual camera carries OBS's output back through
// this computer as the face camera. The desktop sets that loop up and
// reports it; the phone opens it from its Setup sheet, and until it does the
// steps here say they are waiting. (The room keeps reading the phone's own
// picture for the session's cues; the page does not say so, it is plumbing.)

import { Check, Copy, Layers, Lock, Smartphone, SlidersHorizontal } from 'lucide-react';
import { useEffect, useMemo, useState, type ReactNode } from 'react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group';
import { ScrollArea } from '@/components/ui/scroll-area';
import { cn } from '@/lib/utils';
import { faceViewOf, offersFaceLoop, useAppState, type FaceView as FaceViewChoice } from '../bridge/store';
import type { Device } from '../bridge/types';
import { copyText } from '../shell/shell';
import { Card, Well } from '../ui/Card';
import { DeviceCard, natureOf } from '../ui/DeviceCard';
import { StatusChip, type Tone } from '../ui/StatusChip';

interface Props {
    choice: FaceViewChoice;
    onChoice: (c: FaceViewChoice) => void;
    faceCam: string | null;
    onFaceCam: (name: string) => void;
    /** The camera chosen behind the person; it cannot also carry the face. */
    bodyCamera: string | null;
}

/** Virtual cameras first: OBS's is the one this is for. */
export function facePickOrder(cameras: Device[]): Device[] {
    return [...cameras].sort((a, b) => Number(natureOf(b) === 'virtual') - Number(natureOf(a) === 'virtual'));
}

function Step({ n, title, badge, chip, children }: { n: number; title: string; badge?: string; chip?: { tone: Tone; text: string }; children: ReactNode }) {
    return (
        <div className="flex gap-3.5">
            <span className="w-6 shrink-0 pt-0.5 text-[24px] leading-none font-semibold tracking-tight text-rose tabular-nums">{n}</span>
            <div className="min-w-0 flex-1">
                <div className="flex items-center justify-between gap-3">
                    <h3 className="flex items-center gap-2 text-[14px] leading-snug font-semibold tracking-tight text-bone">
                        {title}
                        {badge ? <Badge variant="secondary">{badge}</Badge> : null}
                    </h3>
                    {chip ? <StatusChip tone={chip.tone}>{chip.text}</StatusChip> : null}
                </div>
                <div className="mt-2">{children}</div>
            </div>
        </div>
    );
}

function ChoiceCard({ value, title, lines, badge, disabled, children }: { value: FaceViewChoice; title: string; lines: string[]; badge?: string; disabled?: boolean; children?: ReactNode }) {
    return (
        <label
            className={cn(
                'flex min-w-0 cursor-pointer items-start gap-3.5 rounded-2xl bg-white/5 px-4 py-3 ring-1 ring-white/10 transition-colors hover:bg-white/8 has-data-checked:bg-rose/10 has-data-checked:ring-rose/40',
                disabled && 'cursor-default opacity-60 hover:bg-white/5',
            )}
        >
            <RadioGroupItem value={value} disabled={disabled} aria-label={title} className="mt-1" />
            {children}
            <span className="min-w-0 flex-1">
                <span className="flex items-center gap-2">
                    <span className="text-[15px] font-semibold tracking-tight text-bone">{title}</span>
                    {badge ? <Badge variant="rose">{badge}</Badge> : null}
                </span>
                {lines.map((line) => (
                    <span key={line} className="mt-0.5 block text-[12px] leading-snug text-bone/60">
                        {line}
                    </span>
                ))}
            </span>
        </label>
    );
}

export function FaceView({ choice, onChoice, faceCam, onFaceCam, bodyCamera }: Props) {
    const { source, devices, faceCamera, link } = useAppState();
    const cameras = useMemo(() => facePickOrder(devices?.cameras ?? []), [devices]);
    const anyVirtual = cameras.some((d) => natureOf(d) === 'virtual');
    const [copied, setCopied] = useState(false);
    useEffect(() => {
        if (!copied) return;
        const t = setTimeout(() => setCopied(false), 1800);
        return () => clearTimeout(t);
    }, [copied]);

    if (!source) {
        return <p className="px-1 text-[13px] text-bone/55">Finding the cameras…</p>;
    }
    if (!offersFaceLoop(source)) {
        return (
            <Card>
                <p className="text-[14px] leading-snug text-bone/75">
                    This version of Masseuse.ai shows your phone's own camera as your face, straight to the room. Processing it through OBS on this computer comes with the next release.
                </p>
            </Card>
        );
    }

    const share = source?.share;
    const locked = faceCamera.on;
    const current = faceViewOf(source);
    // Step 1 is optional, so it says nothing while the phone is not sending.
    const shareChip: { tone: Tone; text: string } | undefined = !share?.ready
        ? { tone: 'warn', text: 'Off: started with -no-share' }
        : share.receiving
          ? { tone: 'live', text: 'Arriving' }
          : undefined;
    const backChip: { tone: Tone; text: string } = faceCamera.on
        ? { tone: 'live', text: 'Live: going back to masseuse.ai' }
        : link.state === 'active'
          ? { tone: 'neutral', text: 'Waiting for your phone' }
          : { tone: 'neutral', text: 'Waiting for a session' };

    return (
        <ScrollArea className="flex-1" viewportClassName="flex flex-col gap-4 pb-1">
            <RadioGroup value={choice} onValueChange={(v) => onChoice(v as FaceViewChoice)} disabled={locked} className="grid-cols-2 gap-3">
                <ChoiceCard value="phone" title="Your phone's camera" lines={['As captured; nothing passes through this computer.']} disabled={locked}>
                    <Smartphone className="lucide mt-0.5 h-5 w-5 shrink-0 text-bone/80" strokeWidth={2.2} />
                </ChoiceCard>
                <ChoiceCard value="processed" title="OBS Studio" lines={['Apply filters or use a dedicated front-facing camera.']} badge="Advanced" disabled={locked}>
                    <SlidersHorizontal className="lucide mt-0.5 h-5 w-5 shrink-0 text-bone/80" strokeWidth={2.2} />
                </ChoiceCard>
            </RadioGroup>

            {locked ? (
                <Alert variant="neutral">
                    <Lock />
                    <AlertTitle>The room is showing OBS's picture right now</AlertTitle>
                    <AlertDescription>A new choice takes effect when the session lets the face camera go.</AlertDescription>
                </Alert>
            ) : null}

            {choice === 'processed' ? (
                <Card className="flex flex-col gap-4">
                    <Step n={1} title="Pull your phone's camera into OBS Studio" badge="Optional" chip={current === 'processed' ? shareChip : undefined}>
                        {share?.ready && share.address ? (
                            <Well className="flex items-center gap-3 py-2.5">
                                <code className="min-w-0 flex-1 truncate font-mono text-[13px] text-bone select-text">{share.address}</code>
                                <button
                                    type="button"
                                    onClick={() => void copyText(share.address!).then((ok) => setCopied(ok))}
                                    className="inline-flex h-8 shrink-0 items-center gap-1.5 rounded-full bg-white/10 px-3 text-[12px] font-medium text-bone ring-1 ring-white/15 transition-colors hover:bg-white/14"
                                >
                                    {copied ? <Check className="lucide h-3.5 w-3.5 text-mint" strokeWidth={2.6} /> : <Copy className="lucide h-3.5 w-3.5" strokeWidth={2.4} />}
                                    {copied ? 'Copied' : 'Copy'}
                                </button>
                            </Well>
                        ) : share?.ready ? (
                            <Well className="py-2.5 text-[13px] text-bone/60">The address appears here once this choice is applied.</Well>
                        ) : (
                            <Alert variant="warn">
                                <AlertTitle>This connector was started with -no-share</AlertTitle>
                                <AlertDescription>Start Masseuse.ai without it to offer your phone's picture to OBS.</AlertDescription>
                            </Alert>
                        )}
                        <p className="mt-2 text-[12px] leading-snug text-bone/55">
                            Or give OBS Studio a dedicated camera on this computer instead. Either way, only the camera you select in step 2 goes back to masseuse.ai.
                        </p>
                    </Step>

                    <Step n={2} title="Apply filters in OBS Studio, then select your post-processed front-facing camera">
                        {cameras.length === 0 ? (
                            <Well className="py-2.5 text-[13px] text-bone/60">No camera found on this computer yet.</Well>
                        ) : (
                            <RadioGroup value={faceCam} onValueChange={(v) => onFaceCam(String(v))} disabled={locked} className="gap-2">
                                {cameras.map((d) => {
                                    const isBody = d.name === bodyCamera;
                                    return <DeviceCard key={d.id} device={d} disabled={locked || isBody} note={isBody ? 'In use behind you' : undefined} inUse={faceCamera.on && source?.face?.camera === d.name} />;
                                })}
                            </RadioGroup>
                        )}
                        {!anyVirtual ? (
                            <Alert variant="neutral" className="mt-2 py-2">
                                <Layers />
                                <AlertTitle>No virtual camera yet</AlertTitle>
                                <AlertDescription>Start the virtual camera in OBS (Tools › Start Virtual Camera); it appears here on its own.</AlertDescription>
                            </Alert>
                        ) : null}
                    </Step>

                    <Step n={3} title="Enable it in https://masseuse.ai" chip={current === 'processed' ? backChip : undefined}>
                        <p className="text-[13px] leading-snug text-bone/75">
                            On your phone, under Setup, switch on <span className="text-bone">Show the computer's picture as my face</span>; and <span className="text-bone">Send my phone's picture to the computer</span> if you did step 1.
                        </p>
                    </Step>
                </Card>
            ) : null}
        </ScrollArea>
    );
}
