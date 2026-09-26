// shadcn/ui Progress (Base UI style), the brand's: a hairline track on
// translucent white, the filled part in the rose gradient of the primary
// button, the fill moving with a transition when the value moves (the
// unit's level of its max in the session HUD, screens/SessionHud.tsx).
// The root is a flex row so a label and the value can sit on the track's
// line; the track and the indicator are its children by default, a caller
// composing its own passes them itself.
//
// Source: https://ui.shadcn.com/docs/components/base/progress (cn from
// @/lib/utils, the registry's "cn" alias).

import { Progress as ProgressPrimitive } from '@base-ui/react/progress';

import { cn } from '@/lib/utils';

function Progress({ className, children, value, ...props }: ProgressPrimitive.Root.Props) {
    return (
        <ProgressPrimitive.Root value={value} data-slot="progress" className={cn('flex flex-wrap items-center gap-3', className)} {...props}>
            {children}
            <ProgressTrack>
                <ProgressIndicator />
            </ProgressTrack>
        </ProgressPrimitive.Root>
    );
}

function ProgressTrack({ className, ...props }: ProgressPrimitive.Track.Props) {
    return (
        <ProgressPrimitive.Track
            data-slot="progress-track"
            className={cn('relative flex h-1 w-full items-center overflow-x-hidden rounded-full bg-white/12', className)}
            {...props}
        />
    );
}

function ProgressIndicator({ className, ...props }: ProgressPrimitive.Indicator.Props) {
    return (
        <ProgressPrimitive.Indicator
            data-slot="progress-indicator"
            className={cn('h-full rounded-full bg-gradient-to-r from-rose-deep to-rose transition-[width] duration-500 ease-out', className)}
            {...props}
        />
    );
}

function ProgressLabel({ className, ...props }: ProgressPrimitive.Label.Props) {
    return <ProgressPrimitive.Label data-slot="progress-label" className={cn('text-[13px] font-semibold text-bone', className)} {...props} />;
}

function ProgressValue({ className, ...props }: ProgressPrimitive.Value.Props) {
    return (
        <ProgressPrimitive.Value
            data-slot="progress-value"
            className={cn('ml-auto font-mono text-[12px] tabular-nums text-bone/70', className)}
            {...props}
        />
    );
}

export { Progress, ProgressTrack, ProgressIndicator, ProgressLabel, ProgressValue };
