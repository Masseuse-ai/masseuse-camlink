package capture

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// intake is where ffmpeg publishes: a plain RTSP server on a loopback port
// chosen by the system, accepting one RECORD on a path that is a fresh
// random secret, so no other program on the computer can publish into the
// stream or read it. Everything it receives goes into the connector's
// stream (internal/serve) untouched.
type intake struct {
	srv  *gortsplib.Server
	sink *serve.Server
	path string
	url  string
	log  *slog.Logger

	mu   sync.Mutex
	pub  *serve.Publication
	sess *gortsplib.ServerSession
	// onChange is called with true when a publication starts and false when
	// it ends.
	onChange func(bool)
	dropped  atomic.Uint64
}

func newIntake(sink *serve.Server, logger *slog.Logger, onChange func(bool)) (*intake, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	in := &intake{sink: sink, path: "/" + base64.RawURLEncoding.EncodeToString(raw), log: logger, onChange: onChange}
	in.srv = &gortsplib.Server{Handler: in, RTSPAddress: "127.0.0.1:0"}
	if err := in.srv.Start(); err != nil {
		return nil, fmt.Errorf("capture: intake: %w", err)
	}
	port := in.srv.NetListener().Addr().(*net.TCPAddr).Port
	in.url = fmt.Sprintf("rtsp://127.0.0.1:%d%s", port, in.path)
	return in, nil
}

// URL is what ffmpeg publishes to.
func (in *intake) URL() string { return in.url }

// Close stops the server and ends any publication.
func (in *intake) Close() {
	in.srv.Close()
	in.mu.Lock()
	pub := in.pub
	in.pub, in.sess = nil, nil
	in.mu.Unlock()
	if pub != nil {
		pub.Close()
	}
}

// Publishing reports whether ffmpeg is connected and announced.
func (in *intake) Publishing() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.pub != nil
}

// OnAnnounce takes the publisher's description into the stream. While the
// stream has a handover armed (a restart at another bit rate), the new
// ffmpeg joins the publication its predecessor fed, so readers notice
// nothing; otherwise a second publisher is refused.
func (in *intake) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	if ctx.Path != in.path {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	pub, err := in.sink.Publish(ctx.Description)
	if err != nil {
		in.log.Warn("capture: stream refused the publication", "err", err)
		return &base.Response{StatusCode: base.StatusServiceUnavailable}, nil
	}
	in.pub, in.sess = pub, ctx.Session
	if in.onChange != nil {
		in.onChange(true)
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// armHandover tells the current publication its publisher is about to be
// replaced (serve.Publication.ArmHandover); nothing is publishing does
// nothing.
func (in *intake) armHandover(grace time.Duration) bool {
	in.mu.Lock()
	pub := in.pub
	in.mu.Unlock()
	if pub == nil {
		return false
	}
	pub.ArmHandover(grace)
	return true
}

// OnSetup admits the publisher's tracks; nobody plays from the intake.
func (in *intake) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if ctx.Session.State() != gortsplib.ServerSessionStatePreRecord {
		return &base.Response{StatusCode: base.StatusMethodNotAllowed}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, nil, nil
}

// OnRecord forwards every packet into the stream.
func (in *intake) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	in.mu.Lock()
	pub := in.pub
	in.mu.Unlock()
	if pub == nil {
		return &base.Response{StatusCode: base.StatusMethodNotValidInThisState}, nil
	}
	ctx.Session.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		if err := pub.WritePacketRTP(medi, pkt); err != nil {
			// Logged sparingly: a reader that cannot keep up drops many.
			if n := in.dropped.Add(1); n == 1 || n%1000 == 0 {
				in.log.Warn("capture: packet not forwarded", "err", err, "dropped", n)
			}
		}
	})
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// Dropped is how many packets could not be forwarded into the stream.
func (in *intake) Dropped() uint64 { return in.dropped.Load() }

// OnDescribe refuses readers: the stream is read through internal/serve.
func (in *intake) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusMethodNotAllowed}, nil, nil
}

// OnSessionClose releases the publication when ffmpeg goes: it ends, unless
// a handover is armed and the replacement is on its way. A close from a
// publisher that has already been replaced is ignored.
func (in *intake) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	in.mu.Lock()
	if in.sess != ctx.Session {
		in.mu.Unlock()
		return
	}
	pub := in.pub
	in.pub, in.sess = nil, nil
	in.mu.Unlock()
	if pub != nil {
		pub.Release()
	}
	if in.onChange != nil {
		in.onChange(false)
	}
}
