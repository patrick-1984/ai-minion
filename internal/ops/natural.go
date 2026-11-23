package ops

import "strings"

// NaturalCompare orders names the way people read them: runs of digits compare
// by numeric value, so "wb-2" sorts before "wb-10". Other characters compare
// case-insensitively, and ties fall back to plain byte order, so the result is
// a total order.
func NaturalCompare(a, b string) int {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(la) && j < len(lb) {
		if isDigit(la[i]) && isDigit(lb[j]) {
			si := i
			for i < len(la) && isDigit(la[i]) {
				i++
			}
			sj := j
			for j < len(lb) && isDigit(lb[j]) {
				j++
			}
			na, nb := strings.TrimLeft(la[si:i], "0"), strings.TrimLeft(lb[sj:j], "0")
			if len(na) != len(nb) {
				return cmpInt(len(na), len(nb))
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			continue
		}
		if la[i] != lb[j] {
			return cmpInt(int(la[i]), int(lb[j]))
		}
		i++
		j++
	}
	if c := cmpInt(len(la)-i, len(lb)-j); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
