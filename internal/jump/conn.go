// Package jump implements a PSK-authenticated, encrypted stream transport.
// The protocol is documented in docs/jump-protocol.md.
package jump

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const magic = "URJ1"
const maxFrame = 32 * 1024

var errAuth = errors.New("jump authentication failed")

type Conn struct {
	net.Conn
	r, w              cipher.AEAD
	rmu, wmu          sync.Mutex
	rseq, wseq        uint64
	pending           []byte
	readEOF, writeEOF bool
}

func mac(psk []byte, label string, data []byte) []byte {
	h := hmac.New(sha256.New, psk)
	h.Write([]byte(label))
	h.Write(data)
	return h.Sum(nil)
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func handshake(raw net.Conn, psk []byte, server bool) (*Conn, error) {
	if len(psk) < 32 {
		return nil, fmt.Errorf("PSK too short")
	}
	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, err
	}
	defer raw.SetDeadline(time.Time{})
	hello := make([]byte, 36)
	reply := make([]byte, 64)
	if server {
		if _, err := io.ReadFull(raw, hello); err != nil {
			return nil, err
		}
		if string(hello[:4]) != magic {
			return nil, errAuth
		}
		if _, err := rand.Read(reply[:32]); err != nil {
			return nil, err
		}
		transcript := append(append([]byte{}, hello...), reply[:32]...)
		copy(reply[32:], mac(psk, "server", transcript))
		if err := writeAll(raw, reply); err != nil {
			return nil, err
		}
		proof := make([]byte, 32)
		if _, err := io.ReadFull(raw, proof); err != nil {
			return nil, err
		}
		if !hmac.Equal(proof, mac(psk, "client", transcript)) {
			return nil, errAuth
		}
	} else {
		copy(hello, magic)
		if _, err := rand.Read(hello[4:]); err != nil {
			return nil, err
		}
		if err := writeAll(raw, hello); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(raw, reply); err != nil {
			return nil, err
		}
		transcript := append(append([]byte{}, hello...), reply[:32]...)
		if !hmac.Equal(reply[32:], mac(psk, "server", transcript)) {
			return nil, errAuth
		}
		if err := writeAll(raw, mac(psk, "client", transcript)); err != nil {
			return nil, err
		}
	}
	salt := append(append([]byte{}, hello...), reply[:32]...)
	key, err := hkdf.Key(sha256.New, psk, salt, "urio jump v1 directional AES-256-GCM keys", 64)
	if err != nil {
		return nil, err
	}
	a, _ := aes.NewCipher(key[:32])
	b, _ := aes.NewCipher(key[32:])
	c2s, _ := cipher.NewGCM(a)
	s2c, _ := cipher.NewGCM(b)
	c := &Conn{Conn: raw, r: s2c, w: c2s}
	if server {
		c.r, c.w = c2s, s2c
	}
	return c, nil
}
func Client(raw net.Conn, psk []byte) (*Conn, error) { return handshake(raw, psk, false) }
func Server(raw net.Conn, psk []byte) (*Conn, error) { return handshake(raw, psk, true) }
func nonce(seq uint64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[4:], seq)
	return b
}
func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.pending) == 0 {
		if c.readEOF {
			return 0, io.EOF
		}
		var header [4]byte
		if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		n := binary.BigEndian.Uint32(header[:])
		if n < 17 || n > maxFrame+17 {
			return 0, fmt.Errorf("invalid jump frame length")
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(c.Conn, b); err != nil {
			return 0, err
		}
		if c.rseq == ^uint64(0) {
			return 0, fmt.Errorf("jump sequence exhausted")
		}
		plain, err := c.r.Open(nil, nonce(c.rseq), b, header[:])
		if err != nil {
			c.Conn.Close()
			return 0, errAuth
		}
		c.rseq++
		if plain[0] == 1 && len(plain) == 1 {
			c.readEOF = true
			return 0, io.EOF
		}
		if plain[0] != 0 || len(plain) == 1 {
			return 0, fmt.Errorf("invalid jump frame type")
		}
		c.pending = plain[1:]
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}
func (c *Conn) frame(kind byte, p []byte) error {
	if c.wseq == ^uint64(0) {
		return fmt.Errorf("jump sequence exhausted")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(p)+1+c.w.Overhead()))
	plain := append([]byte{kind}, p...)
	b := c.w.Seal(nil, nonce(c.wseq), plain, header[:])
	c.wseq++
	if err := writeAll(c.Conn, append(header[:], b...)); err != nil {
		c.Conn.Close()
		return err
	}
	return nil
}
func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.writeEOF {
		return 0, net.ErrClosed
	}
	total := 0
	for len(p) > 0 {
		n := min(len(p), maxFrame)
		if err := c.frame(0, p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}
func (c *Conn) CloseWrite() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.writeEOF {
		return nil
	}
	c.writeEOF = true
	return c.frame(1, nil)
}
