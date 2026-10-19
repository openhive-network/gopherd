// Package transport implements the byte-stream layer of hived's P2P
// protocol: TCP + ECDHE-secured AES-256-CBC framing (graphene's
// stcp_socket) + the message header (graphene::net::message_header).
//
// One *Conn = one peer connection. After Dial or AcceptConn returns, the
// caller talks via Send / Receive in (typeTag, body) pairs; all the
// padding, encryption, and length framing is hidden.
//
// Wire format (all multi-byte integers little-endian):
//
//  1. 33-byte raw compressed secp256k1 pubkey (each side, simultaneously)
//  2. Forever after: AES-256-CBC ciphertext of:
//     [ size:u32 ][ msg_type:u32 ][ body...padded to 16-byte boundary ]
//
// AES key + IV derivation:
//
//	shared = sha512( ECDH(local_priv, remote_pub).X )            // 64 B
//	key    = sha256(shared)                                       // 32 B
//	iv     = halves-swapped(city_hash_crc_128(shared))            // 16 B
//
// (See internal/crypto/cityhash.go for why iv is byte-half-swapped vs
// the raw uint128.)
package transport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"gopherd/internal/crypto"
	"gopherd/internal/p2p/proto"
)

// AESBlockSize is the on-wire alignment unit (16 bytes for AES-128/256).
const AESBlockSize = aes.BlockSize

// HandshakeTimeout caps how long the handshake (pubkey exchange + first
// AES setup) can take. Mirrors GRAPHENE_NET_PEER_HANDSHAKE_INACTIVITY_TIMEOUT
// (5 s) loosely; we keep ours a bit larger to tolerate slow loopback in CI.
var HandshakeTimeout = 10 * time.Second

// Conn is one stcp-secured peer connection.
//
// Read and write are NOT concurrent-safe with themselves, but sendMu
// serializes Send so multiple goroutines can call it without corrupting
// the AES stream. Receive must be called from a single reader goroutine.
type Conn struct {
	raw       net.Conn
	sharedKey crypto.SharedSecret512
	encrypter cipher.BlockMode
	decrypter cipher.BlockMode
	sendMu    sync.Mutex
	recvBuf   []byte // scratch for read alignment
	closeOnce sync.Once
	remotePub crypto.PublicKey
	localPub  crypto.PublicKey
	// localPriv is the ephemeral secp256k1 private key we generated for
	// this connection's stcp handshake. Hived's hello_message handler
	// requires us to sign sha256(shared_secret_64) with this key and
	// echo our pubkey back; the signature lets the peer recover the
	// pubkey and prove we both completed the same ECDHE.
	localPriv *crypto.PrivateKey
}

// performHandshake exchanges 33-byte pubkeys with the remote and
// initializes the two AES streams.
//
// Both sides do the symmetric write-then-read; this works because the
// kernel buffers the 33 bytes from the peer until we read them.
func performHandshake(raw net.Conn) (*Conn, error) {
	priv, err := crypto.GenerateEphemeralKey()
	if err != nil {
		return nil, fmt.Errorf("transport: generate ephemeral key: %w", err)
	}
	localPub := priv.PublicKey()

	// Apply a deadline that covers both the write and the read.
	deadline := time.Now().Add(HandshakeTimeout)
	if err := raw.SetDeadline(deadline); err != nil {
		// Some net.Conns (e.g. net.Pipe) don't support SetDeadline;
		// continue without it.
	}
	defer raw.SetDeadline(time.Time{})

	// Send our pubkey.
	if _, err := raw.Write(localPub[:]); err != nil {
		return nil, fmt.Errorf("transport: write local pubkey: %w", err)
	}
	// Read the remote's pubkey.
	var remoteBuf [crypto.PublicKeySize]byte
	if _, err := io.ReadFull(raw, remoteBuf[:]); err != nil {
		return nil, fmt.Errorf("transport: read remote pubkey: %w", err)
	}
	remotePub, err := crypto.PublicKeyFromBytes(remoteBuf[:])
	if err != nil {
		return nil, fmt.Errorf("transport: parse remote pubkey: %w", err)
	}

	shared, err := crypto.SharedSecret(priv, remotePub)
	if err != nil {
		return nil, fmt.Errorf("transport: derive shared secret: %w", err)
	}

	keyHash := sha256.Sum256(shared[:])
	ivBytes := crypto.CityHashIVBytes(shared[:])

	block, err := aes.NewCipher(keyHash[:])
	if err != nil {
		return nil, fmt.Errorf("transport: aes NewCipher: %w", err)
	}
	// hived runs two distinct AES contexts (_send_aes and _recv_aes),
	// both seeded with the same key+iv. With matching key+iv and CBC
	// mode running in lockstep on each direction, the encrypt context
	// on side A produces the same intermediate state as the decrypt
	// context on side B (and vice versa).
	enc := cipher.NewCBCEncrypter(block, ivBytes[:])
	dec := cipher.NewCBCDecrypter(block, ivBytes[:])

	return &Conn{
		raw:       raw,
		sharedKey: shared,
		encrypter: enc,
		decrypter: dec,
		remotePub: remotePub,
		localPub:  localPub,
		localPriv: priv,
	}, nil
}

// Send frames (typeTag, body), pads to 16 bytes, encrypts, writes.
// Concurrent Send calls are serialized.
func (c *Conn) Send(typeTag proto.MessageType, body []byte) error {
	frame, err := proto.EncodeMessage(typeTag, body)
	if err != nil {
		return err
	}
	// Pad to 16-byte boundary with zeros (matches send_message in
	// message_oriented_connection.cpp, which memsets the trailing
	// padding to 0).
	padded := frame
	if rem := len(frame) % AESBlockSize; rem != 0 {
		padded = make([]byte, len(frame)+AESBlockSize-rem)
		copy(padded, frame)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	ciphertext := make([]byte, len(padded))
	c.encrypter.CryptBlocks(ciphertext, padded)
	// A peer that stops reading would otherwise block this write, and every
	// sender queued behind sendMu, forever: the heartbeat that should drop
	// it is itself stuck in Send. Net.Conns without deadlines (net.Pipe in
	// tests) just ignore this.
	_ = c.raw.SetWriteDeadline(time.Now().Add(sendTimeout))
	if _, err := c.raw.Write(ciphertext); err != nil {
		// The CBC stream is out of step after a partial write, so the
		// connection is unusable; closing it ends the read loop too, which
		// tears the peer down.
		_ = c.raw.Close()
		return err
	}
	return nil
}

// sendTimeout bounds one message write. The largest message is a block of
// at most 2 MB, which even a slow peer takes far less than this to accept.
const sendTimeout = 2 * time.Minute

// Receive reads + decrypts one message. Returns (msgType, body, err).
//
// Mirrors message_oriented_connection_impl::read_loop: read one
// 16-byte aligned block to recover the header (8 bytes + first 8 bytes
// of body), determine total size, then read the rest.
func (c *Conn) Receive() (proto.MessageType, []byte, error) {
	const firstBlock = AESBlockSize
	if cap(c.recvBuf) < firstBlock {
		c.recvBuf = make([]byte, firstBlock)
	}
	cipherFirst := c.recvBuf[:firstBlock]
	if _, err := io.ReadFull(c.raw, cipherFirst); err != nil {
		return 0, nil, err
	}
	var plainFirst [firstBlock]byte
	c.decrypter.CryptBlocks(plainFirst[:], cipherFirst)

	size := uint32(plainFirst[0]) | uint32(plainFirst[1])<<8 |
		uint32(plainFirst[2])<<16 | uint32(plainFirst[3])<<24
	typeTag := proto.MessageType(uint32(plainFirst[4]) | uint32(plainFirst[5])<<8 |
		uint32(plainFirst[6])<<16 | uint32(plainFirst[7])<<24)

	if size > proto.MaxMessageSize {
		return 0, nil, proto.ErrMessageTooLarge
	}

	// First 8 bytes of body live in the tail of the first block. The body is
	// the ONE allocation per message: it is handed to the caller, who keeps
	// it (block payloads live until the block is applied and logged). The
	// ciphertext goes through the connection's reusable scratch buffer and
	// is decrypted straight into the body, which has room for the last
	// block's padding. The previous three fresh buffers per message
	// (ciphertext, plaintext, body) were a third of all bytes allocated
	// during a sync.
	const leftover = firstBlock - proto.HeaderSize
	remaining := 0
	paddedRemaining := 0
	if size > leftover {
		remaining = int(size) - leftover
		paddedRemaining = ((remaining + AESBlockSize - 1) / AESBlockSize) * AESBlockSize
	}
	bodyBuf := make([]byte, leftover+paddedRemaining)
	copy(bodyBuf, plainFirst[proto.HeaderSize:])
	if paddedRemaining > 0 {
		if cap(c.recvBuf) < paddedRemaining {
			c.recvBuf = make([]byte, paddedRemaining)
		}
		cipherTail := c.recvBuf[:paddedRemaining]
		if _, err := io.ReadFull(c.raw, cipherTail); err != nil {
			return 0, nil, err
		}
		c.decrypter.CryptBlocks(bodyBuf[leftover:], cipherTail)
	}
	// (Trailing padding bytes inside the last block are the 0-fill the
	// sender added; the returned slice stops before them.)
	return typeTag, bodyBuf[:size], nil
}

// Close shuts down the underlying TCP socket. Idempotent.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.raw.Close()
	})
	return err
}

// LocalPublicKey returns the ephemeral pubkey we presented during the handshake.
func (c *Conn) LocalPublicKey() crypto.PublicKey { return c.localPub }

// RemotePublicKey returns the ephemeral pubkey the peer presented.
func (c *Conn) RemotePublicKey() crypto.PublicKey { return c.remotePub }

// SharedSecret returns the SHA-512 of the ECDH X-coordinate. Used by the
// node layer to sign / verify the `signed_shared_secret` field in
// hello_message.
func (c *Conn) SharedSecret() crypto.SharedSecret512 { return c.sharedKey }

// LocalPrivateKey returns the ephemeral secp256k1 private key generated
// for this connection. The hello-message builder signs sha256(shared_secret)
// with it so hived can recover the pubkey from the signature and prove
// we own LocalPublicKey + completed the same ECDHE.
func (c *Conn) LocalPrivateKey() *crypto.PrivateKey { return c.localPriv }

// RemoteAddr returns the underlying TCP RemoteAddr.
func (c *Conn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// LocalAddr returns the underlying TCP LocalAddr.
func (c *Conn) LocalAddr() net.Addr { return c.raw.LocalAddr() }

// SetDeadline sets read/write deadlines on the underlying socket.
// Some transports (e.g. net.Pipe) silently no-op.
func (c *Conn) SetDeadline(t time.Time) error { return c.raw.SetDeadline(t) }
