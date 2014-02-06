package gotftp

import (
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

// ReadCloser is what the Handler needs to implement to serve TFTP read requests.
type ReadCloser interface {
	io.ReadCloser
}

// WriteCloser is what the Handler needs to implement to serve TFTP write requests.
type WriteCloser interface {
	io.WriteCloser
}

// Handler is the interface a consumer of this library needs to implement to be
// able to serve TFTP requests.
type Handler interface {
	ReadFile(peer net.Addr, filename string) (ReadCloser, error)
	WriteFile(peer net.Addr, filename string) (WriteCloser, error)
}

// ErrTimeout is returned by the packetReader when it times out reading a packet.
var ErrTimeout = errors.New("timeout")

// packetReader is the interface that describes the function used for reading
// packets. The read function returns an error when it times out (ErrTimeout)
// or cannot deserialize a packet. In the latter case, the error is propagates
// from the routines responsible for deserialization.
type packetReader interface {
	read(time.Duration) (x packet, err error)
}

// packetWriter is the interface that describes the function used for writing packets.
type packetWriter interface {
	write(x packet) error
}

// packetValidator is type of the function that gets called from the function
// that writes a packet and waits for an acknowledgement from its peer.
type packetValidator func(p packet) bool

// session records the state for an exchange of UDP packets concerning a single
// TFTP request.
type session struct {
	packetReader
	packetWriter

	h       Handler
	blksize int
	timeout int
}

func serve(r packetReader, w packetWriter, h Handler) {
	s := &session{
		packetReader: r,
		packetWriter: w,

		h:       h,
		blksize: 512,
		timeout: 3,
	}

	s.serve()
}

func (s *session) writeError(code uint16, message string) error {
	p := packetERROR{
		errorCode:    code,
		errorMessage: message,
	}

	return s.write(&p)
}

// writeAndWaitForPacket sends the packet p to our peer and waits for it to
// reply with a packet that can be validated by the packet validator v.
//
// If no valid reply if received before the configured timeout expires, packet
// p will be sent again. The packet will be sent for a maximum of 3 times.
//
// When a non-timeout error occurs when reading a reply, this function sends an
// error packet with the error message back to the peer.
func (s *session) writeAndWaitForPacket(p packet, v packetValidator) (packet, error) {
	var err error

	for i := 0; i < 3; i++ {
		err = s.write(p)
		if err != nil {
			return nil, err
		}

		now := time.Now()
		end := now.Add(time.Duration(s.timeout) * time.Second)
		for ; now.Before(end); now = time.Now() {
			timeout := end.Sub(now)

			p, err := s.read(timeout)
			if err == ErrTimeout {
				break
			}

			if err != nil {
				_ = s.writeError(0, err.Error())
				return nil, err
			}

			// Check validity of packet
			if v(p) {
				return p, nil
			}
		}
	}

	return nil, ErrTimeout
}

func (s *session) serve() {
	p, err := s.read(0)
	if err != nil {
		_ = s.writeError(0, err.Error())
		return
	}

	switch px := p.(type) {
	case *packetRRQ:
		s.serveRRQ(px)
	case *packetWRQ:
		s.serveWRQ(px)
	default:
		_ = s.writeError(4, "")
	}
}

func (s *session) negotiate(o map[string]string) (map[string]string, error) {
	oack := make(map[string]string)

	blksize, ok := o["blksize"]
	if ok {
		i, err := strconv.Atoi(blksize)
		if err != nil {
			return nil, err
		}

		// Lower and upper bound from RFC 2348.
		if i < 8 {
			s.blksize = 8
		} else if i > 65464 {
			s.blksize = 65464
		} else {
			s.blksize = i
		}

		oack["blksize"] = strconv.Itoa(s.blksize)
	}

	timeout, ok := o["timeout"]
	if ok {
		i, err := strconv.Atoi(timeout)
		if err != nil {
			return nil, err
		}

		// Lower and upper bound from RFC 2349.
		if i < 1 {
			s.timeout = 1
		} else if i > 255 {
			s.timeout = 255
		} else {
			s.timeout = i
		}

		oack["timeout"] = strconv.Itoa(s.timeout)
	}

	return oack, nil
}

func ackValidator(blockNr uint16) packetValidator {
	return func(p packet) bool {
		ack, ok := p.(*packetACK)
		return ok && ack.blockNr == blockNr
	}
}

func (s *session) serveRRQ(p *packetRRQ) {
	rc, err := s.h.ReadFile(&net.UDPAddr{}, p.filename)
	if err != nil {
		switch err {
		case os.ErrNotExist:
			_ = s.writeError(1, err.Error())
		case os.ErrPermission:
			_ = s.writeError(2, err.Error())
		default:
			_ = s.writeError(0, err.Error())
		}
		return
	}

	defer func() {
		// This is called from an anonymous function to make errcheck happy.
		_ = rc.Close()
	}()

	if len(p.options) > 0 {
		options, err := s.negotiate(p.options)
		if err != nil {
			_ = s.writeError(8, err.Error())
			return
		}

		p := &packetOACK{options: options}
		_, err = s.writeAndWaitForPacket(p, ackValidator(0))
		if err != nil {
			return
		}
	}

	// Proceed to send the file
	buf := make([]byte, s.blksize)
	for blockNr := uint16(1); ; blockNr++ {
		n, err := rc.Read(buf)

		// Only be concerned about error if no bytes were read.
		if n == 0 {
			if err == io.EOF {
				break
			} else {
				_ = s.writeError(0, err.Error())
				return
			}
		}

		p := &packetDATA{
			blockNr: blockNr,
			data:    buf[:n],
		}

		_, err = s.writeAndWaitForPacket(p, ackValidator(blockNr))
		if err != nil {
			return
		}
	}
}

func (s *session) serveWRQ(p *packetWRQ) {
	_ = s.writeError(0, "not supported")
}
