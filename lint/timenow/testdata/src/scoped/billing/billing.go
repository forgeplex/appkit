package billing

import "time"

func f() time.Time {
	//appkit:ignore timenow -- 仅记录调试耗时
	_ = time.Now()
	return time.Now() // want `禁止在受控业务包中直接调用 time\.Now\(\)`
}
