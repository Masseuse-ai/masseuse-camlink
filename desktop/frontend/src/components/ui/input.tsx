// shadcn/ui Input (Base UI style) on the brand's well: translucent white,
// the ring in rose when focused; text in it may be selected.
//
// Source: https://ui.shadcn.com/docs/components/base/input

import { Input as InputPrimitive } from '@base-ui/react/input';
import type * as React from 'react';

import { cn } from '@/lib/utils';

function Input({ className, type, ...props }: React.ComponentProps<'input'>) {
    return (
        <InputPrimitive
            type={type}
            data-slot="input"
            className={cn(
                'h-10 w-full min-w-0 rounded-xl bg-white/8 px-3.5 text-[14px] text-bone ring-1 ring-white/12 transition-[box-shadow,background-color] outline-none select-text placeholder:text-bone/40 focus-visible:bg-white/10 focus-visible:ring-2 focus-visible:ring-rose disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:ring-ember/70',
                className,
            )}
            {...props}
        />
    );
}

export { Input };
