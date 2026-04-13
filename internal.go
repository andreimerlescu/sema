package sema

func validateN(n int) error {
	if n < 1 {
		return ErrInvalidN{Value: n}
	}
	return nil
}

func newChannel(c int) chan struct{} {
	return make(chan struct{}, c)
}
