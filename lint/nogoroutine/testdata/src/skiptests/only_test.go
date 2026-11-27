package skiptests

func f() {
	go func() {}()
}
