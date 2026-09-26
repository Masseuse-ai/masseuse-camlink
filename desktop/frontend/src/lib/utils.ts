// `cn`, the class-name join shadcn/ui components are written against: clsx
// for the conditionals, tailwind-merge so a caller's class wins over the
// component's own (`cn('h-10', className)` with `className="h-14"` is h-14).
// The shadcn components live in src/components/ui and import this; the
// app's own pieces (src/ui) may use it too.

import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

export function cn(...inputs: ClassValue[]): string {
    return twMerge(clsx(inputs));
}
