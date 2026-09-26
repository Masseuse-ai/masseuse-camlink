// shadcn/ui Tabs (Base UI style) as the brand's segmented well: the list is
// the translucent well, the active tab lit in rose, as the phone draws a
// choice among a few.
//
// Source: https://ui.shadcn.com/docs/components/base/tabs

import { Tabs as TabsPrimitive } from '@base-ui/react/tabs';

import { cn } from '@/lib/utils';

function Tabs({ className, orientation = 'horizontal', ...props }: TabsPrimitive.Root.Props) {
    return (
        <TabsPrimitive.Root
            data-slot="tabs"
            data-orientation={orientation}
            className={cn('group/tabs flex gap-4 data-horizontal:flex-col', className)}
            {...props}
        />
    );
}

function TabsList({ className, ...props }: TabsPrimitive.List.Props) {
    return (
        <TabsPrimitive.List
            data-slot="tabs-list"
            className={cn('inline-flex w-fit items-center gap-1 rounded-2xl bg-white/6 p-1 ring-1 ring-white/10', className)}
            {...props}
        />
    );
}

function TabsTrigger({ className, ...props }: TabsPrimitive.Tab.Props) {
    return (
        <TabsPrimitive.Tab
            data-slot="tabs-trigger"
            className={cn(
                'inline-flex h-9 items-center justify-center gap-2 rounded-xl px-4 text-[14px] font-semibold whitespace-nowrap text-bone/65 transition-colors outline-none hover:text-bone focus-visible:ring-3 focus-visible:ring-ring/50 disabled:pointer-events-none disabled:opacity-50 data-active:bg-rose/20 data-active:text-bone data-active:ring-1 data-active:ring-rose/40 [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0',
                className,
            )}
            {...props}
        />
    );
}

function TabsContent({ className, ...props }: TabsPrimitive.Panel.Props) {
    return <TabsPrimitive.Panel data-slot="tabs-content" className={cn('flex-1 outline-none', className)} {...props} />;
}

export { Tabs, TabsList, TabsTrigger, TabsContent };
