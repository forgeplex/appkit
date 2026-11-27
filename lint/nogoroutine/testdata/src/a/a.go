package a

func work() {}

func f() {
	go work()   // want `禁止直接使用裸 go 语句`
	go func() { // want `禁止直接使用裸 go 语句`
	}()

	//appkit:ignore nogoroutine -- 有界 fan-out 由 errgroup 回收
	go work()
}
