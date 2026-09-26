// The window: the top bar, the steps, the screen the state calls for, the
// notices over it, and the About dialog over it all. The native menu's
// requests arrive as "menu" events from the shell (src/shell), and the
// menu's status lines (the connector's version, the state of updates) are
// kept current from here.

import { useEffect } from 'react';

import { TooltipProvider } from '@/components/ui/tooltip';
import { screenOf, useAppState, useBridge, useDeviceListing, useDispatch } from './bridge/store';
import { About } from './screens/About';
import { Blocked } from './screens/Blocked';
import { Camera } from './screens/Camera';
import { Home } from './screens/Home';
import { Pair } from './screens/Pair';
import { Unit } from './screens/Unit';
import { onMenu } from './shell/shell';
import { Notices } from './ui/Notices';
import { StepBar } from './ui/StepBar';
import { TopBar } from './ui/TopBar';

function Screen() {
    const state = useAppState();
    switch (screenOf(state)) {
        case 'blocked':
            return <Blocked />;
        case 'pair':
            return <Pair />;
        case 'camera':
            return <Camera />;
        case 'unit':
            return <Unit />;
        case 'home':
            return <Home />;
    }
}

export function App() {
    const state = useAppState();
    const dispatch = useDispatch();
    const bridge = useBridge();
    const screen = screenOf(state);

    // The first device listing, once the connector is up, so the Cameras
    // screen has it when it opens; that screen keeps it current itself.
    useDeviceListing(false);

    useEffect(
        () =>
            onMenu((action) => {
                if (action === 'about') dispatch({ type: 'ui/about', open: true });
                if (action === 'check-updates') void bridge.send({ type: 'update_now' });
            }),
        [dispatch, bridge],
    );

    return (
        <TooltipProvider>
            <div className="relative flex h-full min-h-0 flex-col">
                <TopBar />
                {screen === 'blocked' ? null : <StepBar />}
                <main key={screen} className="flex min-h-0 flex-1 flex-col overflow-y-auto">
                    <Screen />
                </main>
                <Notices />
                <About />
            </div>
        </TooltipProvider>
    );
}
