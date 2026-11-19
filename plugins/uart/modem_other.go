//go:build !linux && !darwin

package uart

// SetModemLines sets DTR and RTS. This build has no single call for both,
// so the lines change one after the other (atomic is false).
func SetModemLines(p Port, dtr, rts bool) (atomic bool, err error) {
	return false, setLinesSeq(p, dtr, rts)
}
