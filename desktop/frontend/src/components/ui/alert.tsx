// shadcn/ui Alert on the brand's surfaces, with the tones the phone's
// status chip uses: neutral on translucent white, live in mint, warn in
// amber, alert in ember.
//
// Source: https://ui.shadcn.com/docs/components/base/alert

import { cva, type VariantProps } from 'class-variance-authority';
import type * as React from 'react';

import { cn } from '@/lib/utils';

const alertVariants = cva(
    'group/alert relative grid w-full gap-0.5 rounded-2xl px-4 py-3 text-left text-[14px] leading-snug ring-1 has-[>svg]:grid-cols-[auto_1fr] has-[>svg]:gap-x-3 *:[svg]:row-span-2 *:[svg]:translate-y-0.5 *:[svg]:text-current *:[svg:not([class*="size-"])]:size-4',
    {
        variants: {
            variant: {
                neutral: 'bg-white/6 text-bone ring-white/10',
                live: 'bg-mint/10 text-bone ring-mint/25 *:[svg]:text-mint',
                warn: 'bg-amber/10 text-bone ring-amber/30 *:[svg]:text-amber',
                alert: 'bg-ember/10 text-bone ring-ember/30 *:[svg]:text-ember',
            },
        },
        defaultVariants: { variant: 'neutral' },
    },
);

function Alert({ className, variant, ...props }: React.ComponentProps<'div'> & VariantProps<typeof alertVariants>) {
    return <div data-slot="alert" role="alert" className={cn(alertVariants({ variant }), className)} {...props} />;
}

function AlertTitle({ className, ...props }: React.ComponentProps<'div'>) {
    return <div data-slot="alert-title" className={cn('font-semibold tracking-tight group-has-[>svg]/alert:col-start-2', className)} {...props} />;
}

function AlertDescription({ className, ...props }: React.ComponentProps<'div'>) {
    return <div data-slot="alert-description" className={cn('text-[13px] leading-snug text-bone/70 group-has-[>svg]/alert:col-start-2', className)} {...props} />;
}

export { Alert, AlertTitle, AlertDescription };
