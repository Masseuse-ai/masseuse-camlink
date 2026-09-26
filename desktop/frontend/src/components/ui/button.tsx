// shadcn/ui Button (Base UI style), its variants the brand's, as the
// masseuse.ai web app draws them: the primary pill in the rose gradient
// with its glow (the one action on a screen), the ghost pill on translucent
// white (a second action), the quiet text button (a skip, a change), and
// the round icon button. The phone's sizes are `lg`; a window is read from
// further away and driven with a pointer, so the default here is `md` and
// `sm` fits inside a card. The press is a scale, as on the phone; the
// pointer brightens a ghost or quiet button a little.
//
// Source: https://ui.shadcn.com/docs/components/base/button

import { Button as ButtonPrimitive } from '@base-ui/react/button';
import { cva, type VariantProps } from 'class-variance-authority';
import type { ReactNode } from 'react';

import { cn } from '@/lib/utils';

const buttonVariants = cva(
    'inline-flex shrink-0 items-center justify-center gap-2 rounded-full font-semibold tracking-tight transition-[transform,opacity,background-color,color] duration-200 outline-none select-none active:scale-[0.97] focus-visible:ring-3 focus-visible:ring-ring/50 disabled:opacity-50 disabled:active:scale-100 [&_svg]:pointer-events-none [&_svg]:shrink-0',
    {
        variants: {
            variant: {
                primary: 'text-ink bg-gradient-to-br from-rose to-rose-deep shadow-glow',
                ghost: 'text-bone bg-white/10 ring-1 ring-white/15 hover:bg-white/14',
                quiet: 'text-bone-dim bg-transparent hover:text-bone',
                icon: 'p-0 text-bone bg-black/35 ring-1 ring-white/15 hover:bg-white/10 active:scale-95',
            },
            size: {
                lg: '',
                md: '',
                sm: '',
            },
        },
        compoundVariants: [
            { variant: 'primary', size: 'lg', className: 'h-14 px-7 text-[17px]' },
            { variant: 'primary', size: 'md', className: 'h-11 px-6 text-[15px]' },
            { variant: 'primary', size: 'sm', className: 'h-9 px-4 text-[14px]' },
            { variant: 'ghost', size: 'lg', className: 'h-12 px-5 text-[15px]' },
            { variant: 'ghost', size: 'md', className: 'h-10 px-4 text-[14px]' },
            { variant: 'ghost', size: 'sm', className: 'h-8 px-3.5 text-[13px]' },
            { variant: 'quiet', size: 'lg', className: 'h-10 px-4 text-[14px]' },
            { variant: 'quiet', size: 'md', className: 'h-9 px-3 text-[14px]' },
            { variant: 'quiet', size: 'sm', className: 'h-8 px-2.5 text-[13px]' },
            { variant: 'icon', size: 'lg', className: 'h-11 w-11' },
            { variant: 'icon', size: 'md', className: 'h-9 w-9' },
            { variant: 'icon', size: 'sm', className: 'h-8 w-8' },
        ],
        defaultVariants: { variant: 'primary', size: 'md' },
    },
);

type ButtonProps = ButtonPrimitive.Props &
    VariantProps<typeof buttonVariants> & {
        /** A leading icon, before the label. */
        icon?: ReactNode;
    };

export function Button({ className, variant = 'primary', size = 'md', icon, children, ...props }: ButtonProps) {
    return (
        <ButtonPrimitive data-slot="button" className={cn(buttonVariants({ variant, size }), className)} {...props}>
            {icon}
            {children}
        </ButtonPrimitive>
    );
}

/** A round icon-only control; `label` names it for assistive technology and the tooltip. */
export function IconButton({
    label,
    className,
    children,
    size = 'md',
    ...props
}: Omit<ButtonProps, 'variant' | 'icon'> & { label: string; children: ReactNode }) {
    return (
        <Button variant="icon" size={size} aria-label={label} title={label} className={className} {...props}>
            {children}
        </Button>
    );
}

export { buttonVariants };
