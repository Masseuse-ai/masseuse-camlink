// The mock camera's picture for Ready's preview, drawn here and sent the
// way the connector's ffmpeg sends it (a JPEG in base64, its longer side at
// most 640), so the page cannot tell: a dim room in the camera's own shape
// with a bed across it, a slow sweep of light and the time, so a live
// picture reads as live; or black, as a virtual camera whose program is not
// running sends.

let canvas: HTMLCanvasElement | null = null;

const PREFIX = 'data:image/jpeg;base64,';

export function mockPicture(width: number, height: number, elapsedMs: number, black = false): string {
    const scale = Math.min(1, 640 / Math.max(width, height));
    const w = Math.max(2, Math.round((width * scale) / 2) * 2);
    const h = Math.max(2, Math.round((height * scale) / 2) * 2);
    canvas ??= document.createElement('canvas');
    canvas.width = w;
    canvas.height = h;
    const g = canvas.getContext('2d');
    if (!g) return '';
    if (black) {
        g.fillStyle = '#000';
        g.fillRect(0, 0, w, h);
        return canvas.toDataURL('image/jpeg', 0.7).slice(PREFIX.length);
    }
    // The wall over the floor.
    const wall = g.createLinearGradient(0, 0, 0, h);
    wall.addColorStop(0, '#2b2531');
    wall.addColorStop(0.6, '#1f1a24');
    wall.addColorStop(1, '#141117');
    g.fillStyle = wall;
    g.fillRect(0, 0, w, h);
    g.fillStyle = '#17131a';
    g.fillRect(0, h * 0.7, w, h * 0.3);
    // The bed across the picture: its frame, the mattress, a pillow at the far end.
    const bx = w * 0.07;
    const bw = w * 0.86;
    g.fillStyle = '#2f2934';
    g.beginPath();
    g.roundRect(bx, h * 0.5, bw, h * 0.26, Math.min(w, h) * 0.03);
    g.fill();
    const sheet = g.createLinearGradient(0, h * 0.44, 0, h * 0.62);
    sheet.addColorStop(0, '#5a5160');
    sheet.addColorStop(1, '#3d3643');
    g.fillStyle = sheet;
    g.beginPath();
    g.roundRect(bx + w * 0.01, h * 0.44, bw - w * 0.02, h * 0.16, Math.min(w, h) * 0.025);
    g.fill();
    g.fillStyle = '#6b6271';
    g.beginPath();
    g.roundRect(bx + bw * 0.78, h * 0.4, bw * 0.17, h * 0.07, Math.min(w, h) * 0.03);
    g.fill();
    // A soft light drifting across the room.
    const x = (Math.sin(elapsedMs / 2600) * 0.5 + 0.5) * w;
    const light = g.createRadialGradient(x, h * 0.25, 0, x, h * 0.25, Math.max(w, h) * 0.6);
    light.addColorStop(0, 'rgba(255, 214, 196, 0.16)');
    light.addColorStop(1, 'rgba(255, 214, 196, 0)');
    g.fillStyle = light;
    g.fillRect(0, 0, w, h);
    // The camera's clock, as webcams print it.
    g.font = `${Math.max(10, Math.round(h * 0.042))}px ui-monospace, Menlo, monospace`;
    g.fillStyle = 'rgba(244, 238, 242, 0.5)';
    g.fillText(new Date().toLocaleTimeString(), w * 0.035, h * 0.93);
    return canvas.toDataURL('image/jpeg', 0.72).slice(PREFIX.length);
}
