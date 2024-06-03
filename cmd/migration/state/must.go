package main

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func must1[E any](e E, err error) E {
	if err != nil {
		panic(err)
	}
	return e
}

func must2[P1 any, P2 any](p1 P1, p2 P2, err error) (P1, P2) {
	if err != nil {
		panic(err)
	}
	return p1, p2
}
