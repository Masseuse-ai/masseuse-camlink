// The system's standing on this computer's camera and microphone, on
// macOS, where an application is asked once, in its own name, and refused
// silently ever after. The shell reads and asks (desktop/permissions.go)
// and says the standing as a `media` event; the page asks on the person's
// behalf the first time a screen that concerns the camera opens (Cameras
// and microphone in the first run; Ready, on a computer paired before,
// which skips the steps), so the system's question comes while they are at
// the computer choosing devices rather than at the first session, when the
// screen may be dark and no one is looking at the Mac. The card says what
// is being asked, offers to ask again while the answer is not determined,
// and, once refused, points at Privacy & Security and opens it. Windows and
// Linux have no such question; nothing is shown there.

import { Camera, CameraOff } from 'lucide-react';
import { useEffect } from 'react';

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { useAppState, useBridge, useDispatch, type AppState } from '../bridge/store';
import { openPrivacySettings } from '../shell/shell';

/** What the standing calls for: an ask (not determined yet), the settings (refused or restricted), or nothing. macOS only. */
export type MediaStanding = 'ask' | 'blocked' | null;

export function mediaStanding(state: AppState): MediaStanding {
    if (state.platform !== 'darwin' || !state.media) return null;
    const { camera, mic } = state.media;
    // An ask first: it settles what can be settled, and what is refused
    // shows as such once it is answered.
    if (camera === 'notDetermined' || mic === 'notDetermined') return 'ask';
    if (camera !== 'authorized' || mic !== 'authorized') return 'blocked';
    return null;
}

/**
 * Asks the system once, the first time a screen that calls this mounts
 * while the standing is not determined; the card's button asks again. The
 * shell runs one ask at a time, so a second call while the prompts are up
 * is nothing more than that.
 */
export function useMediaAsk(): void {
    const state = useAppState();
    const dispatch = useDispatch();
    const bridge = useBridge();
    const standing = mediaStanding(state);
    const asked = state.mediaAsked;
    useEffect(() => {
        if (standing !== 'ask' || asked) return;
        dispatch({ type: 'ui/media-asked' });
        void bridge.send({ type: 'request_media_access' });
    }, [standing, asked, dispatch, bridge]);
}

/** The card, when there is one to show: the ask under way, or what macOS is blocking and where that is changed. */
export function MediaAccessAlert({ className }: { className?: string }) {
    const state = useAppState();
    const bridge = useBridge();
    const standing = mediaStanding(state);
    if (!standing || !state.media) return null;
    const { camera, mic } = state.media;

    // Two lines of description at Ready's column width (under 145
    // characters), and the button close under them: the card shares the
    // Session card with the meters and must not push them out (Home.tsx).
    if (standing === 'ask') {
        return (
            <Alert variant="neutral" className={className}>
                <Camera />
                <AlertTitle>Allow the camera and microphone</AlertTitle>
                <AlertDescription>macOS asks whether Masseuse.ai may use this computer's camera and microphone; allow both. If no question is showing, ask again.</AlertDescription>
                <div className="mt-1.5 group-has-[>svg]/alert:col-start-2">
                    <Button variant="ghost" size="sm" onClick={() => void bridge.send({ type: 'request_media_access' })}>
                        Allow camera and microphone
                    </Button>
                </div>
            </Alert>
        );
    }

    const blockedCamera = camera !== 'authorized';
    const blockedMic = mic !== 'authorized';
    const what = blockedCamera && blockedMic ? 'the camera and microphone' : blockedCamera ? 'the camera' : 'the microphone';
    const restricted = camera === 'restricted' || mic === 'restricted';
    return (
        <Alert variant="warn" className={className}>
            <CameraOff />
            <AlertTitle>macOS is blocking {what}</AlertTitle>
            <AlertDescription>
                {restricted
                    ? 'A profile or parental controls on this computer forbid it; Privacy & Security in System Settings shows what applies.'
                    : `Allow it under System Settings › Privacy & Security › Camera, and Microphone; a session runs without ${what} until then.`}
            </AlertDescription>
            <div className="mt-1.5 group-has-[>svg]/alert:col-start-2">
                <Button variant="ghost" size="sm" onClick={() => void openPrivacySettings()}>
                    Open Privacy &amp; Security
                </Button>
            </div>
        </Alert>
    );
}
