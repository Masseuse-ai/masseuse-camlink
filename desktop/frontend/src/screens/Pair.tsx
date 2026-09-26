// Pair: the code the phone needs, shown large, with what to do with it (why
// the person is pairing at all stands in the masthead, ui/Masthead.tsx). The
// service sends a code with hello and a new one when it lapses; how much
// life the code has left is the ring under it, as an authenticator app
// draws it, with the time left in words beside it ("Code rotates in 9
// minutes and 5 seconds", counting down), and the code turns ember for its
// last stretch. A phone typing it in makes the connector say so, and the
// page moves on by itself (bridge/store.tsx, `paired`): to the camera in
// the first run, back to Ready afterwards; there is no confirmation screen.
//
// On the grid (ui/Page.tsx): the title, the code and its ring as one block
// on seven columns at the left; at the right, on five, one card that is the
// phone's side of it, the picture of the room over the three things to do
// there; the two centred on one axis, since the code is the screen. The way
// on, for a person back here after pairing, is the footer's.

import { LoaderCircle, Smartphone, TriangleAlert } from 'lucide-react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { useAppState, useDispatch } from '../bridge/store';
import { Card, CardLabel } from '../ui/Card';
import { CodeCells } from '../ui/CodeCells';
import { CodeTimer, countdownText, useCountdown } from '../ui/CodeTimer';
import { Page, PageLead, PageTitle } from '../ui/Page';
import { Scene } from '../ui/Scene';

// The three things to do on the phone, one line each.
const STEPS = ['Open the masseuse.ai app on your phone', 'Choose “I have the code”', 'Type these eight letters and numbers'];

export function Pair() {
    const { code, online, phones, setupDone } = useAppState();
    const dispatch = useDispatch();
    const countdown = useCountdown(code?.expiresAt, code?.ttlMs);
    const another = phones > 0;
    // A lapsed code is no code: the cells wait for the next one.
    const shown = online === false || !code || countdown?.expired ? null : code.code;

    const title = another ? 'Pair another phone' : 'Type this code into your phone';
    const lead = another
        ? `${phones} phone${phones === 1 ? ' is' : 's are'} paired with this computer. A phone that types this code joins them.`
        : 'The code securely connects your masseuse to this computer.';

    // Nothing leads past this step before a phone has paired: a paired phone
    // can see this computer's camera, so pairing comes first. Paired during
    // this run and back here, the way on is the camera.
    const footer = setupDone ? (
        <Button variant="quiet" onClick={() => dispatch({ type: 'ui/go', step: 'home' })}>
            Back to Ready
        </Button>
    ) : phones > 0 ? (
        <Button onClick={() => dispatch({ type: 'ui/go', step: 'camera' })}>Continue to the camera and microphone</Button>
    ) : undefined;

    return (
        <Page footer={footer} bodyClassName="items-center">
            <div className="col-span-7 flex min-w-0 flex-col">
                <PageTitle>{title}</PageTitle>
                <PageLead>{lead}</PageLead>
                <CodeCells code={shown} ending={Boolean(shown && countdown?.ending)} className="mt-6" />
                {/* Under the code, flush with its left edge: the ring and the time left while there is a code, a word while there is not. */}
                <div className="type-secondary mt-4 flex min-h-8 items-center gap-2.5 text-bone/60">
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
                    <Alert variant="warn" className="mt-5 max-w-[560px]">
                        <TriangleAlert />
                        <AlertTitle>Could not reach masseuse.ai</AlertTitle>
                        <AlertDescription>Check this computer's connection. Masseuse.ai keeps trying on its own; nothing to redo.</AlertDescription>
                    </Alert>
                ) : null}
            </div>

            {/* One card, the phone's side of it: the room the code leads to, and the three things to do. */}
            <Card className="col-span-5 flex min-w-0 flex-col">
                <Scene scene="laptopBehindYou" className="aspect-[2/1] w-full" />
                <CardLabel icon={Smartphone} className="mt-5">
                    On your phone
                </CardLabel>
                <ol className="flex flex-col gap-3">
                    {STEPS.map((step, i) => (
                        <li key={step} className="flex items-center gap-3">
                            <span className="w-6 shrink-0 text-[24px] leading-none font-semibold tracking-tight text-rose tabular-nums">{i + 1}</span>
                            <span className="type-body min-w-0 font-semibold tracking-tight text-bone">{step}</span>
                        </li>
                    ))}
                </ol>
            </Card>
        </Page>
    );
}
