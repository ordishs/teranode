package transport

import (
	"encoding/binary"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// protocol.cpp:220-237 CMessageHeader: a payload above uint32 max is framed
// as command "extmsg", length 0xffffffff, ZERO checksum, followed by
// CExtendedMessageHeader — the real command (12 bytes) and a 64-bit length.
const (
	extHeaderSize   = wire.MessageHeaderSize + wire.CommandSize + 8
	extLengthMarker = uint32(0xffffffff)

	// ExtendedPayloadVersion is version.h:51 EXTENDED_PAYLOAD_VERSION: the
	// minimum protocol version a peer must announce before it may send or
	// receive the extended message header.
	ExtendedPayloadVersion = uint32(70016)
)

var (
	// ErrExtendedVersion is returned when a peer below ExtendedPayloadVersion
	// sends an extended message header.
	ErrExtendedVersion = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: extended message from a peer below protocol version 70016")

	// ErrExtendedNonBlock is returned when a peer frames a non-block message
	// with the extended header. This is a DEVIATION: protocol.cpp:262-266
	// keys the extended header on the command alone and would read such a
	// frame. protocol.cpp:220-237 only WRITES one for a payload above uint32
	// max, and any such payload already exceeds our advertised
	// maxRecvPayloadLength (net_processing.cpp:3306), so no non-block message
	// ever legitimately arrives extended and refusing it costs no honest peer.
	ErrExtendedNonBlock = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: extended header on a non-block message")
)

// extBlockFrameHeader is the 44-byte extended header for a block payload.
func extBlockFrameHeader(magic wire.BitcoinNet, length uint64) []byte {
	hdr := make([]byte, extHeaderSize)

	binary.LittleEndian.PutUint32(hdr[0:4], uint32(magic))
	copy(hdr[4:4+wire.CommandSize], wire.CmdExtMsg)
	binary.LittleEndian.PutUint32(hdr[16:20], extLengthMarker)
	// checksum hdr[20:24] stays zero (protocol.cpp:226)
	copy(hdr[24:24+wire.CommandSize], wire.CmdBlock)
	binary.LittleEndian.PutUint64(hdr[36:44], length)

	return hdr
}
