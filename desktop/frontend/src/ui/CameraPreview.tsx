// Ready's preview of the camera behind the person: what a session would be
// sent, live, while no session has the camera, so the camera can be placed
// before the session starts. The picture is the connector's (the camera in
// the session's mode, small JPEGs; cmd/masseuse-camlink/preview.go) and
// stays on this computer. It is asked for (usePreviewLease) while Ready is
// on screen, the window visible and in use (usePresence, previewIdleMs),
// the switch on and the camera one of the computer's own, allowed; it lets
// go the moment any of that ends, and Hide keeps it off for this run. It stands
// where the illustration of the setup stood, in the 16:9 the room composes
// the camera into, the picture fitted as the room fits it (bars beside a
// square camera's) and unmirrored, as the room sees it. Larger shows it
// across the window, to be read from the bed.

import { Eye, EyeOff, Loader2, Maximize2, MonitorOff } from 'lucide-react';
import { useCallback, useEffect, useRef, useState, type CSSProperties, type ReactNode, type RefObject } from 'react';

import { Button, IconButton } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { cn } from '@/lib/utils';
import { switchedOff, useAppState, useDispatch, usePreviewLease, usePreviewPictures, type AppState } from '../bridge/store';
import { isVirtualCamera } from './DeviceCard';
import { Scene } from './Scene';

/** What the masseuse asks for as a session starts, said under the larger picture. */
const POSE = 'Lie on your front: your shoulders to your feet in the picture, your back uncovered.';

/** The same, as the way to place the camera: Ready says it beside the picture (Home.tsx). */
export const PLACE = 'Place the camera so that, lying on your front, your shoulders to your feet are in the picture, your back uncovered.';

/** What a screen reader is told of the tile, as it changes. */
function spoken(view: View): string {
    switch (view.kind) {
        case 'live':
            return 'The camera behind you is live in the window, on this computer only.';
        case 'starting':
            return 'Turning the camera on.';
        case 'unused':
            return "Move the pointer to see the camera's picture.";
        case 'paused':
            return 'The picture is paused while the window is not in use.';
        case 'hidden':
            return 'The picture is hidden.';
        case 'quiet':
        case 'trouble':
            return view.text;
    }
}

type View = { kind: 'live' | 'starting' | 'unused' | 'paused' | 'hidden' } | { kind: 'quiet' | 'trouble'; text: string };

/** A connector's reason as a sentence: capitalised, with its stop. */
function sentence(s: string): string {
    const t = s.trim();
    const capped = t.charAt(0).toUpperCase() + t.slice(1);
    return /[.!?…]$/.test(capped) ? capped : `${capped}.`;
}

/** What the tile shows, and whether the preview is asked for. */
function viewOf(state: AppState, presence: Presence): { view: View; asks: boolean } {
    const { source, camera, preview, platform, media, previewHidden } = state;
    const quiet = (text: string) => ({ view: { kind: 'quiet', text } as View, asks: false });
    if (camera.on) return quiet('Your session has the camera.');
    if (!source) return quiet('Finding the camera…');
    if (source.kind === 'camera') return quiet('A camera on your network shows its picture in its own app.');
    if (!source.ready) return quiet(sentence(source.note ?? 'no camera is connected'));
    if (switchedOff(camera)) return quiet('Switch the camera on above to see its picture here.');
    // The card beside it says what macOS is doing and where it is changed.
    if (platform === 'darwin' && media && media.camera !== 'authorized') return quiet('The picture shows here once macOS allows the camera.');
    if (previewHidden) return { view: { kind: 'hidden' }, asks: false };
    if (presence === 'unused') return { view: { kind: 'unused' }, asks: false };
    if (presence === 'away') return { view: { kind: 'paused' }, asks: false };
    if (preview.state === 'live') return { view: { kind: 'live' }, asks: true };
    if (preview.state === 'unavailable' && preview.note) return { view: { kind: 'trouble', text: sentence(preview.note) }, asks: true };
    return { view: { kind: 'starting' }, asks: true };
}

/** Whether someone is at the window: not yet since it opened, using it, or away. */
type Presence = 'unused' | 'present' | 'away';

/**
 * Whether the window has been used in the last `idleMs`: a press, a key,
 * the wheel, or the pointer moving (to another place: a window that opens
 * under a pointer standing still may be told it moved). Opening, or coming
 * to the front, is not use: the program opens on its own after an update,
 * the window to the front, with no one in the room.
 */
function usePresence(idleMs: number): Presence {
    const [presence, setPresence] = useState<Presence>('unused');
    useEffect(() => {
        let last = 0;
        let at: { x: number; y: number } | null = null;
        const used = () => {
            last = Date.now();
            setPresence('present');
        };
        const moved = (e: PointerEvent) => {
            const was = at;
            at = { x: e.screenX, y: e.screenY };
            if (was && (was.x !== at.x || was.y !== at.y)) used();
        };
        const kinds = ['pointerdown', 'keydown', 'wheel'] as const;
        for (const kind of kinds) window.addEventListener(kind, used, { passive: true });
        window.addEventListener('pointermove', moved, { passive: true });
        const timer = setInterval(
            () => {
                if (last) setPresence(Date.now() - last < idleMs ? 'present' : 'away');
            },
            Math.min(15_000, Math.max(1_000, idleMs / 6)),
        );
        return () => {
            for (const kind of kinds) window.removeEventListener(kind, used);
            window.removeEventListener('pointermove', moved);
            clearInterval(timer);
        };
    }, [idleMs]);
    return presence;
}

/**
 * Whether the picture in `img` has been all but black for three seconds
 * running: a covered lens, a dark room, or a virtual camera whose program
 * sends nothing. It is sampled once a second, at 32 x 18.
 */
function useDarkPicture(img: RefObject<HTMLImageElement | null>, watching: boolean): boolean {
    const [dark, setDark] = useState(false);
    useEffect(() => {
        if (!watching) return;
        const canvas = document.createElement('canvas');
        canvas.width = 32;
        canvas.height = 18;
        const g = canvas.getContext('2d', { willReadFrequently: true });
        let darkFor = 0;
        const timer = setInterval(() => {
            const el = img.current;
            if (!g || !el || !el.complete || el.naturalWidth === 0) return;
            g.drawImage(el, 0, 0, 32, 18);
            const px = g.getImageData(0, 0, 32, 18).data;
            let sum = 0;
            let brightest = 0;
            for (let i = 0; i < px.length; i += 4) {
                const y = 0.2126 * px[i]! + 0.7152 * px[i + 1]! + 0.0722 * px[i + 2]!;
                sum += y;
                brightest = Math.max(brightest, y);
            }
            darkFor = sum / (px.length / 4) < 8 && brightest < 24 ? darkFor + 1 : 0;
            setDark(darkFor >= 3);
        }, 1000);
        return () => {
            clearInterval(timer);
            setDark(false);
        };
    }, [img, watching]);
    return dark;
}

/**
 * The live picture, filling the frame as the room fits it: each picture
 * decoded before it replaces the one shown, so it never flickers, and one
 * that comes while the last still decodes dropped (the next is an eighth
 * of a second away). It fades in over the illustration with its first.
 */
function Picture({ camera }: { camera?: string }) {
    const img = useRef<HTMLImageElement>(null);
    const decoding = useRef(false);
    const [drawn, setDrawn] = useState(false);
    usePreviewPictures(
        useCallback((jpeg: string) => {
            const el = img.current;
            if (!el || decoding.current) return;
            decoding.current = true;
            const next = new Image();
            next.src = `data:image/jpeg;base64,${jpeg}`;
            next.decode()
                .then(() => {
                    el.src = next.src;
                    setDrawn(true);
                })
                .catch(() => {})
                .finally(() => {
                    decoding.current = false;
                });
        }, []),
    );
    const dark = useDarkPicture(img, drawn);
    const virtual = Boolean(camera && isVirtualCamera({ kind: 'video', id: '', name: camera }));
    return (
        <div className={cn('absolute inset-0 bg-black transition-opacity duration-500 motion-reduce:transition-none', drawn ? 'opacity-100' : 'opacity-0')}>
            <img ref={img} alt={camera ? `What ${camera} sees, live` : 'What the camera sees, live'} draggable={false} className="h-full w-full object-contain" />
            {dark ? (
                <div className="absolute inset-0 flex flex-col items-center justify-center gap-1 px-6 text-center">
                    <MonitorOff className="lucide mb-1 h-5 w-5 text-amber" strokeWidth={2.2} />
                    <p className="type-secondary font-semibold text-bone">The picture is black</p>
                    <p className="type-caption max-w-[34ch] text-bone/65">{virtual ? 'A virtual camera shows only what its program sends: start it there.' : 'Is the lens covered, or the room too dark?'}</p>
                </div>
            ) : null}
        </div>
    );
}

/** A word over the picture: the status chip's pill, a size down in the tile, and not a control. */
function Pill({ icon, children, large }: { icon: ReactNode; children: ReactNode; large?: boolean }) {
    return (
        <span className={cn('inline-flex items-center gap-2 rounded-full bg-black/45 font-medium whitespace-nowrap text-bone ring-1 ring-white/12 backdrop-blur-md', large ? 'px-3.5 py-2 text-[13px]' : 'px-3 py-1.5 text-[12px]')}>
            {icon}
            {children}
        </span>
    );
}

const dot = (tone: string) => <span className={cn('h-2 w-2 shrink-0 rounded-full', tone)} />;

/** What is said in the middle of a frame with no picture: a title, a line, and the one thing to do, if any. */
function Note({ title, line, titleTone = 'text-bone', action }: { title?: string; line: string; titleTone?: string; action?: ReactNode }) {
    return (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-1 bg-ink/40 px-6 text-center">
            {title ? <p className={cn('type-secondary font-semibold', titleTone)}>{title}</p> : null}
            <p className="type-caption max-w-[38ch] text-bone/70">{line}</p>
            {action ? <div className="mt-2.5">{action}</div> : null}
        </div>
    );
}

/** The room's 16:9: the illustration of the setup, the live picture over it while there is one, and what is said over them. */
function Frame({ view, camera, large, onLarger, onHide, onShow }: { view: View; camera?: string; large?: boolean; onLarger?: () => void; onHide: () => void; onShow: () => void }) {
    const resting = view.kind !== 'live' && view.kind !== 'starting';
    const corner = large ? 'top-4' : 'top-3';
    return (
        <div className="@container/frame relative h-full w-full overflow-hidden rounded-2xl bg-white/[0.035] ring-1 ring-white/10">
            <div className={cn('absolute inset-0 transition-[opacity,filter] duration-500 motion-reduce:transition-none', resting ? 'opacity-25 blur-[2px]' : 'opacity-100')}>
                <Scene scene="laptopBehindYou" className="h-full w-full" />
            </div>
            {view.kind === 'live' ? (
                <>
                    <Picture camera={camera} />
                    <div className={cn('absolute', corner, large ? 'left-4' : 'left-3')}>
                        <Pill large={large} icon={dot('bg-mint animate-pulse-soft motion-reduce:animate-none')}>
                            <span>
                                Live<span className="@max-xs/frame:hidden"> · on this computer only</span>
                            </span>
                        </Pill>
                    </div>
                    <div className={cn('absolute flex gap-2', corner, large ? 'right-4' : 'right-3')}>
                        {onLarger ? (
                            <IconButton label="Show larger" size="sm" onClick={onLarger}>
                                <Maximize2 className="lucide h-4 w-4" strokeWidth={2.2} />
                            </IconButton>
                        ) : null}
                        <IconButton label="Hide the picture" size="sm" onClick={onHide}>
                            <EyeOff className="lucide h-4 w-4" strokeWidth={2.2} />
                        </IconButton>
                    </div>
                </>
            ) : null}
            {view.kind === 'starting' ? (
                <div className={cn('absolute', corner, large ? 'left-4' : 'left-3')}>
                    <Pill large={large} icon={<Loader2 className="lucide h-3.5 w-3.5 animate-spin text-rose" strokeWidth={2.5} />}>
                        Turning the camera on
                    </Pill>
                </div>
            ) : null}
            {view.kind === 'unused' ? <Note line="Move the pointer to see the camera's picture here." /> : null}
            {view.kind === 'paused' ? <Note title="Paused" line="The window has not been used for a while. Move the pointer to see the picture again." /> : null}
            {view.kind === 'hidden' ? (
                <Note
                    title="Picture hidden"
                    line="The camera stays off until a session reads it."
                    action={
                        <Button variant="ghost" size="sm" icon={<Eye className="lucide h-4 w-4" strokeWidth={2.4} />} onClick={onShow}>
                            Show the picture
                        </Button>
                    }
                />
            ) : null}
            {view.kind === 'trouble' ? <Note title="No picture" titleTone="text-amber" line={view.text} /> : null}
            {view.kind === 'quiet' ? <Note line={view.text} /> : null}
        </div>
    );
}

// The frame takes the column's width, or as much as its height leaves
// room for at 16:9.
const FIT: CSSProperties = { width: 'min(100cqw, 100cqh * 16 / 9)', aspectRatio: '16 / 9' };
const SIZE_CONTAINER: CSSProperties = { containerType: 'size' };
// The larger frame: as wide as the window lets it be, short of the dialog's
// header and the caption and a margin that keeps it clear of macOS's title
// strip (52 pt), whatever the window's height.
const LARGER: CSSProperties = { width: 'min(880px, (100vh - 240px) * 16 / 9, 100vw - 48px)' };

export function CameraPreview() {
    const state = useAppState();
    const dispatch = useDispatch();
    const presence = usePresence(state.previewIdleMs);
    const { view, asks } = viewOf(state, presence);
    usePreviewLease(asks);
    const [larger, setLarger] = useState(false);
    const enlargeable = view.kind === 'live' || view.kind === 'starting' || view.kind === 'paused' || view.kind === 'trouble';
    useEffect(() => {
        if (larger && !enlargeable) setLarger(false);
    }, [larger, enlargeable]);
    const hide = () => dispatch({ type: 'ui/preview-hidden', hidden: true });
    const show = () => dispatch({ type: 'ui/preview-hidden', hidden: false });
    const camera = state.preview.camera ?? state.source?.camera;
    return (
        <div className="flex min-h-0 w-full flex-1 flex-col items-center justify-center" style={SIZE_CONTAINER}>
            <div className="shrink-0" style={FIT}>
                <Frame view={view} camera={camera} onLarger={() => setLarger(true)} onHide={hide} onShow={show} />
            </div>
            <span className="sr-only" aria-live="polite">
                {spoken(view)}
            </span>
            <Dialog open={larger && enlargeable} onOpenChange={setLarger}>
                <DialogContent className="max-w-none gap-4 p-5 sm:max-w-none" style={LARGER}>
                    <DialogHeader className="pr-12">
                        <DialogTitle>Behind you</DialogTitle>
                        <DialogDescription>{camera ? `${camera}, as your session will see it.` : 'As your session will see it.'} The picture stays on this computer.</DialogDescription>
                    </DialogHeader>
                    <div className="aspect-video w-full">
                        <Frame
                            view={view}
                            camera={camera}
                            large
                            onHide={() => {
                                setLarger(false);
                                hide();
                            }}
                            onShow={show}
                        />
                    </div>
                    <p className="type-body text-center text-bone/75">{view.kind === 'trouble' ? view.text : POSE}</p>
                </DialogContent>
            </Dialog>
        </div>
    );
}
