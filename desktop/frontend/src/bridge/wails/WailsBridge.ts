// The connector behind the desktop shell (desktop/connector.go). The shell
// runs masseuse-camlink -ipc as a child, relays each JSON line it writes as
// the Wails event "connector", and takes the page's requests as typed
// methods (the generated bindings). A page that mounts after the connector
// has spoken reads the state from the shell's snapshot first: the last
// event of each kind, in the order they are meant to be read.

import { Events } from '@wailsio/runtime';

import { ConnectorService } from '../../../bindings/github.com/FemLed/masseuse-camlink/desktop';
import type { Bridge, ConnectorCommand, ConnectorEvent } from '../types';

export class WailsBridge implements Bridge {
    subscribe(handler: (event: ConnectorEvent) => void): () => void {
        let live = true;
        // Live events are queued until the snapshot has been read, so the
        // page never sees a newer event before the state it builds on.
        let queue: ConnectorEvent[] | null = [];
        const off = Events.On('connector', (ev) => {
            if (!live) return;
            const event = ev.data as ConnectorEvent;
            if (queue) queue.push(event);
            else handler(event);
        });
        ConnectorService.Snapshot()
            .then((events) => {
                if (!live) return;
                for (const event of events ?? []) handler(event as unknown as ConnectorEvent);
            })
            .catch(() => {})
            .finally(() => {
                if (!live) return;
                const pending = queue ?? [];
                queue = null;
                for (const event of pending) handler(event);
            });
        return () => {
            live = false;
            off();
        };
    }

    async send(command: ConnectorCommand): Promise<void> {
        switch (command.type) {
            case 'list_devices':
                return ConnectorService.ListDevices();
            case 'set_source':
                return ConnectorService.SetSource(command.choice);
            case 'set_camera':
                return ConnectorService.SetCamera(command.view, command.enabled);
            case 'select_unit':
                return ConnectorService.SelectUnit(command.id);
            case 'update_now':
                return ConnectorService.UpdateNow();
            case 'request_media_access':
                // The shell's own, not the connector's: the system's prompts
                // for the camera and the microphone; the answer is a media event.
                return ConnectorService.RequestMediaAccess();
            case 'quit':
                return ConnectorService.Quit();
        }
    }
}

/** Starts the connector again after it stopped (the Blocked screen's "Start again"). */
export function restartConnector(): Promise<void> {
    return ConnectorService.Restart();
}
