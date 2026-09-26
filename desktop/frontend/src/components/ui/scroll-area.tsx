// shadcn/ui Scroll Area (Base UI style) in the brand: the native scrollbar
// is hidden and a thin thumb of bone is drawn instead, on no track, only
// while the pointer is over the list or it is scrolling, the way an overlay
// scrollbar behaves; at rest there is nothing. The viewport gives its
// content a little room on every side, so a card's ring (a box shadow
// drawn outside its box) is not clipped by the scrolling box at the top or
// the sides, and keeps a gutter at the right for the thumb so nothing sits
// under it; the root's negative margins pay that room back, so the list
// sits where it would without the scroll area. A ScrollArea fills the box
// it is given: put it in a column with `min-h-0 flex-1`.
//
// Source: https://ui.shadcn.com/docs/components/base/scroll-area

import { ScrollArea as ScrollAreaPrimitive } from '@base-ui/react/scroll-area';

import { cn } from '@/lib/utils';

function ScrollArea({ className, children, viewportClassName, ...props }: ScrollAreaPrimitive.Root.Props & { viewportClassName?: string }) {
    return (
        <ScrollAreaPrimitive.Root data-slot="scroll-area" className={cn('relative -mx-1 -mt-1 min-h-0', className)} {...props}>
            <ScrollAreaPrimitive.Viewport
                data-slot="scroll-area-viewport"
                className={cn('size-full rounded-[inherit] pt-1 pr-3 pb-1 pl-1 outline-none focus-visible:ring-3 focus-visible:ring-ring/50', viewportClassName)}
            >
                {children}
            </ScrollAreaPrimitive.Viewport>
            <ScrollBar />
            <ScrollAreaPrimitive.Corner />
        </ScrollAreaPrimitive.Root>
    );
}

function ScrollBar({ className, orientation = 'vertical', ...props }: ScrollAreaPrimitive.Scrollbar.Props) {
    return (
        <ScrollAreaPrimitive.Scrollbar
            data-slot="scroll-area-scrollbar"
            data-orientation={orientation}
            orientation={orientation}
            className={cn(
                // Hidden at rest; shown while the pointer is over the area or it scrolls (Base UI's data attributes).
                'flex touch-none p-0.5 opacity-0 transition-opacity duration-300 select-none data-hovering:opacity-100 data-scrolling:opacity-100 data-horizontal:h-2 data-horizontal:flex-col data-vertical:h-full data-vertical:w-2',
                className,
            )}
            {...props}
        >
            <ScrollAreaPrimitive.Thumb data-slot="scroll-area-thumb" className="relative flex-1 rounded-full bg-bone/25 transition-colors hover:bg-bone/40" />
        </ScrollAreaPrimitive.Scrollbar>
    );
}

export { ScrollArea, ScrollBar };
