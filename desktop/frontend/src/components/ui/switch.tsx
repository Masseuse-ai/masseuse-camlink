// shadcn/ui Switch (Base UI style), in the brand: a translucent track that
// turns rose when on, the bone thumb turning ink over it, the registry's
// two sizes (`default` for a control read from a chair, `sm` inside dense
// rows) and its widened hit area, since a switch is a small thing to aim a
// pointer at. Dark only, so the registry's dark variants are the one set.
//
// Source: https://ui.shadcn.com/docs/components/base/switch

import { Switch as SwitchPrimitive } from '@base-ui/react/switch';

import { cn } from '@/lib/utils';

function Switch({ className, size = 'default', ...props }: SwitchPrimitive.Root.Props & { size?: 'sm' | 'default' }) {
    return (
        <SwitchPrimitive.Root
            data-slot="switch"
            data-size={size}
            className={cn(
                'peer group/switch relative inline-flex shrink-0 items-center rounded-full border border-transparent transition-all outline-none',
                // The hit area reaches past the track on every side.
                'after:absolute after:-inset-x-3 after:-inset-y-2',
                'focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50',
                'data-[size=default]:h-[18.4px] data-[size=default]:w-[32px] data-[size=sm]:h-[14px] data-[size=sm]:w-[24px]',
                'data-checked:bg-rose data-unchecked:bg-white/15 data-disabled:cursor-not-allowed data-disabled:opacity-50',
                className,
            )}
            {...props}
        >
            <SwitchPrimitive.Thumb
                data-slot="switch-thumb"
                className={cn(
                    'pointer-events-none block rounded-full bg-bone shadow-[0_1px_3px_rgb(0_0_0/0.4)] ring-0 transition-transform data-checked:bg-ink',
                    'group-data-[size=default]/switch:size-4 group-data-[size=sm]/switch:size-3',
                    'group-data-[size=default]/switch:data-checked:translate-x-[calc(100%-2px)] group-data-[size=sm]/switch:data-checked:translate-x-[calc(100%-2px)]',
                    'group-data-[size=default]/switch:data-unchecked:translate-x-0 group-data-[size=sm]/switch:data-unchecked:translate-x-0',
                )}
            />
        </SwitchPrimitive.Root>
    );
}

export { Switch };
