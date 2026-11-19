package uart

func setLinesSeq(p Port, dtr, rts bool) error {
	if err := p.SetDTR(dtr); err != nil {
		return err
	}
	return p.SetRTS(rts)
}
