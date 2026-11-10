package transport

import (
	"crypto/sha256"
	"encoding/binary"
)

// transcriptLabel is the domain-separation prefix of the transcript hash.
const transcriptLabel = "minion-hs-v1"

// TranscriptHashSize is the size of the transcript hash in bytes.
const TranscriptHashSize = sha256.Size

// TranscriptHash returns the AD-6 transcript hash:
//
//	SHA-256("minion-hs-v1" || len(wh) || wh || len(hh) || hh)
//
// where wh and hh are the raw bytes of the Worker hello and the Hub hello
// exactly as they travelled on the wire (the JSON frame bodies, not a
// re-encoding) and len is a 4-byte big-endian length. Both sides hash the
// bytes they sent or received, so any change of a hello byte on the way
// gives the two sides different hashes and every later signature,
// confirmation and key differs. The Hub's signature is not part of the
// transcript: it travels in a separate frame.
func TranscriptHash(workerHello, hubHello []byte) [TranscriptHashSize]byte {
	h := sha256.New()
	h.Write([]byte(transcriptLabel))
	for _, m := range [][]byte{workerHello, hubHello} {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(m)))
		h.Write(l[:])
		h.Write(m)
	}
	var out [TranscriptHashSize]byte
	copy(out[:], h.Sum(nil))
	return out
}

// pakeConfirmData is the data of a pake-confirm signature: the transcript
// hash followed by the CPace confirmation tag of the signer (amendment A-1).
func pakeConfirmData(transcriptHash, tag []byte) []byte {
	d := make([]byte, 0, len(transcriptHash)+len(tag))
	d = append(d, transcriptHash...)
	return append(d, tag...)
}
