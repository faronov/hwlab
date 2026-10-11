package swd

// Request exposes the request encoder to tests.
func Request(ap, read bool, reg byte) byte { return request(ap, read, reg) }
