// Pair: the code the phone needs, shown large, with what to do with it (why
// the person is pairing at all stands under the wordmark, ui/TopBar.tsx). The
// service sends a code with hello and a new one when it lapses; how much
// life the code has left is the ring under it, as an authenticator app
// draws it, with the time left in words beside it ("Code rotates in 9
// minutes and 5 seconds", counting down), and the code turns ember for its
// last stretch. A phone typing it in makes the connector say so, and the
// setup moves on.

import { Check, LoaderCircle, Smartphone, TriangleAlert } from 'lucide-react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { useAppState, useDispatch } from '../bridge/store';
import { Card, CardLabel } from '../ui/Card';
import { CodeCells } from '../ui/CodeCells';
import { CodeTimer, countdownText, useCountdown } from '../ui/CodeTimer';
import { Scene } from '../ui/Scene';

// The three things to do on the phone, one line each.
const STEPS = ['Open the masseuse.ai app on your phone', 'Choose “I have the code”', 'Type these eight letters and numbers'];

export function Pair() {
    const { code, online, phones, pairedAt, setupDone } = useAppState();
    const dispatch = useDispatch();
    const countdown = useCountdown(code?.expiresAt, code?.ttlMs);
    const justPaired = pairedAt !== null;
    const another = setupDone || (phones > 0 && !justPaired);
    // A lapsed code is no code: the cells wait for the next one.
    const shown = online === false || !code || countdown?.expired ? null : code.code;

    return (
        <div className="screen-body grid min-h-0 flex-1 grid-cols-[minmax(0,1.15fr)_minmax(300px,0.85fr)] gap-8 px-8 pt-1 pb-5">
            <div className="flex min-w-0 flex-col justify-center">
                {justPaired ? (
                    <>
                        <span className="mb-4 inline-flex h-12 w-12 items-center justify-center rounded-full bg-mint/15 text-mint ring-1 ring-mint/30">
                            <Check className="lucide h-6 w-6" strokeWidth={2.6} />
                        </span>
                        <h1 className="text-[28px] leading-tight font-semibold tracking-tight text-bone">Paired with your phone</h1>
                        <p className="mt-2 max-w-[44ch] text-[15px] leading-snug text-bone/75">
                            Sessions that use this camera connect on their own from now on.
                            {phones > 1 ? ` ${phones} phones are paired with this computer.` : ''}
                        </p>
                        <div className="mt-6 flex items-center gap-3">
                            {setupDone ? (
                                <Button onClick={() => dispatch({ type: 'ui/go', step: 'home' })}>Back to Ready</Button>
                            ) : (
                                <Button onClick={() => dispatch({ type: 'ui/go', step: 'camera' })}>Choose the camera and microphone</Button>
                            )}
                        </div>
                    </>
                ) : (
                    <>
                        <h1 className="text-[28px] leading-tight font-semibold tracking-tight text-bone">{another ? 'Pair another phone' : 'Type this code into your phone'}</h1>
                        <p className="mt-2 max-w-[46ch] text-[15px] leading-snug text-bone/75">
                            {another
                                ? `${phones} phone${phones === 1 ? ' is' : 's are'} paired with this computer. A phone that types this code joins them.`
                                : 'The code securely connects your masseuse to this computer.'}
                        </p>
                        <div className="mt-7">
                            <CodeCells code={shown} ending={Boolean(shown && countdown?.ending)} />
                        </div>
                        {/* Under the code, flush with its left edge: the ring and the time left while there is a code, a word while there is not. */}
                        <div className="mt-4 flex min-h-8 items-center gap-2.5 text-[13px] leading-snug text-bone/60">
                            {online === false ? (
                                <>
                                    <LoaderCircle className="lucide h-3.5 w-3.5 animate-spin text-amber" strokeWidth={2.4} />
                                    Reaching masseuse.ai…
                                </>
                            ) : shown === null ? (
                                <>
                                    <LoaderCircle className="lucide h-3.5 w-3.5 animate-spin text-rose" strokeWidth={2.4} />
                                    Generating a code…
                                </>
                            ) : (
                                <>
                                    <CodeTimer countdown={countdown} />
                                    {countdown ? (
                                        <span className={cn('tabular-nums', countdown.ending && 'text-ember')} aria-hidden>
                                            Code rotates in {countdownText(countdown.remainingMs)}
                                        </span>
                                    ) : null}
                                </>
                            )}
                        </div>
                        {online === false ? (
                            <Alert variant="warn" className="mt-5 max-w-[520px]">
                                <TriangleAlert />
                                <AlertTitle>Could not reach masseuse.ai</AlertTitle>
                                <AlertDescription>Check this computer's connection. Masseuse.ai keeps trying on its own; nothing to redo.</AlertDescription>
                            </Alert>
                        ) : null}
                        <div className="mt-8 flex items-center gap-3">
                            {setupDone ? (
                                <Button variant="quiet" onClick={() => dispatch({ type: 'ui/go', step: 'home' })}>
                                    Back to Ready
                                </Button>
                            ) : (
                                <Button variant="quiet" onClick={() => dispatch({ type: 'ui/go', step: 'camera' })}>
                                    Set up the camera first
                                </Button>
                            )}
                        </div>
                    </>
                )}
            </div>

            <div className="flex min-w-0 flex-col justify-center gap-4">
                <Scene scene="laptopBehindYou" className="aspect-[2/1] w-full" />
                <Card>
                    <CardLabel icon={Smartphone}>On your phone</CardLabel>
                    <ol className="flex flex-col gap-3.5">
                        {STEPS.map((step, i) => (
                            <li key={step} className="flex items-center gap-3.5">
                                <span className="w-6 shrink-0 text-[26px] leading-none font-semibold tracking-tight text-rose tabular-nums">{i + 1}</span>
                                <span className="min-w-0 text-[14px] leading-snug font-semibold tracking-tight text-bone">{step}</span>
                            </li>
                        ))}
                    </ol>
                </Card>
            </div>
        </div>
    );
}
