package transport

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
)

// KeySize is the size of one directional channel key (ChaCha20-Poly1305).
const KeySize = 32

// HKDF info labels of the two directions.
const (
	infoWorkerToHub = "minion-hs-v1 key worker-to-hub"
	infoHubToWorker = "minion-hs-v1 key hub-to-worker"
)

// channelKeys are the two directional keys of a channel.
type channelKeys struct {
	workerToHub [KeySize]byte
	hubToWorker [KeySize]byte
}

// deriveKeys derives the directional keys (AD-6 step 6): HKDF-SHA256 with
// input keying material X25519(eph_w, eph_h) followed by the CPace ISK
// (pair_code mode only; nil otherwise), salt = transcript_hash and one info
// label per direction.
func deriveKeys(dh, isk, transcriptHash []byte) (channelKeys, error) {
	var k channelKeys
	if len(dh) == 0 || len(transcriptHash) == 0 {
		return k, errors.New("transport: empty key material")
	}
	ikm := make([]byte, 0, len(dh)+len(isk))
	ikm = append(ikm, dh...)
	ikm = append(ikm, isk...)
	prk, err := hkdf.Extract(sha256.New, ikm, transcriptHash)
	if err != nil {
		return k, err
	}
	for _, d := range []struct {
		info string
		out  *[KeySize]byte
	}{{infoWorkerToHub, &k.workerToHub}, {infoHubToWorker, &k.hubToWorker}} {
		b, err := hkdf.Expand(sha256.New, prk, d.info, KeySize)
		if err != nil {
			return channelKeys{}, err
		}
		copy(d.out[:], b)
	}
	return k, nil
}

func (k *channelKeys) wipe() {
	clear(k.workerToHub[:])
	clear(k.hubToWorker[:])
}
