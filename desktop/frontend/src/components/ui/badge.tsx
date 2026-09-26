// shadcn/ui Badge (Base UI style): a small pill for a word or two. The
// registry's variants are kept (default on the rose primary, secondary on
// translucent white, outline, destructive) and the brand's tints added:
// `mint`, `rose`, `amber` and `ember` are the colour on a translucent well
// of itself, the way the status chip's dot reads a state (the posture
// label in the session HUD, screens/SessionHud.tsx). Hover states are
// left out: these are on a phone. Renders a <span>; `render` swaps the
// element (Base UI's useRender).
//
// Source: https://ui.shadcn.com/docs/components/base/badge (cn from
// @/lib/utils, the registry's "cn" alias).

import { mergeProps } from '@base-ui/react/merge-props';
import { useRender } from '@base-ui/react/use-render';
import { cva, type VariantProps } from 'class-variance-authority';

import { cn } from '@/lib/utils';

const badgeVariants = cva(
    'group/badge inline-flex h-5 w-fit shrink-0 items-center justify-center gap-1 overflow-hidden rounded-full border border-transparent px-2 py-0.5 text-xs font-medium whitespace-nowrap transition-colors focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 [&>svg]:pointer-events-none [&>svg]:size-3!',
    {
        variants: {
            variant: {
                default: 'bg-primary text-primary-foreground',
                secondary: 'bg-secondary text-secondary-foreground',
                destructive: 'bg-destructive/15 text-destructive focus-visible:ring-destructive/20',
                outline: 'border-border text-foreground',
                mint: 'bg-mint/15 text-mint',
                rose: 'bg-rose/15 text-rose',
                amber: 'bg-amber/15 text-amber',
                ember: 'bg-ember/15 text-ember',
            },
        },
        defaultVariants: {
            variant: 'default',
        },
    },
);

function Badge({ className, variant = 'default', render, ...props }: useRender.ComponentProps<'span'> & VariantProps<typeof badgeVariants>) {
    return useRender({
        defaultTagName: 'span',
        props: mergeProps<'span'>({ className: cn(badgeVariants({ variant }), className) }, props),
        render,
        state: { slot: 'badge', variant },
    });
}

export { Badge, badgeVariants };
