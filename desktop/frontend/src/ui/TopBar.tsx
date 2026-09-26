// The window's top bar: the wordmark, how the connector stands with
// masseuse.ai, and under the wordmark, on every step, why the person is
// here at all (ui/Punchline.tsx: the tagline and the figures). On macOS the
// native title bar is hidden and this bar is where the traffic lights sit
// (main.go, MacTitleBarHiddenInset), so it insets for them and drags the
// window; on Windows and Linux the native title bar and menu bar are above.

import { useAppState } from '../bridge/store';
import { Wordmark } from '../brand/Wordmark';
import { Punchline } from './Punchline';
import { StatusChip, type Tone } from './StatusChip';

function serviceStatus(online: boolean | null): { tone: Tone; text: string } {
    if (online === null) return { tone: 'busy', text: 'Reaching masseuse.ai…' };
    if (online) return { tone: 'live', text: 'Connected to masseuse.ai' };
    return { tone: 'warn', text: 'Reconnecting to masseuse.ai…' };
}

export function TopBar() {
    const { platform, online, blocked } = useAppState();
    const status = serviceStatus(online);
    // The wordmark's left edge, which the line under it shares.
    const inset = platform === 'darwin' ? 'pl-[86px]' : 'pl-6';
    return (
        <header className="drag-region shrink-0">
            <div className={`flex h-[52px] items-center justify-between pr-4 ${inset}`}>
                <Wordmark className="w-[126px] text-bone" />
                <div className="no-drag flex items-center gap-2">
                    {blocked ? null : (
                        <StatusChip tone={status.tone}>{status.text}</StatusChip>
                    )}
                </div>
            </div>
            {/* Tucked under the wordmark, its subtitle. Not over an error: the Blocked screen has the window to itself, as it has without the steps. */}
            {blocked ? null : <Punchline className={`-mt-2.5 pr-4 pb-1 ${inset}`} />}
        </header>
    );
}
