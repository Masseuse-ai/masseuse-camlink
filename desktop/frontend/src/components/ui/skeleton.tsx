// shadcn/ui Skeleton: a pulsing well the shape of what is still to come,
// on the brand's translucent white (the value slot of a session HUD tile
// while its signal calibrates, screens/SessionHud.tsx). Under Reduce
// Motion it holds still.
//
// Source: https://ui.shadcn.com/docs/components/base/skeleton (cn from
// @/lib/utils, the registry's "cn" alias).

import { cn } from '@/lib/utils';

function Skeleton({ className, ...props }: React.ComponentProps<'div'>) {
    return <div data-slot="skeleton" className={cn('animate-pulse rounded-md bg-white/12 motion-reduce:animate-none', className)} {...props} />;
}

export { Skeleton };
