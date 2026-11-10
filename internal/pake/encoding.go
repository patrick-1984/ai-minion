package pake

// String utilities from draft-irtf-cfrg-cpace-21, Appendix A.

// prependLen returns LEB128(len(data)) || data (A.1.1).
func prependLen(data []byte) []byte {
	out := make([]byte, 0, len(data)+10)
	n := uint64(len(data))
	for {
		if n < 128 {
			out = append(out, byte(n))
		} else {
			out = append(out, byte(n&0x7f)|0x80)
		}
		n >>= 7
		if n == 0 {
			break
		}
	}
	return append(out, data...)
}

// lvCat returns prepend_len(a0) || prepend_len(a1) || ... (A.1.3).
func lvCat(args ...[]byte) []byte {
	var out []byte
	for _, a := range args {
		out = append(out, prependLen(a)...)
	}
	return out
}

// generatorString implements generator_string (8.1, A.2).
func generatorString(dsi, prs, ci, sid []byte, sInBytes int) []byte {
	zpad := max(0, sInBytes-1-len(prependLen(prs))-len(prependLen(dsi)))
	return lvCat(dsi, prs, make([]byte, zpad), ci, sid)
}

// transcriptIR implements transcript_ir for the initiator-responder
// setting (A.3.4).
func transcriptIR(ya, ada, yb, adb []byte) []byte {
	return append(lvCat(ya, ada), lvCat(yb, adb)...)
}
