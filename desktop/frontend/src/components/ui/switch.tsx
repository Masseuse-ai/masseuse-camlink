// shadcn/ui Switch (Base UI style): translucent track, rose when on.
//
// Source: https://ui.shadcn.com/docs/components/base/switch

import { Switch as SwitchPrimitive } from '@base-ui/react/switch';

import { cn } from '@/lib/utils';

function Switch({ className, ...props }: SwitchPrimitive.Root.Props) {
    return (
        <SwitchPrimitive.Root
            data-slot="switch"
            className={cn(
                'peer group/switch relative inline-flex h-[22px] w-[38px] shrink-0 items-center rounded-full border border-transparent transition-colors outline-none focus-visible:ring-3 focus-visible:ring-ring/50 data-checked:bg-rose data-unchecked:bg-white/15 data-disabled:cursor-not-allowed data-disabled:opacity-50',
                className,
            )}
            {...props}
        >
            <SwitchPrimitive.Thumb
                data-slot="switch-thumb"
                className="pointer-events-none block size-[18px] rounded-full bg-bone shadow-[0_1px_3px_rgb(0_0_0/0.4)] transition-transform data-checked:translate-x-[17px] data-checked:bg-ink data-unchecked:translate-x-[1px]"
            />
        </SwitchPrimitive.Root>
    );
}

export { Switch };
