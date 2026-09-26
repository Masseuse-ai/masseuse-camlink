// Blocked: the program cannot run as it is, and says why, whole-window,
// with the one thing to do about it.

import { FolderOpen, OctagonX, Power, RefreshCw } from 'lucide-react';

import { Button } from '@/components/ui/button';
import { useAppState } from '../bridge/store';
import { quit, restartConnector, revealStateDir } from '../shell/shell';
import { Well } from '../ui/Card';

export function Blocked() {
    const { blocked, shell } = useAppState();
    if (!blocked) return null;

    const copy = (() => {
        switch (blocked.kind) {
            case 'already-running':
                return {
                    icon: OctagonX,
                    title: 'Masseuse.ai is already running, in another window',
                    text: 'Close that one first. Two copies would fight over the camera and the unit, so this one stays out of the way.',
                };
            case 'connector-stopped':
                return {
                    icon: Power,
                    title: 'Masseuse.ai stopped',
                    text: 'The program behind this window ended unexpectedly. The camera is off and the unit was released. Its last lines are below; starting again is safe.',
                };
            case 'state-dir-unwritable':
                return {
                    icon: FolderOpen,
                    title: 'The state folder cannot be written',
                    text: `Masseuse.ai keeps its identity, pairings and choices in ${shell.stateDir || 'its state folder'} and could not write there. Check the folder's permissions, or free some space.`,
                };
        }
    })();
    const Icon = copy.icon;

    return (
        <div className="screen-body flex flex-1 items-center justify-center px-8 pb-10">
            <div className="w-full max-w-[560px]">
                <span className="mb-5 inline-flex h-12 w-12 items-center justify-center rounded-full bg-ember/15 text-ember ring-1 ring-ember/30">
                    <Icon className="lucide h-6 w-6" strokeWidth={2.2} />
                </span>
                <h1 className="text-[26px] leading-tight font-semibold tracking-tight text-bone">{copy.title}</h1>
                <p className="mt-2 text-[15px] leading-snug text-bone/75">{copy.text}</p>
                {blocked.detail ? (
                    <Well className="mt-4 select-text">
                        <pre className="max-h-40 overflow-auto font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-all text-bone/70">{blocked.detail}</pre>
                    </Well>
                ) : null}
                <div className="mt-6 flex items-center gap-2">
                    {blocked.kind === 'connector-stopped' ? (
                        <Button icon={<RefreshCw className="lucide h-4 w-4" strokeWidth={2.4} />} onClick={() => void restartConnector()}>
                            Start again
                        </Button>
                    ) : null}
                    {blocked.kind === 'state-dir-unwritable' ? (
                        <Button variant="ghost" icon={<FolderOpen className="lucide h-4 w-4" strokeWidth={2.4} />} onClick={() => void revealStateDir()}>
                            Open the state folder
                        </Button>
                    ) : null}
                    <Button variant={blocked.kind === 'already-running' ? 'primary' : 'quiet'} onClick={() => void quit()}>
                        Quit
                    </Button>
                </div>
            </div>
        </div>
    );
}
