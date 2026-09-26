// The brand's illustrated scenes (the masseuse.ai web app's Lottie
// animations, 480x240: the laptop at the foot of the bed, the unit on the
// desk), lazily loaded, with a soft well as the fallback.

import { LottieView, type LottieLoader } from './LottieView';

export const scenes = {
    laptopBehindYou: {
        animation: (() => import('../brand/animations/laptop-behind-you.json')) as LottieLoader,
        label: "A person lying face down on a bed, seen from the side, with a laptop on a stool at the foot of the bed, its lid open toward them; the webcam's view fans over their shoulders, back, legs and feet, and their phone stands at the head end",
    },
    cameraBehindYou: {
        animation: (() => import('../brand/animations/camera-behind-you.json')) as LottieLoader,
        label: 'A camera on a stand behind a person lying on a bed, its view fanning over their whole length',
    },
    unitOnTheDesk: {
        animation: (() => import('../brand/animations/unit-on-the-desk.json')) as LottieLoader,
        label: 'A stimulation unit on a desk beside a laptop, its two pads laid out, a Bluetooth pulse between them',
    },
    connectYourUnit: {
        animation: (() => import('../brand/animations/connect-your-unit.json')) as LottieLoader,
        label: 'A stimulation unit being switched on, a pulse ring rising from it as it joins the laptop',
    },
} as const;

interface Props {
    scene: keyof typeof scenes;
    className?: string;
    fit?: 'contain' | 'cover';
}

export function Scene({ scene, className = 'aspect-[2/1] w-full', fit = 'contain' }: Props) {
    const s = scenes[scene];
    return (
        <LottieView animation={s.animation} label={s.label} className={`${className} rounded-2xl bg-gradient-to-b from-white/6 to-transparent`} fit={fit}>
            <div className="h-full w-full" />
        </LottieView>
    );
}
