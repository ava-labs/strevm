package tree

func last[E any, S ~[]E](s S) (E, bool) {
	if len(s) == 0 {
		var zero E
		return zero, false
	}
	return s[len(s)-1], true
}
