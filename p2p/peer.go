package p2p

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/logger"
)

// peerAddr is the structure of a peer list element.
// It is also a valid net.Addr.
type peerAddr struct {
	IP     net.IP
	Port   uint64
	Pubkey []byte // optional
}

func newPeerAddr(addr net.Addr, pubkey []byte) *peerAddr {
	n := addr.Network()
	if n != "tcp" && n != "tcp4" && n != "tcp6" {
		// for testing with non-TCP
		return &peerAddr{net.ParseIP("127.0.0.1"), 30303, pubkey}
	}
	ta := addr.(*net.TCPAddr)
	return &peerAddr{ta.IP, uint64(ta.Port), pubkey}
}

func (d peerAddr) Network() string {
	if d.IP.To4() != nil {
		return "tcp4"
	} else {
		return "tcp6"
	}
}

func (d peerAddr) String() string {
	return fmt.Sprintf("%v:%d", d.IP, d.Port)
}

func (d *peerAddr) RlpData() interface{} {
	return []interface{}{string(d.IP), d.Port, d.Pubkey}
}

// Peer represents a remote peer.
type Peer struct {
	// Peers have all the log methods.
	// Use them to display messages related to the peer.
	*logger.Logger

	infolock   sync.Mutex
	identity   ClientIdentity
	caps       []Cap
	listenAddr *peerAddr // what remote peer is listening on
	dialAddr   *peerAddr // non-nil if dialing

	conn net.Conn
	crw  MsgChanReadWriter

	// These fields maintain the running protocols.
	protocols       []Protocol
	runBaseProtocol bool       // for testing
	CryptoType      CryptoType //
	cryptoReady     chan struct{}

	runlock sync.RWMutex // protects running
	running map[string]*proto

	protoWG  sync.WaitGroup
	protoErr chan error
	closed   chan struct{}
	disc     chan DiscReason

	activity event.TypeMux // for activity events

	slot int // index into Server peer list

	// These fields are kept so base protocol can access them.
	// TODO: this should be one or more interfaces
	ourID         ClientIdentity        // client id of the Server
	ourListenAddr *peerAddr             // listen addr of Server, nil if not listening
	newPeerAddr   chan<- *peerAddr      // tell server about received peers
	otherPeers    func() []*Peer        // should return the list of all peers
	pubkeyHook    func(*peerAddr) error // called at end of handshake to validate pubkey
}

// NewPeer returns a peer for testing purposes.
func NewPeer(id ClientIdentity, caps []Cap) *Peer {
	conn, _ := net.Pipe()
	peer := newPeer(conn, nil, nil)
	peer.setHandshakeInfo(id, nil, caps)
	close(peer.closed)
	return peer
}

func newServerPeer(server *Server, conn net.Conn, dialAddr *peerAddr) *Peer {
	p := newPeer(conn, server.Protocols, dialAddr)
	p.ourID = server.Identity
	p.newPeerAddr = server.peerConnect
	p.otherPeers = server.Peers
	p.pubkeyHook = server.verifyPeer
	p.runBaseProtocol = true
	if server.Encryption {
		p.CryptoType = EthCrypto
	}

	// laddr can be updated concurrently by NAT traversal.
	// newServerPeer must be called with the server lock held.
	if server.laddr != nil {
		p.ourListenAddr = newPeerAddr(server.laddr, server.Identity.PublicKey())
	}
	return p
}

func newPeer(conn net.Conn, protocols []Protocol, dialAddr *peerAddr) *Peer {
	p := &Peer{
		Logger:      logger.NewLogger("P2P " + conn.RemoteAddr().String()),
		conn:        conn,
		dialAddr:    dialAddr,
		protocols:   protocols,
		running:     make(map[string]*proto),
		disc:        make(chan DiscReason),
		protoErr:    make(chan error),
		closed:      make(chan struct{}),
		cryptoReady: make(chan struct{}),
	}
	return p
}

// Identity returns the client identity of the remote peer. The
// identity can be nil if the peer has not yet completed the
// handshake.
func (p *Peer) Identity() ClientIdentity {
	p.infolock.Lock()
	defer p.infolock.Unlock()
	return p.identity
}

func (self *Peer) PublicKey() (pubkey []byte) {
	self.infolock.Lock()
	defer self.infolock.Unlock()
	switch {
	case self.identity != nil:
		pubkey = self.identity.PublicKey()[1:]
	case self.dialAddr != nil:
		pubkey = self.dialAddr.Pubkey
	case self.listenAddr != nil:
		pubkey = self.listenAddr.Pubkey
	}
	return
}

// Caps returns the capabilities (supported subprotocols) of the remote peer.
func (p *Peer) Caps() []Cap {
	p.infolock.Lock()
	defer p.infolock.Unlock()
	return p.caps
}

func (p *Peer) setHandshakeInfo(id ClientIdentity, laddr *peerAddr, caps []Cap) {
	p.infolock.Lock()
	p.identity = id
	p.listenAddr = laddr
	p.caps = caps
	p.infolock.Unlock()
}

// RemoteAddr returns the remote address of the network connection.
func (p *Peer) RemoteAddr() net.Addr {
	return p.conn.RemoteAddr()
}

// LocalAddr returns the local address of the network connection.
func (p *Peer) LocalAddr() net.Addr {
	return p.conn.LocalAddr()
}

// Disconnect terminates the peer connection with the given reason.
// It returns immediately and does not wait until the connection is closed.
func (p *Peer) Disconnect(reason DiscReason) {
	select {
	case p.disc <- reason:
	case <-p.closed:
	}
}

// String implements fmt.Stringer.
func (p *Peer) String() string {
	kind := "inbound"
	p.infolock.Lock()
	if p.dialAddr != nil {
		kind = "outbound"
	}
	p.infolock.Unlock()
	return fmt.Sprintf("Peer(%p %v %s)", p, p.conn.RemoteAddr(), kind)
}

var (
	inactivityTimeout     = 2 * time.Second
	disconnectGracePeriod = 2 * time.Second
)

func (p *Peer) loop() (reason DiscReason, err error) {
	defer p.activity.Stop()
	defer p.closeProtocols()
	defer close(p.closed)
	defer p.conn.Close()

	if err = p.handleCryptoHandshake(); err != nil {
		// from here on everything can be encrypted, authenticated
		return DiscProtocolError, err // no graceful disconnect
	}
	close(p.cryptoReady)
	defer p.crw.Close()

	// read loop
	protoDone := make(chan struct{}, 1)

	in := p.crw.ReadC()
	errc := p.crw.ErrorC()
	unblock := p.crw.ReadNextC()

	unblock <- true

	if p.runBaseProtocol {
		p.startBaseProtocol()
	}

loop:
	for {
		select {
		case msg := <-in:
			// a new message has arrived.
			var wait bool
			if wait, err = p.dispatch(msg, protoDone); err != nil {
				p.Errorf("msg dispatch error: %v\n", err)
				reason = discReasonForError(err)
				break loop
			}
			if !wait {
				// Msg has already been read completely, continue with next message.
				unblock <- true
			}
			p.activity.Post(time.Now())
		case <-protoDone:
			// protocol has consumed the message payload,
			// we can continue reading from the socket.
			unblock <- true

		case err := <-errc:
			// read or write failed. there is no need to run the
			// polite disconnect sequence because the connection
			// is probably dead anyway.
			// TODO: handle write errors as well
			return DiscNetworkError, err
		case err = <-p.protoErr:
			reason = discReasonForError(err)
			break loop
		case reason = <-p.disc:
			break loop
		}
	}
	// tell the remote end to disconnect
	p.writeProtoMsg("", NewMsg(discMsg, reason))
	// io.Copy(ioutil.Discard, p.conn)//??
	<-time.After(disconnectGracePeriod)
	return reason, err
}

func (p *Peer) dispatch(msg Msg, protoDone chan struct{}) (wait bool, err error) {
	proto, err := p.getProto(msg.Code)
	if err != nil {
		return false, err
	}
	if msg.Size <= wholePayloadSize {
		// optimization: msg is small enough, read all
		// of it and move on to the next message
		buf, err := ioutil.ReadAll(msg.Payload)
		if err != nil {
			return false, err
		}
		msg.Payload = bytes.NewReader(buf)
		proto.in <- msg
	} else {
		wait = true
		pr := &eofSignal{msg.Payload, int64(msg.Size), protoDone}
		msg.Payload = pr
		proto.in <- msg
	}
	return wait, nil
}

type CryptoType byte

const (
	NoCrypto CryptoType = iota
	EthCrypto
)

var cryptoType = map[CryptoType]string{
	NoCrypto:  "no encryption",
	EthCrypto: "AES256 CTR HMAC SHA256",
}

func (self CryptoType) String() (s string) {
	s = cryptoType[self]
	if len(s) == 0 {
		s = string([]byte{byte(self)})
	}
	return
}

func (p *Peer) handleCryptoHandshake() (err error) {
	var crw MsgReadWriter
	switch p.CryptoType {
	case NoCrypto:
		if crw, err = NewMsgRW(bufio.NewReader(p.conn), p.conn); err != nil {
			return
		}
		p.Infof("insecure connection using no encryption/authentication")

	case EthCrypto:
		// cryptoId is just created for the lifecycle of the handshake
		// it is survived by an encrypted readwriter
		var initiator bool
		var sessionToken []byte
		sessionToken = make([]byte, keyLen)
		if _, err = rand.Read(sessionToken); err != nil {
			return
		}
		if p.dialAddr != nil { // this should have its own method Outgoing() bool
			initiator = true
		}
		// create crypto layer
		// this could in principle run only once but maybe we want to allow
		// identity switching
		var crypto *cryptoId
		if crypto, err = newCryptoId(p.ourID); err != nil {
			return
		}
		// run on peer
		// this bit handles the handshake and creates a secure communications channel with
		if sessionToken, crw, err = crypto.NewSession(bufio.NewReader(p.conn), p.conn, p.PublicKey(), sessionToken, initiator); err != nil {
			p.Errorf("unable to setup secure session: %v", err)
			return
		}
	default:
		err = fmt.Errorf("unrecognised crypto type %v", p.CryptoType)
		p.Errorf("%v", err)
	}
	p.crw = NewMessenger(crw)
	p.Infof("secure connection using %v", p.CryptoType)
	return
}

func (p *Peer) startBaseProtocol() {
	p.runlock.Lock()
	defer p.runlock.Unlock()
	p.running[""] = p.startProto(0, Protocol{
		Length: baseProtocolLength,
		Run:    runBaseProtocol,
	})
}

// startProtocols starts matching named subprotocols.
func (p *Peer) startSubprotocols(caps []Cap) {
	sort.Sort(capsByName(caps))

	p.runlock.Lock()
	defer p.runlock.Unlock()
	offset := baseProtocolLength
outer:
	for _, cap := range caps {
		for _, proto := range p.protocols {
			if proto.Name == cap.Name &&
				proto.Version == cap.Version &&
				p.running[cap.Name] == nil {
				p.running[cap.Name] = p.startProto(offset, proto)
				offset += proto.Length
				continue outer
			}
		}
	}
}

func (p *Peer) startProto(offset uint64, impl Protocol) *proto {
	rw := &proto{
		in:      make(chan Msg),
		out:     p.crw.WriteC(),
		offset:  offset,
		maxcode: impl.Length,
	}
	p.protoWG.Add(1)
	go func() {
		err := impl.Run(p, rw)
		if err == nil {
			p.Infof("protocol %q returned", impl.Name)
			err = newPeerError(errMisc, "protocol returned")
		} else {
			p.Errorf("protocol %q error: %v\n", impl.Name, err)
		}
		select {
		case p.protoErr <- err:
		case <-p.closed:
		}
		p.protoWG.Done()
	}()
	return rw
}

// getProto finds the protocol responsible for handling
// the given message code.
func (p *Peer) getProto(code uint64) (*proto, error) {
	p.runlock.RLock()
	defer p.runlock.RUnlock()
	for _, proto := range p.running {
		if code >= proto.offset && code < proto.offset+proto.maxcode {
			return proto, nil
		}
	}
	return nil, newPeerError(errInvalidMsgCode, "%d", code)
}

func (p *Peer) closeProtocols() {
	p.runlock.RLock()
	for _, p := range p.running {
		close(p.in)
	}
	p.runlock.RUnlock()
	p.protoWG.Wait()
}

// writeProtoMsg sends the given message on behalf of the given named protocol.
func (p *Peer) writeProtoMsg(protoName string, msg Msg) error {
	p.runlock.RLock()
	proto, ok := p.running[protoName]
	p.runlock.RUnlock()
	if !ok {
		return fmt.Errorf("protocol %s not handled by peer", protoName)
	}
	return proto.WriteMsg(msg)
}
