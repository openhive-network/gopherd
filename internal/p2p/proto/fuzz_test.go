package proto

import (
	"bytes"
	"testing"
)

// message is what every p2p message type implements.
type message interface {
	Marshal() ([]byte, error)
	Unmarshal([]byte) error
}

// allMessages returns a fresh value of every message type the node decodes
// from a peer. Keep it in step with the Unmarshal methods in this package.
func allMessages() []func() message {
	return []func() message{
		func() message { return &LegacyHelloMessage{} },
		func() message { return &HelloMessage{} },
		func() message { return &ConnectionRejectedMessage{} },
		func() message { return &AddressMessage{} },
		func() message { return &ClosingConnectionMessage{} },
		func() message { return &CurrentTimeRequestMessage{} },
		func() message { return &CurrentTimeReplyMessage{} },
		func() message { return &ItemIDsInventoryMessage{} },
		func() message { return &BlockchainItemIDsInventoryMessage{} },
		func() message { return &FetchBlockchainItemIDsMessage{} },
		func() message { return &FetchItemsMessage{} },
		func() message { return &ItemNotAvailableMessage{} },
		func() message { return &BlockMessage{} },
		func() message { return &CompressedBlockMessage{} },
		func() message { return &TrxMessage{} },
	}
}

// FuzzDecodeMessage feeds arbitrary bytes to the envelope decoder, the first
// thing to see a peer's traffic. It must not panic, must refuse bodies over
// MaxMessageSize, and an envelope it accepts must re-encode to the bytes it
// consumed apart from the padding, which the decoder ignores and the encoder
// zeroes.
func FuzzDecodeMessage(f *testing.F) {
	if b, err := EncodeMessage(5000, []byte{1, 2, 3}); err == nil {
		f.Add(b)
	}
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, src []byte) {
		r := bytes.NewReader(src)
		typ, body, err := DecodeMessage(r)
		if err != nil {
			return
		}
		if len(body) > MaxMessageSize {
			t.Fatalf("accepted a %d-byte body", len(body))
		}
		consumed := src[:len(src)-r.Len()]
		enc, err := EncodeMessage(typ, body)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		used := HeaderSize + len(body)
		if len(enc) != len(consumed) || !bytes.Equal(enc[:used], consumed[:used]) {
			t.Fatalf("envelope re-encodes differently\n in: %x\nout: %x", consumed, enc)
		}
	})
}

// FuzzMessageUnmarshal decodes a message body as the type chosen by the first
// byte. Every type's Unmarshal sees peer-controlled bytes, so none may panic,
// and what one accepts must be stable under Marshal/Unmarshal.
func FuzzMessageUnmarshal(f *testing.F) {
	msgs := allMessages()
	for i, mk := range msgs {
		f.Add(append([]byte{byte(i)}, make([]byte, 16)...))
		if b, err := mk().Marshal(); err == nil {
			f.Add(append([]byte{byte(i)}, b...))
		}
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) == 0 {
			return
		}
		mk := msgs[int(src[0])%len(msgs)]
		m := mk()
		if err := m.Unmarshal(src[1:]); err != nil {
			return
		}
		enc, err := m.Marshal()
		if err != nil {
			// Accepting something it cannot write back is allowed only if
			// the value is unrepresentable; report it so it is looked at.
			t.Fatalf("%T: accepted %x but Marshal fails: %v", m, src[1:], err)
		}
		m2 := mk()
		if err := m2.Unmarshal(enc); err != nil {
			t.Fatalf("%T: re-encoding does not decode: %v\nenc: %x", m, err, enc)
		}
		enc2, err := m2.Marshal()
		if err != nil || !bytes.Equal(enc2, enc) {
			t.Fatalf("%T: encoding is not stable (err=%v)\nenc:  %x\nenc2: %x", m, err, enc, enc2)
		}
	})
}
