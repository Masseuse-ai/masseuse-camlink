// About: what this program is, which version, whose identity key it holds,
// and where to read how it stays private, how to verify the download, and
// how to report a problem. Opened from the application menu (main.go emits
// a "menu" event) so it is the same on every platform.

import { BookOpen, Code, ExternalLink, ShieldAlert, ShieldCheck } from 'lucide-react';

import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { useAppState, useDispatch } from '../bridge/store';
import mark from '../brand/mark.svg';
import { Wordmark } from '../brand/Wordmark';
import { openLink, type LinkName } from '../shell/shell';

const LINKS: { name: LinkName; label: string; icon: typeof BookOpen }[] = [
    { name: 'privacy', label: 'How it stays private', icon: ShieldCheck },
    { name: 'verify', label: 'Verify this download', icon: BookOpen },
    { name: 'security', label: 'Report a security issue', icon: ShieldAlert },
    { name: 'source', label: 'Open source on GitHub', icon: Code },
];

export function About() {
    const { aboutOpen, hello, shell } = useAppState();
    const dispatch = useDispatch();
    return (
        <Dialog open={aboutOpen} onOpenChange={(open) => dispatch({ type: 'ui/about', open })}>
            <DialogContent className="sm:max-w-[440px]">
                <div className="flex items-center gap-4">
                    <img src={mark} alt="" width={56} height={56} className="h-14 w-14 shrink-0 rounded-[14px] shadow-lg ring-1 ring-white/10" />
                    <Wordmark className="w-[170px] text-bone" />
                </div>
                <DialogHeader>
                    <DialogTitle>Masseuse.ai for your computer</DialogTitle>
                    <DialogDescription>
                        Sends this computer's camera and microphone, or a camera on your network, to the one verified enclave your session is using, and puts your stimulation unit in the session's hands, within bounds this program holds to.
                    </DialogDescription>
                </DialogHeader>
                <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 rounded-2xl bg-white/6 p-3.5 font-mono text-[12px] ring-1 ring-white/10 select-text">
                    <dt className="text-bone/45">connector</dt>
                    <dd className="text-bone/85">masseuse-camlink {hello?.version ?? '…'}</dd>
                    <dt className="text-bone/45">window</dt>
                    <dd className="text-bone/85">{shell.version}</dd>
                    <dt className="text-bone/45">identity</dt>
                    <dd className="text-bone/85">{hello ? `${hello.identity}…` : '…'}</dd>
                    <dt className="text-bone/45">state</dt>
                    <dd className="truncate text-bone/85" title={hello?.stateDir ?? shell.stateDir}>
                        {hello?.stateDir ?? shell.stateDir}
                    </dd>
                </dl>
                <p className="text-[12px] leading-snug text-bone/55">Free and open source · Signed and verifiable · Every release is reproducible from its tag.</p>
                {hello?.awake ? <p className="text-[12px] leading-snug text-bone/55">This computer stays awake while this window is open, so a laptop at the foot of the bed does not sleep while the room is prepared; the screen may go dark. Keep the lid open.</p> : null}
                <div className="grid grid-cols-1 gap-1.5">
                    {LINKS.map(({ name, label, icon: Icon }) => (
                        <Button key={name} variant="ghost" size="sm" className="w-full justify-start rounded-xl" icon={<Icon className="lucide h-4 w-4 text-bone/70" strokeWidth={2.2} />} onClick={() => void openLink(name)}>
                            <span className="flex-1 text-left">{label}</span>
                            <ExternalLink className="lucide h-3 w-3 text-bone/40" strokeWidth={2.4} />
                        </Button>
                    ))}
                </div>
                <p className="text-[11px] leading-snug text-bone/45">Copyright 2026 FemLed. Apache License 2.0; bundled ffmpeg LGPL 2.1 (see THIRD_PARTY.md). Charis SIL under the SIL Open Font License.</p>
            </DialogContent>
        </Dialog>
    );
}
