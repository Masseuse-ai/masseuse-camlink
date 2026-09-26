// A Lottie animation played by lottie-web's light build (SVG renderer, no
// expressions, about 40 KB gzipped), which is fetched together with the
// animation's JSON the first time a LottieView mounts and never before: the
// welcome and session screens do not pay for it. Plays the `intro` marker
// once and then loops the `loop` marker; under prefers-reduced-motion it
// shows the first frame of the loop as a still (the finished picture). The
// children are the fallback, shown until the animation has drawn.
//
// The JSON is cloned before lottie-web sees it: the player writes into the
// data it is given, and the same animation may be mounted more than once
// over a session (the setup sheet, each time it opens).
//
// The fetch is set off from a layout effect, which runs as the view is
// committed: a screen change is a view transition (app/App.tsx), and React
// runs plain effects only once its animation has finished, which would put
// the scene's arrival a third of a second later on every screen that opens
// with one. Nothing here reads layout; the effect only starts the loads.

import type { AnimationItem, LottiePlayer } from 'lottie-web';
import { useLayoutEffect, useRef, useState, type ReactNode } from 'react';

/** What LottieView reads off an animation; the rest is the player's. */
export interface LottieData {
    fr: number;
    ip: number;
    op: number;
    w: number;
    h: number;
    markers?: { cm: string; tm: number; dr: number }[];
}

/** A loader for the animation module, so the JSON is a lazy chunk of its own. */
export type LottieLoader = () => Promise<{ default: LottieData }>;

interface Props {
    animation: LottieLoader;
    /** Marker played once on entry; absent or not in the file, the loop plays from the start. */
    intro?: string;
    /** Marker looped afterwards; absent or not in the file, the whole timeline loops. */
    loop?: string;
    /** What the picture is, for screen readers. */
    label: string;
    /** Sizes the box; the SVG fills it, keeping the animation's aspect ratio. */
    className?: string;
    /**
     * How the animation meets a box of another shape: `contain` letterboxes
     * (the default); `cover` fills the box and crops the sides that overflow,
     * centred, for a scene with empty sky over a box shorter than the scene.
     */
    fit?: 'contain' | 'cover';
    /** Shown until the animation has drawn (and, on a failed load, for good). */
    children?: ReactNode;
}

type Segment = [number, number];

let playerPromise: Promise<LottiePlayer> | null = null;

/** The player, loaded once per page (LoadingLink shares it). A failed load is
 * not remembered, so the next caller tries again. */
export function loadPlayer(): Promise<LottiePlayer> {
    if (!playerPromise) {
        const attempt = import('lottie-web/build/player/lottie_light').then((m) => m.default);
        attempt.catch(() => {
            if (playerPromise === attempt) playerPromise = null;
        });
        playerPromise = attempt;
    }
    return playerPromise;
}

/** A named marker as a playSegments segment, or null when absent or empty. */
export function marker(data: LottieData, name: string | undefined): Segment | null {
    if (!name) return null;
    const found = data.markers?.find((m) => m.cm === name);
    return found && found.dr > 0 ? [found.tm, found.tm + found.dr] : null;
}

export function reducedMotion(): boolean {
    return typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;
}

export function LottieView({ animation, intro = 'intro', loop = 'loop', label, className = 'aspect-[2/1] w-full', fit = 'contain', children }: Props) {
    const box = useRef<HTMLDivElement>(null);
    const [ready, setReady] = useState(false);

    useLayoutEffect(() => {
        const container = box.current;
        if (!container) return;
        let cancelled = false;
        let item: AnimationItem | null = null;

        Promise.all([loadPlayer(), animation()])
            .then(([player, module]) => {
                if (cancelled) return;
                const data = module.default;
                const still = reducedMotion();
                const introSegment = marker(data, intro);
                const loopSegment = marker(data, loop) ?? [data.ip, data.op] satisfies Segment;
                item = player.loadAnimation({
                    container,
                    renderer: 'svg',
                    loop: true,
                    autoplay: false,
                    animationData: structuredClone(data),
                    rendererSettings: { progressiveLoad: true, preserveAspectRatio: fit === 'cover' ? 'xMidYMid slice' : 'xMidYMid meet' },
                });
                item.addEventListener('DOMLoaded', () => {
                    if (!cancelled) setReady(true);
                });
                if (still) {
                    item.goToAndStop(loopSegment[0], true);
                } else {
                    // Queue the intro then the loop; with `loop: true` the player
                    // repeats the last segment it was given once the queue is empty.
                    item.playSegments(introSegment ? [introSegment, loopSegment] : [loopSegment], true);
                }
            })
            .catch(() => {
                // Chunk failed to load (offline, deploy in progress): the fallback stays.
            });

        return () => {
            cancelled = true;
            item?.destroy();
            item = null;
        };
    }, [animation, intro, loop, fit]);

    return (
        <div className={`relative overflow-hidden ${className}`} role="img" aria-label={label}>
            <div ref={box} aria-hidden className="absolute inset-0" />
            {!ready && children ? (
                <div aria-hidden className="absolute inset-0 flex items-center justify-center">
                    {children}
                </div>
            ) : null}
        </div>
    );
}
