package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"

	json0 "encoding/json"

	"encoding/gob"

	"bytes"

	//"github.com/golang/glog"
	crypto "github.com/libp2p/go-libp2p-crypto"
	host "github.com/libp2p/go-libp2p-host"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	ps "github.com/libp2p/go-libp2p-peerstore"
	libp2pswarm "github.com/libp2p/go-libp2p-swarm"
	bhost "github.com/libp2p/go-libp2p/p2p/host/basic"
	ma "github.com/multiformats/go-multiaddr"
	multicodec "github.com/multiformats/go-multicodec"
	json "github.com/multiformats/go-multicodec/json"
)

const proto = "/example/1.0.0"

// MsgError is an error that happens while handling an incoming message
type MsgError struct {
	c    ChanID
	m    Msg
	info string
}

func (e MsgError) Error() string {
	return fmt.Sprintf("message error on channel %v: %v", e.c, e.info)
}

// ChanID identifies a channel
type ChanID uint32

// SwarmID identifies a swarm
type SwarmID uint32

// Opcode identifies the type of message
type Opcode uint8

// From the RFC:
//   +----------+------------------+
//   | Msg Type | Description      |
//   +----------+------------------+
//   | 0        | HANDSHAKE        |
//   | 1        | DATA             |
//   | 2        | ACK              |
//   | 3        | HAVE             |
//   | 4        | INTEGRITY        |
//   | 5        | PEX_RESv4        |
//   | 6        | PEX_REQ          |
//   | 7        | SIGNED_INTEGRITY |
//   | 8        | REQUEST          |
//   | 9        | CANCEL           |
//   | 10       | CHOKE            |
//   | 11       | UNCHOKE          |
//   | 12       | PEX_RESv6        |
//   | 13       | PEX_REScert      |
//   | 14-254   | Unassigned       |
//   | 255      | Reserved         |
//   +----------+------------------+
const (
	handshake Opcode = 13 // weird number so it's easier to notice in debug info
)

// MsgData holds the data payload of a message
type MsgData interface{}

// Handshake holds a handshake message data payload
type Handshake struct {
	C ChanID
	S SwarmID
	// TODO: swarm SwarmMetadata
	// TODO: peer capabilities
}

// Msg holds a protocol message
type Msg struct {
	Op   Opcode
	Data MsgData
}

// msgAux is an auxiliary struct that looks like Msg except it has
// a []byte to store the incoming gob for MsgData
// (see marshal/unmarshal functions on Msg)
type msgAux struct {
	Op   Opcode
	Data []byte
}

// UnmarshalJSON handles the deserializing of a message.
//
// We can't get away with off-the-shelf JSON, because
// we're using an interface type for MsgData, which causes problems
// on the decode side.
func (m *Msg) UnmarshalJSON(b []byte) error {
	// Use builtin json to unmarshall into aux
	var aux msgAux
	json0.Unmarshal(b, &aux)

	// The Op field in aux is already what we want for m.Op
	m.Op = aux.Op

	// decode the gob in aux.Data and put it in m.Data
	dec := gob.NewDecoder(bytes.NewBuffer(aux.Data))
	switch aux.Op {
	case handshake:
		var h Handshake
		err := dec.Decode(&h)
		if err != nil {
			return errors.New("failed to decode handshake")
		}
		m.Data = h
	default:
		return errors.New("failed to decode message data")
	}

	return nil
}

// MarshalJSON handles the serializing of a message.
//
// See note above UnmarshalJSON for the reason for the custom MarshalJSON
func (m Msg) MarshalJSON() ([]byte, error) {
	// Encode m.Data into a gob
	var b bytes.Buffer
	enc := gob.NewEncoder(&b)
	switch m.Data.(type) {
	case Handshake:
		gob.Register(Handshake{})
		err := enc.Encode(m.Data.(Handshake))
		if err != nil {
			return nil, fmt.Errorf("Failed to marshal Handshake: %v", err)
		}
	default:
		return nil, errors.New("failed to marshal message data")
	}

	// build an aux and marshal using built-in json
	aux := msgAux{Op: m.Op, Data: b.Bytes()}
	return json0.Marshal(aux)
}

// Datagram holds a protocol datagram
type Datagram struct {
	ChanID ChanID
	Msgs   []Msg
}

// ProtocolState is a per-channel state local to a peer
type ProtocolState uint

const (
	begin         ProtocolState = 0
	waitHandshake ProtocolState = 1 // waiting for ack of first handshake
	ready         ProtocolState = 2
)

// Chan holds the current state of a channel
type Chan struct {
	//ours   ChanID // receiving channel id (unique)
	//theirs ChanID // sending channel id
	sw     SwarmID        // the swarm that this channel is communicating for
	theirs ChanID         // remote id to attach to outgoing datagrams on this channel
	state  ProtocolState  // current state of the protocol on this channel
	stream *WrappedStream // stream to use for sending and receiving datagrams on this channel
	remote peer.ID        // peer.ID of the remote peer
}

type swarm struct {
	// chans is a peer ID -> channel ID map for this swarm
	// it does not include this peer, because this peer does not have a local channel ID
	chans map[peer.ID]ChanID
	// TODO: other swarm metadata stored here
}

// Peer is currently just a couple of things related to a peer (as defined in the RFC)
type Peer struct {
	// libp2p Host interface
	h host.Host

	// all of this peer's channels, indexed by a local ChanID
	chans map[ChanID]*Chan

	// all of this peer's swarms, indexed by a global? SwarmID
	swarms map[SwarmID]*swarm

	// all of this peer's streams, indexed by a global? peer.ID
	streams map[peer.ID]*WrappedStream
}

func newSwarm() *swarm {
	chans := make(map[peer.ID]ChanID)
	return &swarm{chans: chans}
}

// NewPeer makes and initializes a new peer
func NewPeer(port int) *Peer {

	// initially, there are no locally known swarms
	swarms := make(map[SwarmID](*swarm))

	chans := make(map[ChanID](*Chan))
	// Special channel 0 is the reserved channel for incoming starting handshakes
	chans[0] = &Chan{}
	chans[0].state = begin

	// initially, no streams
	streams := make(map[peer.ID](*WrappedStream))

	// Create a basic host to implement the libp2p Host interface
	h := NewBasicHost(port)

	p := Peer{chans: chans, h: h, swarms: swarms, streams: streams}

	// setup stream handler so we can immediately start receiving
	p.setupStreamHandler()

	return &p
}

func (p *Peer) id() peer.ID {
	return p.h.ID()
}

func (p *Peer) setupStreamHandler() {
	log.Println("setting stream handler")
	p.h.SetStreamHandler(proto, func(s inet.Stream) {

		remote := s.Conn().RemotePeer()
		log.Printf("%s received a stream from %s", p.h.ID(), remote)
		//remote := stream.Conn().RemotePeer()
		//ws, ok := p.streams[remote]
		// if !ok {
		// 	// this is a new stream we don't know about?
		// 	log.Printf("%s: Received a stream from %s", p.h.ID(), remote)
		// 	ws = WrapStream(stream)
		// 	p.streams[remote] = ws
		// }
		defer s.Close()
		ws := WrapStream(s)
		// buf := make([]byte, 1)
		// n, err2 := ws.r.Read(buf)
		// log.Printf("%v read %v bytes, err=%v", p.id(), n, err2)
		err := p.HandleStream(ws)
		log.Println("handled stream")
		if err != nil {
			log.Fatal(err)
		}
	})
}

// HandleStream handles an incoming stream
// TODO: not sure how this works wrt multiple incoming datagrams
func (p *Peer) HandleStream(ws *WrappedStream) error {
	log.Printf("%v handling stream", p.id())
	d, err := p.receiveDatagram(ws)
	log.Printf("%v recvd Datagram", p.id())
	if err != nil {
		return err
	}
	return p.handleDatagram(d, ws)
}

// receiveDatagram reads and decodes a datagram from the stream
func (p *Peer) receiveDatagram(ws *WrappedStream) (*Datagram, error) {
	log.Printf("%v receiveDatagram", p.id())
	if ws == nil {
		return nil, fmt.Errorf("%v receiveDatagram on nil *WrappedStream", p.h.ID())
	}
	var d Datagram
	err := ws.dec.Decode(&d)
	log.Printf("decoded datagram %v\n", d)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// sendDatagram encodes and writes a datagram to the channel
func (p *Peer) sendDatagram(d Datagram, c ChanID) error {
	_, ok := p.chans[c]
	if !ok {
		return errors.New("could not find channel")
	}
	remote := p.chans[c].remote
	s, err := p.h.NewStream(context.Background(), remote, proto)
	if err != nil {
		return fmt.Errorf("sendDatagram: (chan %v) NewStream to %v: %v", c, remote, err)
	}

	ws := WrapStream(s)

	// cstruct, ok := p.chans[c]
	// if !ok {
	// 	return errors.New(fmt.Sprintf("bad channel id %v", c))
	// }
	// ws := cstruct.stream
	// if ws == nil {
	// 	return errors.New(fmt.Sprintf("channel %v has no stream set", c))
	// }
	log.Printf("%v sending datagram %v\n", p.id(), d)
	err2 := ws.enc.Encode(d)
	if err2 != nil {
		return fmt.Errorf("send datagram encode error %v", err2)
	}
	// Because output is buffered with bufio, we need to flush!
	err3 := ws.w.Flush()
	log.Printf("%v flushed datagram", p.id())
	if err3 != nil {
		return fmt.Errorf("send datagram flush error: %v", err3)
	}
	return nil
}

func (p *Peer) handleDatagram(d *Datagram, ws *WrappedStream) error {
	log.Printf("%v handling datagram %v\n", p.id(), d)
	if len(d.Msgs) == 0 {
		return errors.New("no messages in datagram")
	}
	for _, msg := range d.Msgs {
		cid := d.ChanID
		// if cid == 0 {
		// 	// special channel 0 for incoming handshake messages
		// 	// set up a channel here, so that all downstream functions
		// 	// can act on channels and not have to worry about streams
		// 	cid = chooseOurID()
		// 	p.addChan(cid, 0, 0, begin, ws.stream.Conn().RemotePeer())
		// }
		_, ok := p.chans[cid]
		if !ok {
			return errors.New("channel not found")
		}
		err := p.handleMsg(cid, msg, ws.stream.Conn().RemotePeer())
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Peer) handleMsg(c ChanID, m Msg, remote peer.ID) error {
	switch m.Op {
	case handshake:
		return p.handleHandshake(c, m, remote)
	default:
		return MsgError{m: m, info: "bad opcode"}
	}
}

func (p *Peer) handleHandshake(cid ChanID, m Msg, remote peer.ID) error {
	log.Printf("%v handling handshake", p.id())
	h, ok := m.Data.(Handshake)
	if !ok {
		return MsgError{c: cid, m: m, info: "could not convert to HANDSHAKE"}
	}

	// cid==0 means this is an incoming starting handshake
	if cid == 0 {
		if h.C < 1 {
			return MsgError{c: cid, m: m, info: "handshake cannot request channel ID 0"}
		}
		// need to create a new channel
		newCID := chooseOurID()
		p.addChan(newCID, h.S, h.C, ready, remote)
		log.Printf("%v moving to ready state", p.id())
		p.sendReplyHandshake(newCID, h.C, h.S)
	} else {
		c := p.chans[cid]
		switch c.state {
		case begin:
			return MsgError{c: cid, m: m, info: "starting handshake must use channel ID 0"}
		case waitHandshake:
			c := p.chans[cid]
			log.Println("in waitHandshake state")
			if h.C == 0 {
				log.Println("received closing handshake")
				p.closeChannel(cid)
			} else {
				c.theirs = h.C
				log.Printf("%v moving to ready state", p.id())
				c.state = ready
			}
		case ready:
			log.Println("in ready state")
			if h.C == 0 {
				log.Println("received closing handshake")
				p.closeChannel(cid)
			} else {
				return MsgError{c: cid, m: m, info: "got non-closing handshake while in ready state"}
			}
		default:
			return MsgError{c: cid, m: m, info: "bad channel state"}
		}
	}
	return nil
}

func (p *Peer) closeChannel(c ChanID) error {
	log.Println("closing channel")
	delete(p.chans, c)
	return nil
}

// addChan adds a channel at the key ours
func (p *Peer) addChan(ours ChanID, sid SwarmID, theirs ChanID, state ProtocolState, remote peer.ID) error {
	log.Printf("addChan ours=%v, sid=%v, theirs=%v, state=%v, remote=%v", ours, sid, theirs, state, remote)

	if ours < 1 {
		return errors.New("cannot setup channel with ours<1")
	}

	// add the channel to the peer-level map
	p.chans[ours] = &Chan{sw: sid, theirs: theirs, state: state, remote: remote}

	// add the channel to the swarm-level map
	log.Printf("%v adding channel %v to swarm %v for %v", p.id(), ours, sid, remote)
	sw, ok := p.swarms[sid]
	if !ok {
		return fmt.Errorf("no swarm exists at sid=%v", sid)
	}
	sw.chans[remote] = ours

	return nil
}

func (p *Peer) startHandshake(remote peer.ID, sid SwarmID) error {
	log.Printf("%v starting handshake", p.id())

	ours := chooseOurID()
	// their channel is 0 until they reply with a handshake
	p.addChan(ours, sid, 0, begin, remote)
	p.chans[ours].state = waitHandshake
	return p.sendReqHandshake(ours, sid)
}

func (p *Peer) sendReqHandshake(ours ChanID, sid SwarmID) error {
	log.Printf("%v sending request handshake", p.id())
	return p.sendHandshake(ours, 0, sid)
}

func (p *Peer) sendReplyHandshake(ours ChanID, theirs ChanID, sid SwarmID) error {
	log.Printf("%v sending reply handshake", p.id())
	return p.sendHandshake(ours, theirs, sid)
}

func (p *Peer) sendClosingHandshake(remote peer.ID, sid SwarmID) error {
	// get chanID from peer.ID and SwarmID
	c := p.swarms[sid].chans[remote]

	log.Printf("%v sending closing handshake on sid=%v c=%v to %v", p.id(), sid, c, remote)
	// handshake with c=0 will signal a close handshake
	h := Handshake{C: 0}
	m := Msg{Op: handshake, Data: h}
	d := Datagram{ChanID: p.chans[c].theirs, Msgs: []Msg{m}}
	log.Printf("%v sending datagram for closing handshake", p.id())
	err := p.sendDatagram(d, c)
	if err != nil {
		return fmt.Errorf("sendClosingHandshake: %v", err)
	}
	return p.closeChannel(c)
}

func (p *Peer) sendHandshake(ours ChanID, theirs ChanID, sid SwarmID) error {
	h := Handshake{C: ours, S: sid}
	m := Msg{Op: handshake, Data: h}
	d := Datagram{ChanID: theirs, Msgs: []Msg{m}}
	return p.sendDatagram(d, ours)
}

func chooseOurID() ChanID {
	// TODO
	return 7
}

// WrappedStream wraps a libp2p stream. We encode/decode whenever we
// write/read from a stream, so we can just carry the encoders
// and bufios with us
type WrappedStream struct {
	stream inet.Stream
	enc    multicodec.Encoder
	dec    multicodec.Decoder
	w      *bufio.Writer
	r      *bufio.Reader
}

// WrapStream takes a stream and complements it with r/w bufios and
// decoder/encoder. In order to write raw data to the stream we can use
// wrap.w.Write(). To encode something into it we can wrap.enc.Encode().
// Finally, we should wrap.w.Flush() to actually send the data. Handling
// incoming data works similarly with wrap.r.Read() for raw-reading and
// wrap.dec.Decode() to decode.
func WrapStream(s inet.Stream) *WrappedStream {
	reader := bufio.NewReader(s)
	writer := bufio.NewWriter(s)

	// Note that if these change, then the MarshalJSON/UnmarshalJSON functions for Msg
	// may no longer get called, which may mess up the codec for Msg.Data
	dec := json.Multicodec(false).Decoder(reader)
	enc := json.Multicodec(false).Encoder(writer)
	return &WrappedStream{
		stream: s,
		r:      reader,
		w:      writer,
		enc:    enc,
		dec:    dec,
	}
}

// NewBasicHost makes and initializes a basic host
func NewBasicHost(port int) host.Host {
	// Ignoring most errors for brevity
	// See echo example for more details and better implementation
	priv, pub, _ := crypto.GenerateKeyPair(crypto.RSA, 2048)
	pid, _ := peer.IDFromPublicKey(pub)
	listen, _ := ma.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port))
	ps := ps.NewPeerstore()
	ps.AddPrivKey(pid, priv)
	ps.AddPubKey(pid, pub)
	n, _ := libp2pswarm.NewNetwork(context.Background(),
		[]ma.Multiaddr{listen}, pid, ps, nil)
	return bhost.New(n)
}

// Adds a swarm with a given ID
func (p *Peer) AddSwarm(id SwarmID) {
	p.swarms[id] = newSwarm()
}

// Connect creates a stream from p to the peer at id and sets a stream handler
// func (p *Peer) Connect(id peer.ID) (*WrappedStream, error) {
// 	log.Printf("%s: Connecting to %s", p.h.ID(), id)
// 	stream, err := p.h.NewStream(context.Background(), id, proto)
// 	if err != nil {
// 		return nil, err
// 	}

// 	ws := WrapStream(stream)

// 	p.streams[id] = ws

// 	return ws, nil
// }

// Disconnect closes the stream that p is using to connect to the peer at id
// func (p *Peer) Disconnect(id peer.ID) error {
// 	ws, ok := p.streams[id]
// 	if ok {
// 		ws.stream.Close()
// 		return nil
// 	}
// 	return errors.New("disconnect error, no stream to close")
// }
