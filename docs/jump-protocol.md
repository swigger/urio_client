# Jump transport v1

TCP stream protocol. PSKs must be randomly generated (at least 256 bits of entropy).
This is a private relay transport, not an impersonation or TLS camouflage protocol.

1. Client sends `URJ1` (4 bytes) and a fresh 32-byte cryptographic random nonce.
2. Server sends its fresh 32-byte nonce and HMAC-SHA256(PSK, `server` || transcript).
3. Client verifies the MAC and sends HMAC-SHA256(PSK, `client` || transcript).
4. Server verifies before opening any destination connection.

The transcript is the complete 36-byte client hello followed by the 32-byte
server nonce. HKDF-SHA256 uses the PSK as IKM, the transcript as salt, and
`urio jump v1 directional AES-256-GCM keys` as info, producing 64 bytes.
The first 32 bytes form the client-to-server key; the remaining bytes form the
server-to-client key. The fresh server challenge prevents replay of an old
client proof. Both MACs bind the protocol version and both nonces.

Each direction has a separate AES-256-GCM key and starts its 64-bit sequence
number at zero. The 96-bit GCM nonce is four zero bytes followed by the big-endian
sequence number. Sequence exhaustion aborts the connection. Each frame is:

```
uint32be ciphertext_length
AES-GCM( type_byte || payload, AAD = ciphertext_length )
```

The ciphertext includes the 16-byte authentication tag. Length is bounded to
17..32785 before allocation. Data frames have type 0 and 1..32768 payload bytes.
Type 1 has no payload and authenticates half-close. A raw TCP EOF without that
frame is truncation, not a clean EOF. Each frame increments the implicit
sequence number, rejecting duplicates, reordering and modification. No data
is released before tag verification. A failed tag closes the connection.

The first encrypted client bytes are uint16be length plus a UTF-8 `host:port`
destination (maximum 512 bytes). Server resolves and checks the resulting IPs,
then dials an approved IP directly to avoid DNS rebinding. It replies with one
encrypted status byte: 0 success, 1 refusal/dial failure. Remaining bytes are
the relayed stream. Half-close is propagated so response-after-EOF protocols work.

Handshake timeout is 10 seconds; destination setup is bounded to 30 seconds.
The server caps simultaneous connections, including unauthenticated handshakes.
Possession of the PSK grants relay access. PSK compromise allows decryption of
recorded jump traffic (no forward secrecy); nested ur.io TLS remains independent.
Rotate the PSK when replacing a server or revoking access.

Tests cover duplex traffic, large multi-frame transfer, half-close, wrong PSKs,
tampering, replay, permitted destinations and cancellation of active relays.
