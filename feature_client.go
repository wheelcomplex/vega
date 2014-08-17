package vega

import (
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

func Dial(addr string) (*FeatureClient, error) {
	client, err := NewClient(addr)
	if err != nil {
		return nil, err
	}

	return &FeatureClient{
		Client: client,
	}, nil
}

// Create a new FeatureClient wrapping a explicit Client
func NewFeatureClient(c *Client) *FeatureClient {
	return &FeatureClient{Client: c}
}

type Handler interface {
	HandleMessage(*Message) *Message
}

type wrappedHandlerFunc struct {
	f func(*Message) *Message
}

func (w *wrappedHandlerFunc) HandleMessage(m *Message) *Message {
	return w.f(m)
}

func HandlerFunc(h func(*Message) *Message) Handler {
	return &wrappedHandlerFunc{h}
}

// type Handler func(*Message) *Message

// Wraps Client to provide highlevel behaviors that build on the basics
// of the distributed mailboxes. Should only be used by one goroutine
// at a time.
type FeatureClient struct {
	*Client

	localQueue string
	lock       sync.Mutex
}

// Create a new FeatureClient that wraps the same Client as
// this one. Useful for creating a new instance to use in a new
// goroutine
func (fc *FeatureClient) Clone() *FeatureClient {
	return &FeatureClient{Client: fc.Client}
}

// Return the name of a ephemeral queue only for this instance
func (fc *FeatureClient) LocalQueue() string {
	fc.lock.Lock()
	defer fc.lock.Unlock()

	if fc.localQueue != "" {
		return fc.localQueue
	}

	r := RandomQueue()

	err := fc.EphemeralDeclare(r)
	if err != nil {
		panic(err)
	}

	fc.localQueue = r

	return r
}

const cEphemeral = "#ephemeral"

func (fc *FeatureClient) Declare(name string) error {
	if strings.HasSuffix(name, cEphemeral) {
		return fc.Client.EphemeralDeclare(name)
	}

	return fc.Client.Declare(name)
}

func (fc *FeatureClient) HandleRequests(name string, h Handler) error {
	for {
		del, err := fc.LongPoll(name, 1*time.Minute)
		if err != nil {
			return err
		}

		if del == nil {
			continue
		}

		msg := del.Message

		ret := h.HandleMessage(msg)

		del.Ack()

		fc.Push(msg.ReplyTo, ret)
	}
}

func (fc *FeatureClient) Request(name string, msg *Message) (*Delivery, error) {
	msg.ReplyTo = fc.LocalQueue()

	err := fc.Push(name, msg)
	if err != nil {
		return nil, err
	}

	for {
		resp, err := fc.LongPoll(msg.ReplyTo, 1*time.Minute)
		if err != nil {
			return nil, err
		}

		if resp == nil {
			continue
		}

		return resp, nil
	}
}

type Receiver struct {
	// channel that messages are sent to
	Channel <-chan *Delivery

	// Any error detected while receiving
	Error error

	shutdown chan struct{}
}

func (rec *Receiver) Close() error {
	close(rec.shutdown)
	return nil
}

func (fc *FeatureClient) Receive(name string) *Receiver {
	c := make(chan *Delivery)

	rec := &Receiver{c, nil, make(chan struct{})}

	go func() {
		for {
			select {
			case <-rec.shutdown:
				close(c)
				return
			default:
				// We don't cancel this action if Receive is told to Close. Instead
				// we let it timeout and then detect the shutdown request and exit.
				msg, err := fc.Client.LongPoll(name, 1*time.Minute)
				if err != nil {
					close(c)
					return
				}

				if msg == nil {
					continue
				}

				c <- msg
			}
		}
	}()

	return rec
}

type pipeAddr struct {
	q string
}

func (p *pipeAddr) Network() string {
	return "vega"
}

func (p *pipeAddr) String() string {
	return "vega:" + p.q
}

type pipeConn struct {
	fc      *FeatureClient
	pairM   string
	ownM    string
	closed  bool
	abandon bool
	buffer  []byte
}

func (p *pipeConn) Close() error {
	if p.abandon {
		return nil
	}

	p.abandon = true

	msg := Message{
		Type: "pipe/close",
	}

	p.fc.Abandon(p.ownM)
	return p.fc.Push(p.pairM, &msg)
}

func (p *pipeConn) LocalAddr() net.Addr {
	return &pipeAddr{p.ownM}
}

func (p *pipeConn) RemoteAddr() net.Addr {
	return &pipeAddr{p.pairM}
}

func (p *pipeConn) Read(b []byte) (int, error) {
	if p.closed {
		return 0, io.EOF
	}

	if p.buffer != nil {
		n := len(p.buffer)
		bn := len(b)

		if bn < n {
			copy(b, p.buffer[:bn])
			p.buffer = p.buffer[bn:]
			return bn, nil
		}

		copy(b, p.buffer)
		p.buffer = nil

		if bn > n {
			resp, err := p.fc.Poll(p.ownM)
			if err != nil {
				return n, nil
			}

			err = resp.Ack()
			if err != nil {
				return n, nil
			}

			if resp.Message.Type == "pipe/close" {
				p.closed = true
				return n, nil
			}

			b = b[n:]

			bn2 := len(b)
			n2 := len(resp.Message.Body)

			if bn2 < n2 {
				copy(b, resp.Message.Body[:bn2])
				p.buffer = resp.Message.Body[bn2:]
				return n + bn2, nil
			}

			copy(b, resp.Message.Body)
			p.buffer = nil

			return n + n2, nil
		}

		return n, nil
	}

	for {
		resp, err := p.fc.LongPoll(p.ownM, 1*time.Minute)
		if err != nil {
			return 0, err
		}

		if resp == nil {
			continue
		}

		err = resp.Ack()
		if err != nil {
			return 0, err
		}

		if resp.Message.Type == "pipe/close" {
			p.closed = true
			return 0, io.EOF
		}

		bn := len(b)
		n := len(resp.Message.Body)

		if bn < n {
			copy(b, resp.Message.Body[:bn])
			p.buffer = resp.Message.Body[bn:]
			return bn, nil
		}

		copy(b, resp.Message.Body)
		p.buffer = nil

		return n, nil
	}
}

func (p *pipeConn) Write(b []byte) (int, error) {
	if p.closed {
		return 0, io.EOF
	}

	msg := Message{
		Body: b,
	}

	err := p.fc.Push(p.pairM, &msg)
	if err != nil {
		return 0, err
	}

	return len(b), nil
}

func (p *pipeConn) SetDeadline(t time.Time) error {
	return nil
}

func (p *pipeConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (p *pipeConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (fc *FeatureClient) ListenPipe(name string) (net.Conn, error) {
	q := "pipe:" + name
	err := fc.Declare(q)
	if err != nil {
		return nil, err
	}

	for {
		resp, err := fc.LongPoll(q, 1*time.Minute)
		if err != nil {
			return nil, err
		}

		if resp == nil {
			continue
		}

		err = resp.Ack()
		if err != nil {
			return nil, err
		}

		if resp.Message.Type != "pipe/initconnect" {
			return nil, EProtocolError
		}

		ownM := RandomQueue()
		fc.EphemeralDeclare(ownM)

		msg := Message{
			Type:    "pipe/setup",
			ReplyTo: ownM,
		}

		err = fc.Push(resp.Message.ReplyTo, &msg)
		if err != nil {
			fc.Abandon(ownM)
			return nil, err
		}

		return &pipeConn{
			fc:    fc,
			pairM: resp.Message.ReplyTo,
			ownM:  ownM}, nil
	}
}

func (fc *FeatureClient) ConnectPipe(name string) (net.Conn, error) {
	ownM := RandomQueue()
	fc.EphemeralDeclare(ownM)

	msg := Message{
		Type:    "pipe/initconnect",
		ReplyTo: ownM,
	}

	q := "pipe:" + name

	err := fc.Push(q, &msg)
	if err != nil {
		fc.Abandon(ownM)
		return nil, err
	}

	for {
		resp, err := fc.LongPoll(ownM, 1*time.Minute)
		if err != nil {
			return nil, err
		}

		if resp == nil {
			continue
		}

		err = resp.Ack()
		if err != nil {
			return nil, err
		}

		if resp.Message.Type != "pipe/setup" {
			fc.Abandon(ownM)
			return nil, EProtocolError
		}

		return &pipeConn{
			fc:    fc,
			pairM: resp.Message.ReplyTo,
			ownM:  ownM}, nil
	}
}
