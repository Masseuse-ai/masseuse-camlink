// Unit: the stimulation unit Masseuse.ai serves. Nothing to set up when
// there is one: switched on and within reach, it is found and held at zero
// until a session on the phone uses this computer. With several found, one
// is chosen here (or on the phone); the unit let go of is put to zero
// first. Which units work is shown where there is none yet, and behind
// "Which units work?" otherwise (ui/SupportedUnits.tsx).

import { BatteryMedium, Bluetooth, BluetoothOff, Check, Cpu, LoaderCircle, Lock, Power, ShieldCheck, TriangleAlert, Zap } from 'lucide-react';
import { useEffect, useState } from 'react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { RadioGroup } from '@/components/ui/radio-group';
import { ScrollArea } from '@/components/ui/scroll-area';
import { useAppState, useBridge, useDispatch } from '../bridge/store';
import type { Descriptor } from '../bridge/types';
import { Card, CardLabel, Well } from '../ui/Card';
import { Scene } from '../ui/Scene';
import { SupportedUnits } from '../ui/SupportedUnits';
import { UnitCard, familyName } from '../ui/UnitCard';

/** The connector's word on a unit gone, by the reason it knows (cmd/masseuse-camlink/estim.go). */
export function disconnectedLine(reason: string | undefined): { title: string; text: string } {
    switch (reason) {
        case 'idle-off':
            return { title: 'The unit switched itself off after sitting idle at zero', text: 'Press its power button; it reconnects on its own.' };
        case 'output-off':
            return { title: 'The unit switched itself off after running for too long', text: 'Press its power button; it reconnects on its own.' };
        case 'battery-off':
            return { title: 'The unit switched itself off, battery low', text: 'Charge it; it reconnects on its own when it is on again.' };
        case 'button-off':
            return { title: 'The unit was switched off at its power button', text: 'It reconnects on its own when it is on again.' };
        case 'link-lost':
            return { title: 'The link to the unit dropped', text: 'It reconnects on its own once it is within reach again.' };
        case 'let-go':
            return { title: 'The unit was let go for the one selected', text: 'The new unit connects in a moment.' };
        default:
            return { title: 'The unit disconnected', text: 'It reconnects on its own when it is on and within reach.' };
    }
}

function ServingCard({ unit }: { unit: Descriptor }) {
    const armed = unit.armed;
    const status = unit.status;
    return (
        <Card>
            <CardLabel icon={Zap} tone={armed ? 'rose' : 'dim'} trailing={armed ? <Badge variant="rose">Armed by a session</Badge> : <Badge variant="mint">Held at zero</Badge>}>
                Serving
            </CardLabel>
            <h2 className="text-[20px] leading-tight font-semibold tracking-tight text-bone">{unit.label}</h2>
            <p className="mt-0.5 text-[13px] text-bone/55">{familyName(unit.kind)}</p>
            <p className="mt-3 text-[14px] leading-snug text-bone/75">
                {armed
                    ? `A session on your phone has it armed, up to ${armed.levelBound} of ${unit.capabilities.levelMax}. It goes back to zero when the session ends, when the service goes quiet, or when you close this window.`
                    : 'Held at zero, its own buttons live, until a session on your phone uses this computer. The session arms it within a bound you set on the phone.'}
            </p>
            <div className="mt-4 grid grid-cols-3 gap-2">
                {status?.batteryPercent !== undefined ? (
                    <Well className="flex items-center gap-2.5 py-2.5">
                        <BatteryMedium className="lucide h-4 w-4 text-bone/60" strokeWidth={2.2} />
                        <span className="text-[13px]">
                            <span className="block text-[11px] tracking-wide text-bone/50 uppercase">Battery</span>
                            <span className="font-semibold tabular-nums text-bone">{status.batteryPercent}%</span>
                        </span>
                    </Well>
                ) : null}
                {status?.power ? (
                    <Well className="flex items-center gap-2.5 py-2.5">
                        <Power className="lucide h-4 w-4 text-bone/60" strokeWidth={2.2} />
                        <span className="text-[13px]">
                            <span className="block text-[11px] tracking-wide text-bone/50 uppercase">Power</span>
                            <span className="font-semibold text-bone capitalize">{status.power}</span>
                        </span>
                    </Well>
                ) : null}
                <Well className="flex items-center gap-2.5 py-2.5">
                    <Cpu className="lucide h-4 w-4 text-bone/60" strokeWidth={2.2} />
                    <span className="text-[13px]">
                        <span className="block text-[11px] tracking-wide text-bone/50 uppercase">Level</span>
                        <span className="font-semibold tabular-nums text-bone">
                            {status?.levelA ?? 0}
                            <span className="text-bone/45"> / {unit.capabilities.levelMax}</span>
                        </span>
                    </span>
                </Well>
                <Well className="flex items-center gap-2.5 py-2.5">
                    <ShieldCheck className="lucide h-4 w-4 text-bone/60" strokeWidth={2.2} />
                    <span className="text-[13px]">
                        <span className="block text-[11px] tracking-wide text-bone/50 uppercase">Range</span>
                        <span className="font-semibold tabular-nums text-bone">
                            0<span className="text-bone/45"> to </span>
                            {armed?.levelBound ?? unit.capabilities.levelMaxDefault ?? unit.capabilities.levelMax}
                        </span>
                    </span>
                </Well>
            </div>
        </Card>
    );
}

export function Unit() {
    const { units, unitsScanning, unit, bluetooth, setupDone, platform, link } = useAppState();
    const dispatch = useDispatch();
    const bridge = useBridge();
    const [choice, setChoice] = useState<string | null>(unit?.id ?? null);
    const [switching, setSwitching] = useState<string | null>(null);
    const [whichOpen, setWhichOpen] = useState(false);

    useEffect(() => {
        if (unit?.connected) {
            setChoice(unit.id ?? null);
            setSwitching(null);
        }
    }, [unit]);

    const armed = Boolean(unit?.armed);
    const gone = unit && !unit.connected && unit.reason !== 'let-go' ? disconnectedLine(unit.reason) : null;
    const heldByOther = unit?.connected && unit.held;

    const select = async (id: string) => {
        setChoice(id);
        if (id === unit?.id && unit.connected) return;
        setSwitching(id);
        await bridge.send({ type: 'select_unit', id });
        setTimeout(() => setSwitching((s) => (s === id ? null : s)), 3000);
    };

    const finish = () => dispatch(setupDone ? { type: 'ui/go', step: 'home' } : { type: 'ui/setup-done' });

    return (
        <div className="screen-body flex min-h-0 flex-1 flex-col px-8 pt-1 pb-5">
            <div>
                <h1 className="text-[24px] leading-tight font-semibold tracking-tight text-bone">Your stimulation unit</h1>
                <p className="mt-1 max-w-[70ch] text-[14px] leading-snug text-bone/75">
                    Switch it on and bring it within reach; nothing else to set up. Masseuse.ai holds it at zero until a session on your phone uses this computer, and puts it back to zero when the session ends.
                </p>
            </div>

            <div className="mt-4 grid min-h-0 flex-1 grid-cols-[minmax(0,1fr)_minmax(320px,0.9fr)] gap-4">
                <Card className="flex min-h-0 flex-col">
                    <CardLabel
                        icon={Bluetooth}
                        trailing={
                            <span className="flex items-center gap-3">
                                {unitsScanning || units.length === 0 ? (
                                    <span className="inline-flex items-center gap-1.5 text-[12px] text-bone/55">
                                        <LoaderCircle className="lucide h-3.5 w-3.5 animate-spin text-rose" strokeWidth={2.4} />
                                        Looking…
                                    </span>
                                ) : null}
                                {units.length > 0 ? (
                                    <button type="button" onClick={() => setWhichOpen(true)} className="text-[12px] font-medium text-bone/55 underline decoration-bone/30 underline-offset-2 hover:text-bone">
                                        Which units work?
                                    </button>
                                ) : null}
                            </span>
                        }
                    >
                        Units found
                    </CardLabel>

                    {bluetooth === 'permission' ? (
                        <Alert variant="warn" className="mb-3">
                            <BluetoothOff />
                            <AlertTitle>Masseuse.ai needs permission to use Bluetooth</AlertTitle>
                            <AlertDescription>
                                {platform === 'darwin' ? 'macOS asked when this window opened. Allow it under System Settings › Privacy & Security › Bluetooth, then come back here.' : 'Allow Bluetooth for Masseuse.ai in the system settings, then come back here.'}
                            </AlertDescription>
                        </Alert>
                    ) : null}
                    {bluetooth === 'off' ? (
                        <Alert variant="warn" className="mb-3">
                            <BluetoothOff />
                            <AlertTitle>Bluetooth is off</AlertTitle>
                            <AlertDescription>
                                {platform === 'windows' ? 'Turn it on in Settings › Bluetooth & devices. A unit over a USB cable is found either way.' : 'Turn it on, then the unit is found on its own. A unit over a USB cable is found either way.'}
                            </AlertDescription>
                        </Alert>
                    ) : null}

                    {units.length === 0 ? (
                        <ScrollArea className="flex-1" viewportClassName="pb-0.5">
                            <div className="mb-2.5 flex items-center gap-3">
                                <Scene scene="connectYourUnit" className="aspect-[2/1] w-[104px] shrink-0" />
                                <div className="min-w-0">
                                    <p className="text-[14px] font-semibold tracking-tight text-bone/85">No units found</p>
                                    <p className="mt-0.5 text-[12px] leading-snug text-bone/55">Switch the unit on. Masseuse.ai finds it on its own. It works with these:</p>
                                </div>
                            </div>
                            <SupportedUnits layout="compact" />
                        </ScrollArea>
                    ) : (
                        <ScrollArea className="flex-1" viewportClassName="pb-0.5">
                            <RadioGroup value={choice} onValueChange={(v) => void select(String(v))} disabled={armed} className="gap-2">
                                {units.map((u) => (
                                    <UnitCard key={u.id} unit={u} serving={unit} disabled={armed} />
                                ))}
                            </RadioGroup>
                        </ScrollArea>
                    )}

                    {switching && switching !== unit?.id ? (
                        <p className="mt-3 inline-flex items-center gap-2 text-[13px] text-bone/65">
                            <LoaderCircle className="lucide h-3.5 w-3.5 animate-spin text-rose" strokeWidth={2.4} />
                            Switching: the unit let go is put to zero first.
                        </p>
                    ) : null}
                    {armed && units.length > 1 ? (
                        <p className="mt-3 inline-flex items-center gap-2 text-[13px] text-bone/65">
                            <Lock className="lucide h-3.5 w-3.5" strokeWidth={2.4} />
                            A session has the unit armed; stop the unit on the phone before switching.
                        </p>
                    ) : null}

                </Card>

                <ScrollArea viewportClassName="flex flex-col gap-3 pb-0.5">
                    {unit?.connected ? (
                        <ServingCard unit={unit} />
                    ) : gone ? (
                        <Card>
                            <CardLabel icon={Power} trailing={<Badge variant="amber">Disconnected</Badge>}>
                                Last served
                            </CardLabel>
                            <h2 className="text-[18px] leading-tight font-semibold tracking-tight text-bone">{unit?.label}</h2>
                            <Alert variant="warn" className="mt-3">
                                <TriangleAlert />
                                <AlertTitle>{gone.title}</AlertTitle>
                                <AlertDescription>{gone.text}</AlertDescription>
                            </Alert>
                        </Card>
                    ) : (
                        <Card>
                            <CardLabel icon={ShieldCheck}>What the session may do</CardLabel>
                            <ul className="flex flex-col gap-2.5 text-[13px] leading-snug text-bone/75">
                                {[
                                    'The unit is held paused at zero, its own buttons live, until a session on your phone arms it.',
                                    'The highest intensity is a bound you set on the phone, for that session only; never more than the unit allows.',
                                    'It goes back to zero when the session ends, when the service goes quiet for 15 seconds, when any command fails, and when you close this window.',
                                    'The intensity moves one step at a time and is read back at every step.',
                                ].map((line) => (
                                    <li key={line} className="flex gap-2.5">
                                        <Check className="lucide mt-0.5 h-3.5 w-3.5 shrink-0 text-mint" strokeWidth={2.6} />
                                        <span>{line}</span>
                                    </li>
                                ))}
                            </ul>
                        </Card>
                    )}

                    {heldByOther ? (
                        <Alert variant="warn">
                            <TriangleAlert />
                            <AlertTitle>Another program on this computer has this unit open</AlertTitle>
                            <AlertDescription>Close it before a session, or its commands and Masseuse.ai's will collide. Masseuse.ai shares the link rather than fighting for it.</AlertDescription>
                        </Alert>
                    ) : null}

                    {link.state === 'active' && !unit?.connected ? (
                        <Alert variant="neutral">
                            <Zap />
                            <AlertTitle>The session is running without a unit</AlertTitle>
                            <AlertDescription>A unit switched on now is found and offered to the session on its own.</AlertDescription>
                        </Alert>
                    ) : null}
                </ScrollArea>
            </div>

            <div className="mt-3 flex shrink-0 items-center justify-end gap-4">
                <div className="flex shrink-0 items-center gap-2">
                    {setupDone ? (
                        <Button variant="quiet" onClick={() => dispatch({ type: 'ui/go', step: 'home' })}>
                            Back to Ready
                        </Button>
                    ) : (
                        <>
                            <Button variant="quiet" onClick={() => dispatch({ type: 'ui/go', step: 'camera' })}>
                                Back
                            </Button>
                            {unit?.connected ? (
                                <Button onClick={finish}>Done</Button>
                            ) : (
                                <Button variant="ghost" onClick={finish}>
                                    I don't have one · Skip until later
                                </Button>
                            )}
                        </>
                    )}
                </div>
            </div>

            <Dialog open={whichOpen} onOpenChange={setWhichOpen}>
                <DialogContent className="sm:max-w-[520px]">
                    <DialogHeader>
                        <DialogTitle>Units Masseuse.ai works with</DialogTitle>
                        <DialogDescription>Connected to this computer, any of these is found on its own, held at zero, and the session's to drive within the bound you set on the phone.</DialogDescription>
                    </DialogHeader>
                    <SupportedUnits />
                </DialogContent>
            </Dialog>
        </div>
    );
}
