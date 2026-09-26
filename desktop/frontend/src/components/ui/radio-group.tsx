// shadcn/ui Radio Group (Base UI style) in the brand's colours: an outline
// on translucent white, rose when chosen. The device and unit cards
// (src/ui) wrap each item in a card the whole of which is the target.
//
// Source: https://ui.shadcn.com/docs/components/base/radio-group

import { Radio as RadioPrimitive } from '@base-ui/react/radio';
import { RadioGroup as RadioGroupPrimitive } from '@base-ui/react/radio-group';

import { cn } from '@/lib/utils';

function RadioGroup({ className, ...props }: RadioGroupPrimitive.Props) {
    return <RadioGroupPrimitive data-slot="radio-group" className={cn('grid w-full gap-2', className)} {...props} />;
}

function RadioGroupItem({ className, ...props }: RadioPrimitive.Root.Props) {
    return (
        <RadioPrimitive.Root
            data-slot="radio-group-item"
            className={cn(
                'peer relative flex aspect-square size-[18px] shrink-0 rounded-full border border-white/25 bg-white/5 transition-colors outline-none focus-visible:ring-3 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 data-checked:border-rose data-checked:bg-rose',
                className,
            )}
            {...props}
        >
            <RadioPrimitive.Indicator data-slot="radio-group-indicator" className="flex size-full items-center justify-center">
                <span className="size-2 rounded-full bg-ink" />
            </RadioPrimitive.Indicator>
        </RadioPrimitive.Root>
    );
}

export { RadioGroup, RadioGroupItem };
