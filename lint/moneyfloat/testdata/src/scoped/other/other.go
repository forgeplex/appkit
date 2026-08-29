// scope 未匹配本包：即使出现浮点也不报告。
package other

type Metric struct {
	Value float64
}

func Avg(xs []float64) float64 {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}
