// The window's chrome, three rows on one grid. The title strip: the wordmark
// alone at the left and, at the right, a word about the service only while
// there is one to say (reaching it, reconnecting); connected, it says
// nothing, since Ready's Session box carries the link's standing where it
// matters. Under it the masthead (ui/Masthead.tsx: the tagline and the
// figures), then the steps (ui/StepBar.tsx), both on the content inset. On
// macOS the native title bar is hidden and the strip is where the traffic
// lights sit (main.go, MacTitleBarHiddenInset), so the wordmark insets for
// them and the strip drags the window; on Windows and Linux the native
// title bar and menu bar are above, and the strip is shorter.

import { LoaderCircle } from 'lucide-react';

import { useAppState } from '../bridge/store';
import { Wordmark } from '../brand/Wordmark';
import { cn } from '@/lib/utils';
import { Masthead } from './Masthead';

/** The one line worth a word: while the service is out of reach. */
function ServiceWord({ online }: { online: boolean | null }) {
    if (online === true) return null;
    return (
        <span className="no-drag type-caption inline-flex items-center gap-2 text-bone/60" role="status">
            {online === null ? <LoaderCircle className="lucide h-3.5 w-3.5 animate-spin text-rose" strokeWidth={2.4} /> : <span className="h-2 w-2 shrink-0 rounded-full bg-amber" />}
            {online === null ? 'Reaching masseuse.ai…' : 'Reconnecting to masseuse.ai…'}
        </span>
    );
}

export function TopBar() {
    const { platform, online, blocked } = useAppState();
    const mac = platform === 'darwin';
    return (
        <header className="drag-region shrink-0">
            <div className={cn('flex items-center justify-between pr-inset', mac ? 'h-[52px] pl-[86px]' : 'h-11 pl-inset')}>
                <Wordmark className="w-[126px] text-bone" />
                {blocked ? null : <ServiceWord online={online} />}
            </div>
            {/* Not over an error: the Blocked screen has the window to itself, as it has without the steps. */}
            {blocked ? null : <Masthead />}
        </header>
    );
}
