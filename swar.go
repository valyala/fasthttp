package fasthttp

import "encoding/binary"

// The helpers below examine eight bytes at once by treating a uint64 as eight
// byte lanes. Words are loaded little-endian, so byte i of a slice is lane i.
// A borrow from a lower lane can only add a false match above a lane that
// already matched, which keeps the any-lane answers exact.

const (
	// lowBits has bit 0 of every lane set; a byte times lowBits repeats that
	// byte in all lanes.
	lowBits uint64 = 0x0101010101010101
	// highBits has bit 7 of every lane set.
	highBits uint64 = 0x8080808080808080
)

func loadWord(b []byte) uint64 {
	return binary.LittleEndian.Uint64(b)
}

func storeWord(b []byte, w uint64) {
	binary.LittleEndian.PutUint64(b, w)
}

// repeatByte returns a word with c in every lane.
func repeatByte(c byte) uint64 {
	return uint64(c) * lowBits
}

// anyByteBelow reports whether any lane of w is below n. n must not exceed 0x80.
//
// Subtracting n borrows into a lane's high bit exactly when the lane is below
// n; lanes that already had their high bit set are excluded.
func anyByteBelow(w uint64, n byte) bool {
	return (w-repeatByte(n))&^w&highBits != 0
}

// anyByteEqual reports whether any lane of w equals c.
func anyByteEqual(w uint64, c byte) bool {
	return anyByteBelow(w^repeatByte(c), 1)
}

// anyByteIsCTL reports whether any lane of w is an ASCII control character:
// below 0x20, or 0x7f (DEL).
func anyByteIsCTL(w uint64) bool {
	return anyByteBelow(w, 0x20) || anyByteEqual(w, 0x7f)
}

// lowercaseWord returns w with every ASCII uppercase lane lowercased.
//
// With the high bits cleared no lane can carry into its neighbour, so adding
// 0x80-'A' sets a lane's high bit exactly when the lane is at least 'A', and
// adding 0x80-('Z'+1) exactly when it is past 'Z'. Non-ASCII lanes are
// excluded, and the remaining markers move from bit 7 to bit 5, the ASCII
// case bit.
func lowercaseWord(w uint64) uint64 {
	ascii := w &^ highBits
	atLeastA := ascii + (0x80-'A')*lowBits
	pastZ := ascii + (0x80-('Z'+1))*lowBits
	return w | (atLeastA&^pastZ&^w&highBits)>>2
}
