// The first run's four steps, in a row under the top bar: where the person
// is in rose, what is done in mint with a check, what is to come dimmed.
//
// A step is done by the connector's facts alone: Pair once a phone is
// paired, Cameras once a camera the connector can serve is chosen and the
// person has moved on, Unit once a unit is found and the person has moved
// on. A step ahead opens only when every step before it is done (the
// footers move forward on the same terms; there is no skipping), so nothing
// past Pair can be reached before a phone has paired: a paired phone can see
// this computer's camera, so pairing comes first. Going back is always
// allowed. Once the setup is done every step is a place to change a choice,
// and Ready is home.

import { Check } from 'lucide-react';

import { useAppState, useDispatch, type Step } from '../bridge/store';
import { cn } from '@/lib/utils';

const STEPS: { id: Step; label: string }[] = [
    { id: 'pair', label: 'Pair' },
    { id: 'camera', label: 'Cameras and microphone' },
    { id: 'unit', label: 'Unit' },
    { id: 'home', label: 'Ready' },
];

export function StepBar() {
    const state = useAppState();
    const dispatch = useDispatch();
    const current = STEPS.findIndex((s) => s.id === state.step);

    const done = (id: Step, index: number): boolean => {
        if (state.setupDone) return id !== state.step;
        switch (id) {
            case 'pair':
                return state.phones > 0;
            case 'camera':
                return Boolean(state.source?.ready) && index < current;
            case 'unit':
                return Boolean(state.unit?.connected) && index < current;
            default:
                return false;
        }
    };
    // Every step before this one is done: the gate a step ahead opens on.
    const opened = (index: number): boolean => STEPS.slice(0, index).every((s, k) => done(s.id, k));

    return (
        // A 40-pt row on the content inset, the first pill's numeral flush with
        // the masthead and the titles under it (the pill's own padding pulled back).
        <nav aria-label="Setup steps" className="flex h-10 shrink-0 items-center gap-1 px-inset">
            {STEPS.map((step, i) => {
                const isCurrent = step.id === state.step;
                const isDone = done(step.id, i);
                const reachable = state.setupDone || i <= current || opened(i);
                return (
                    <div key={step.id} className="flex items-center gap-1">
                        {i > 0 ? <span aria-hidden className={cn('mx-1 h-px w-6', isDone || isCurrent ? 'bg-white/25' : 'bg-white/10')} /> : null}
                        <button
                            type="button"
                            disabled={!reachable}
                            aria-current={isCurrent ? 'step' : undefined}
                            onClick={() => dispatch({ type: 'ui/go', step: step.id })}
                            className={cn(
                                'inline-flex h-8 items-center gap-2 rounded-full px-3 text-[13px] font-medium transition-colors outline-none focus-visible:ring-3 focus-visible:ring-ring/50 disabled:cursor-default',
                                i === 0 && '-ml-3',
                                isCurrent && 'bg-rose/15 text-bone ring-1 ring-rose/40',
                                !isCurrent && isDone && 'text-bone/75 hover:text-bone',
                                !isCurrent && !isDone && 'text-bone/40',
                                !isCurrent && reachable && 'hover:bg-white/6',
                            )}
                        >
                            <span
                                className={cn(
                                    'inline-flex h-5 w-5 items-center justify-center rounded-full text-[11px] font-semibold tabular-nums',
                                    isCurrent && 'bg-rose text-ink',
                                    !isCurrent && isDone && 'bg-mint/20 text-mint',
                                    !isCurrent && !isDone && 'bg-white/8 text-bone/50',
                                )}
                            >
                                {isDone && !isCurrent ? <Check className="lucide h-3 w-3" strokeWidth={2.6} /> : i + 1}
                            </span>
                            {step.label}
                        </button>
                    </div>
                );
            })}
        </nav>
    );
}
