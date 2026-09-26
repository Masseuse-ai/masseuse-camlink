// shadcn/ui Collapsible (Base UI style): a trigger and the panel it opens
// and closes, non-modal, so the picture and the controls around it stay
// tappable while the panel is open (the session HUD's tiles under its
// header, screens/SessionHud.tsx). The panel animates its height through
// Base UI's `--collapsible-panel-height` (measured by the primitive) and
// the `data-starting-style` / `data-ending-style` attributes it sets for
// the frames the transition runs on; a caller's className adds to those
// classes. The panel is kept mounted while closed (`keepMounted`), so
// the tiles keep their state and the reopen is only the height.
//
// Source: https://ui.shadcn.com/docs/components/base/collapsible (cn from
// @/lib/utils, the registry's "cn" alias).

import { Collapsible as CollapsiblePrimitive } from '@base-ui/react/collapsible';

import { cn } from '@/lib/utils';

function Collapsible({ ...props }: CollapsiblePrimitive.Root.Props) {
    return <CollapsiblePrimitive.Root data-slot="collapsible" {...props} />;
}

function CollapsibleTrigger({ ...props }: CollapsiblePrimitive.Trigger.Props) {
    return <CollapsiblePrimitive.Trigger data-slot="collapsible-trigger" {...props} />;
}

function CollapsibleContent({ className, ...props }: CollapsiblePrimitive.Panel.Props) {
    return (
        <CollapsiblePrimitive.Panel
            data-slot="collapsible-content"
            keepMounted
            className={cn(
                'h-[var(--collapsible-panel-height)] overflow-hidden transition-[height,opacity] duration-300 ease-out',
                'data-[starting-style]:h-0 data-[starting-style]:opacity-0 data-[ending-style]:h-0 data-[ending-style]:opacity-0',
                'motion-reduce:transition-none',
                className,
            )}
            {...props}
        />
    );
}

export { Collapsible, CollapsibleTrigger, CollapsibleContent };
